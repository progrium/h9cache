// Package pull turns image references into merged image trees: it resolves
// tags, fetches manifests, converts each layer once, merges the layers, and
// records the result in the store.
package pull

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/remote"

	"github.com/progrium/h9reg/cas"
	"github.com/progrium/h9reg/store"
	"github.com/progrium/h9reg/tree"
)

// Image is the stored record of a pulled image, under images/.
type Image struct {
	Manifest string        `json:"manifest"` // the platform manifest's digest
	Tree     string        `json:"tree"`     // merged image tree id
	Root     tree.Entry    `json:"root"`     // attributes of the image root
	Platform string        `json:"platform"`
	Config   v1.ConfigFile `json:"config"`
	Pulled   time.Time     `json:"pulled"`
}

// Layer is the stored record of a converted layer, under layers/.
type Layer struct {
	Tree string     `json:"tree"`
	Root tree.Entry `json:"root"` // the layer root's attributes, Opaque included
}

// Ref is the stored resolution of a tag, under refs/.
type Ref struct {
	Digest   string    `json:"digest"`
	Resolved time.Time `json:"resolved"`
}

// Puller resolves specs to image records, pulling on a miss.
type Puller struct {
	Store    store.Store
	CAS      *cas.CAS
	Platform v1.Platform   // default platform for multi-platform images
	TagTTL   time.Duration // how long a tag resolution is trusted
	Remote   []remote.Option
	Log      *slog.Logger

	// Mkroot looks references up as ghcr.io/OWNER/mkroot-NAME first; see
	// Locate.
	Mkroot bool
	// MkrootRegistry is where mkroot images are looked up; MkrootRegistry
	// (ghcr.io) if empty.
	MkrootRegistry string

	// Instance names this process in pull claims; NewInstanceID if empty.
	Instance string
	// ClaimTTL is how long another instance's pull claim stands without
	// being refreshed; DefaultClaimTTL if zero.
	ClaimTTL time.Duration
	// PollInterval is how often a waiter checks for another instance's
	// pull to finish; 500ms if zero.
	PollInterval time.Duration

	mu       sync.Mutex
	inflight map[string]*call
}

type call struct {
	done chan struct{}
	img  *Image
	err  error
}

// DefaultPlatform is linux on the architecture h9reg was built for.
func DefaultPlatform() v1.Platform {
	return v1.Platform{OS: "linux", Architecture: runtime.GOARCH}
}

func (p *Puller) log() *slog.Logger {
	if p.Log == nil {
		return slog.Default()
	}
	return p.Log
}

func digestKey(prefix string, d v1.Hash) string {
	return prefix + d.Algorithm + "/" + d.Hex
}

func refKey(t name.Tag) string {
	return "refs/" + t.RegistryStr() + "/" + t.RepositoryStr() + "/" + t.TagStr()
}

func platformKey(index v1.Hash, p v1.Platform) string {
	s := strings.ReplaceAll(p.String(), "/", "_")
	return digestKey("platforms/", index) + "/" + s
}

func getJSON(ctx context.Context, st store.Store, key string, v any) (bool, error) {
	b, err := st.Get(ctx, key)
	if errors.Is(err, store.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, json.Unmarshal(b, v)
}

func putJSON(ctx context.Context, st store.Store, key string, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return st.Put(ctx, key, b)
}

// Resolve returns the image a spec names, pulling it if it isn't stored.
func (p *Puller) Resolve(ctx context.Context, spec Spec) (*Image, error) {
	plat := p.Platform
	if spec.Platform != nil {
		plat = *spec.Platform
	}
	top, err := p.resolveRef(ctx, spec.Ref)
	if err != nil {
		return nil, err
	}
	if img, err := p.lookup(ctx, top, plat); img != nil || err != nil {
		return img, err
	}

	key := top.String() + "!" + plat.String()
	p.mu.Lock()
	if p.inflight == nil {
		p.inflight = map[string]*call{}
	}
	if c, ok := p.inflight[key]; ok {
		p.mu.Unlock()
		select {
		case <-c.done:
			return c.img, c.err
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	c := &call{done: make(chan struct{})}
	p.inflight[key] = c
	p.mu.Unlock()

	// The pull continues if this caller gives up, so later attaches find it.
	ref := spec.Ref.Context().Digest(top.String())
	c.img, c.err = p.claimedPull(context.WithoutCancel(ctx), top, plat, func(ctx context.Context) (*Image, error) {
		return p.pull(ctx, ref, top, plat)
	})
	close(c.done)
	p.mu.Lock()
	delete(p.inflight, key)
	p.mu.Unlock()
	return c.img, c.err
}

// resolveRef turns a reference into the digest of its manifest or index.
func (p *Puller) resolveRef(ctx context.Context, ref name.Reference) (v1.Hash, error) {
	if d, ok := ref.(name.Digest); ok {
		return v1.NewHash(d.DigestStr())
	}
	tag := ref.(name.Tag)
	var cached Ref
	found, err := getJSON(ctx, p.Store, refKey(tag), &cached)
	if err != nil {
		return v1.Hash{}, err
	}
	if found && time.Since(cached.Resolved) < p.TagTTL {
		return v1.NewHash(cached.Digest)
	}
	// HEAD returns the digest without counting as a Docker Hub pull.
	desc, err := remote.Head(tag, append(p.Remote, remote.WithContext(ctx))...)
	if err != nil {
		if found {
			p.log().Warn("tag check failed, using last resolution", "ref", tag.String(), "digest", cached.Digest, "err", err)
			return v1.NewHash(cached.Digest)
		}
		return v1.Hash{}, fmt.Errorf("resolve %s: %w", tag, err)
	}
	if !found || cached.Digest != desc.Digest.String() {
		p.log().Info("resolved tag", "ref", tag.String(), "digest", desc.Digest.String())
	}
	err = putJSON(ctx, p.Store, refKey(tag), Ref{Digest: desc.Digest.String(), Resolved: time.Now().UTC()})
	return desc.Digest, err
}

// lookup finds a stored image for a manifest or index digest.
func (p *Puller) lookup(ctx context.Context, top v1.Hash, plat v1.Platform) (*Image, error) {
	var img Image
	if found, err := getJSON(ctx, p.Store, digestKey("images/", top), &img); found || err != nil {
		return &img, err
	}
	var m string
	if found, err := getJSON(ctx, p.Store, platformKey(top, plat), &m); !found || err != nil {
		return nil, err
	}
	h, err := v1.NewHash(m)
	if err != nil {
		return nil, err
	}
	if found, err := getJSON(ctx, p.Store, digestKey("images/", h), &img); !found || err != nil {
		return nil, err
	}
	return &img, nil
}

func (p *Puller) pull(ctx context.Context, ref name.Digest, top v1.Hash, plat v1.Platform) (*Image, error) {
	start := time.Now()
	opts := append(p.Remote, remote.WithContext(ctx))
	desc, err := remote.Get(ref, opts...)
	if err != nil {
		return nil, fmt.Errorf("fetch %s: %w", ref, err)
	}
	var img v1.Image
	var platform *v1.Platform // from the index entry, which carries the variant
	manifest := top
	if desc.MediaType.IsIndex() {
		idx, err := desc.ImageIndex()
		if err != nil {
			return nil, err
		}
		im, err := idx.IndexManifest()
		if err != nil {
			return nil, err
		}
		found := false
		for _, want := range candidatePlatforms(plat) {
			for _, m := range im.Manifests {
				if m.Platform != nil && m.Platform.Satisfies(want) {
					manifest, platform, found = m.Digest, m.Platform, true
					break
				}
			}
			if found {
				break
			}
		}
		if !found {
			return nil, fmt.Errorf("%s: no image for platform %s", ref, plat)
		}
		if err := putJSON(ctx, p.Store, platformKey(top, plat), manifest.String()); err != nil {
			return nil, err
		}
		var rec Image
		if ok, err := getJSON(ctx, p.Store, digestKey("images/", manifest), &rec); ok || err != nil {
			return &rec, err
		}
		img, err = idx.Image(manifest)
		if err != nil {
			return nil, err
		}
	} else {
		img, err = desc.Image()
		if err != nil {
			return nil, err
		}
	}

	cfg, err := img.ConfigFile()
	if err != nil {
		return nil, fmt.Errorf("%s: config: %w", ref, err)
	}
	layers, err := img.Layers()
	if err != nil {
		return nil, err
	}
	merged := tree.NewDir(tree.Entry{Mode: 0o755})
	for i, l := range layers {
		rec, err := p.layer(ctx, l)
		if err != nil {
			return nil, fmt.Errorf("%s: layer %d: %w", ref, i, err)
		}
		lt, err := tree.Load(ctx, p.Store, rec.Tree, rec.Root)
		if err != nil {
			return nil, err
		}
		tree.Merge(merged, lt)
	}
	addMountPoints(merged)
	id, err := tree.Write(ctx, p.Store, merged)
	if err != nil {
		return nil, err
	}
	root := merged.Entry
	root.Tree = id
	if platform == nil {
		platform = cfg.Platform()
	}
	rec := &Image{
		Manifest: manifest.String(),
		Tree:     id,
		Root:     root,
		Platform: platform.String(),
		Config:   *cfg,
		Pulled:   time.Now().UTC(),
	}
	if err := putJSON(ctx, p.Store, digestKey("images/", manifest), rec); err != nil {
		return nil, err
	}
	p.log().Info("pulled image", "ref", ref.String(), "manifest", manifest.String(), "tree", id,
		"layers", len(layers), "took", time.Since(start).Round(time.Millisecond))
	return rec, nil
}

// layer converts one layer, or returns its record if it was converted
// before. Base layers are shared by many images, so this is common.
func (p *Puller) layer(ctx context.Context, l v1.Layer) (*Layer, error) {
	d, err := l.Digest()
	if err != nil {
		return nil, err
	}
	var rec Layer
	if ok, err := getJSON(ctx, p.Store, digestKey("layers/", d), &rec); ok || err != nil {
		return &rec, err
	}
	rc, err := l.Uncompressed()
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	batch := p.CAS.NewBatch()
	n, err := ConvertLayer(ctx, batch, rc)
	if err != nil {
		return nil, err
	}
	if err := batch.Flush(ctx); err != nil {
		return nil, err
	}
	id, err := tree.Write(ctx, p.Store, n)
	if err != nil {
		return nil, err
	}
	rec = Layer{Tree: id, Root: n.Entry}
	rec.Root.Tree = id
	if err := putJSON(ctx, p.Store, digestKey("layers/", d), rec); err != nil {
		return nil, err
	}
	p.log().Debug("converted layer", "digest", d.String(), "tree", id)
	return &rec, nil
}

// candidatePlatforms lists the platforms to look for in an index, best
// first. h9 runs Linux and 9front programs alike, so a Linux platform
// falls back to Plan 9 on the same architecture, for root images such as
// 9front's that have no Linux image.
func candidatePlatforms(plat v1.Platform) []v1.Platform {
	out := []v1.Platform{plat}
	if plat.OS == "linux" {
		p9 := plat
		p9.OS = "plan9"
		out = append(out, p9)
	}
	return out
}

// MountPoints are the directories h9 binds onto when it builds a
// namespace over a root. A 9P root is read-only, so they have to exist in
// the tree: without /mnt/term/cwd h9 can't place the working directory.
var MountPoints = []string{"dev", "env", "fd", "mnt/term/cwd", "net", "proc", "srv", "tmp"}

// addMountPoints adds any of MountPoints the image lacks, as empty
// directories. A name the image uses for something else is left alone.
func addMountPoints(root *tree.Node) {
	for _, mp := range MountPoints {
		n := root
		for _, el := range strings.Split(mp, "/") {
			c, ok := n.Children[el]
			if !ok {
				c = tree.NewDir(tree.Entry{Name: el, Mode: 0o755})
				n.Children[el] = c
			}
			if c.Kind != tree.Dir {
				break
			}
			n = c
		}
	}
}
