package gc

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/registry"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/tarball"

	"github.com/progrium/h9cache/cas"
	"github.com/progrium/h9cache/pull"
	"github.com/progrium/h9cache/store"
	"github.com/progrium/h9cache/store/dir"
	"github.com/progrium/h9cache/tree"
)

func layer(t *testing.T, files map[string]string) v1.Layer {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for name, body := range files {
		tw.WriteHeader(&tar.Header{Name: name, Typeflag: tar.TypeReg, Mode: 0o644, Size: int64(len(body)), ModTime: time.Unix(1700000000, 0)})
		tw.Write([]byte(body))
	}
	tw.Close()
	b := buf.Bytes()
	l, err := tarball.LayerFromOpener(func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(b)), nil })
	if err != nil {
		t.Fatal(err)
	}
	return l
}

type env struct {
	ctx  context.Context
	st   store.Store
	host string
	p    *pull.Puller
}

func setup(t *testing.T) *env {
	srv := httptest.NewServer(registry.New(registry.Logger(log.New(io.Discard, "", 0))))
	t.Cleanup(srv.Close)
	st, _ := dir.New(t.TempDir())
	e := &env{ctx: context.Background(), st: st, host: strings.TrimPrefix(srv.URL, "http://")}
	e.p = e.puller()
	return e
}

func (e *env) puller() *pull.Puller {
	return &pull.Puller{Store: e.st, CAS: cas.New(e.st, 1<<20), Platform: v1.Platform{OS: "linux", Architecture: "amd64"},
		TagTTL: time.Hour, PollInterval: 10 * time.Millisecond}
}

// push makes an image of a shared base layer and one of its own.
func (e *env) push(t *testing.T, tag, own string) {
	base := layer(t, map[string]string{"etc/base": strings.Repeat("base layer\n", 5000)})
	top := layer(t, map[string]string{"app/" + own: strings.Repeat(own+" data\n", 20000)})
	img, _ := mutate.AppendLayers(empty.Image, base, top)
	img, _ = mutate.ConfigFile(img, &v1.ConfigFile{OS: "linux", Architecture: "amd64"})
	ref, _ := name.NewTag(e.host + "/test/" + tag)
	if err := remote.Write(ref, img); err != nil {
		t.Fatal(err)
	}
}

func (e *env) resolve(t *testing.T, p *pull.Puller, tag string) *pull.Image {
	t.Helper()
	spec, err := pull.ParseSpec(e.host + "/test/" + tag)
	if err != nil {
		t.Fatal(err)
	}
	img, err := p.Resolve(e.ctx, spec)
	if err != nil {
		t.Fatal(err)
	}
	return img
}

// read reads every file of an image through c.
func (e *env) read(t *testing.T, c *cas.CAS, img *pull.Image) {
	t.Helper()
	n, err := tree.Load(e.ctx, e.st, img.Tree, img.Root)
	if err != nil {
		t.Fatal(err)
	}
	var walk func(*tree.Node)
	walk = func(n *tree.Node) {
		for _, ch := range n.Children {
			if ch.Kind == tree.Dir {
				walk(ch)
				continue
			}
			buf := make([]byte, ch.Size)
			if _, err := c.ReadAt(e.ctx, ch.Chunks, buf, 0); err != nil && !errors.Is(err, io.EOF) {
				t.Fatalf("%s: %v", ch.Name, err)
			}
		}
	}
	walk(n)
}

// age makes an image look unused for d.
func (e *env) age(t *testing.T, img *pull.Image, d time.Duration) {
	old := *img
	old.Pulled = time.Now().Add(-d)
	b, _ := json.Marshal(old)
	key := "images/" + strings.Replace(img.Manifest, ":", "/", 1)
	if err := e.st.Put(e.ctx, key, b); err != nil {
		t.Fatal(err)
	}
}

func count(t *testing.T, st store.Store, prefix string) int {
	keys, err := st.List(context.Background(), prefix)
	if err != nil {
		t.Fatal(err)
	}
	return len(keys)
}

func TestCollect(t *testing.T) {
	e := setup(t)
	e.push(t, "old:1", "old")
	e.push(t, "new:1", "new")
	oldImg := e.resolve(t, e.p, "old:1")
	newImg := e.resolve(t, e.p, "new:1")
	e.age(t, oldImg, 40*24*time.Hour)
	// An attach within Keep keeps an image however old its pull.
	e.age(t, newImg, 40*24*time.Hour)
	e.p.Touch(e.ctx, newImg)

	// An instance serving reads, with indexes loaded before the collection.
	reader := cas.New(e.st, 0)
	if err := reader.LoadPacks(e.ctx); err != nil {
		t.Fatal(err)
	}
	trees, packs := count(t, e.st, "trees/"), count(t, e.st, "packs/")

	dry, err := Run(e.ctx, e.st, Options{DryRun: true})
	if err != nil {
		t.Fatal(err)
	}
	if dry.ImagesDeleted != 1 || count(t, e.st, "images/") != 2 || count(t, e.st, "trees/") != trees {
		t.Fatalf("dry run: %+v", dry)
	}

	rep, err := Run(e.ctx, e.st, Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Log(rep)
	if rep.ImagesDeleted != 1 || rep.LayersDeleted != 1 || rep.TreesDeleted == 0 || rep.Packs.Deleted != 1 {
		t.Fatalf("collection: %+v", rep)
	}
	if count(t, e.st, "images/") != 1 || count(t, e.st, "layers/") != 2 || count(t, e.st, "packs/") != packs-1 {
		t.Fatal("wrong records left")
	}
	if count(t, e.st, "gc/") != 1 { // the epoch, not the lock
		t.Fatal("lock left behind")
	}
	// What stays is whole, for the old reader and a new one.
	e.read(t, reader, newImg)
	e.read(t, cas.New(e.st, 0), newImg)

	// A puller from before the collection reloads its indexes, so the old
	// image's chunks aren't taken as stored when it's pulled again.
	again := e.resolve(t, e.p, "old:1")
	e.read(t, cas.New(e.st, 0), again)

	// Nothing more to collect.
	rep, err = Run(e.ctx, e.st, Options{})
	if err != nil || rep.ImagesDeleted != 0 || rep.TreesDeleted != 0 || rep.Packs.Deleted != 0 {
		t.Fatalf("second collection: %+v %v", rep, err)
	}
}

func TestLock(t *testing.T) {
	e := setup(t)
	e.push(t, "a:1", "a")
	b, _ := json.Marshal(pull.GCLock{Instance: "other", Time: time.Now()})
	e.st.Put(e.ctx, pull.GCLockKey, b)
	if _, err := Run(e.ctx, e.st, Options{}); !errors.Is(err, ErrRunning) {
		t.Fatalf("second collection: %v", err)
	}

	// A pull waits for the collection to finish.
	done := make(chan struct{})
	go func() {
		e.resolve(t, e.p, "a:1")
		close(done)
	}()
	select {
	case <-done:
		t.Fatal("pulled during a collection")
	case <-time.After(200 * time.Millisecond):
	}
	if count(t, e.st, "images/") != 0 {
		t.Fatal("image written during a collection")
	}
	e.st.Delete(e.ctx, pull.GCLockKey)
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("pull didn't resume")
	}

	// A stale lock is taken over.
	b, _ = json.Marshal(pull.GCLock{Instance: "dead", Time: time.Now().Add(-time.Hour)})
	e.st.Put(e.ctx, pull.GCLockKey, b)
	if _, err := Run(e.ctx, e.st, Options{}); err != nil {
		t.Fatal(err)
	}
}
