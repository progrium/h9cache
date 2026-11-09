package imagefs

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"strings"

	"github.com/progrium/h9reg/index"
	"github.com/progrium/h9reg/ninep"
	"github.com/progrium/h9reg/pull"
	"github.com/progrium/h9reg/store"
	"github.com/progrium/h9reg/tree"
)

// Names of the files the server adds at each image root.
const (
	IndexName = ".h9index"
	ImageName = ".h9image"
)

// virtual is a file the server adds to an image: never stored in its tree,
// and the same for every connection.
type virtual struct {
	name string
	data []byte
	qid  uint64
}

func (v *virtual) stat(im *image) ninep.Dir {
	t := uint32(im.rec.Pulled.Unix())
	owner := idName(im.users, 0)
	return ninep.Dir{
		Qid:    ninep.Qid{Path: v.qid},
		Mode:   0o444,
		Atime:  t,
		Mtime:  t,
		Length: uint64(len(v.data)),
		Name:   v.name,
		UID:    owner,
		GID:    idName(im.groups, 0),
		MUID:   owner,
	}
}

func (im *image) metaFile(name string) *virtual {
	for _, v := range im.meta {
		if v.name == name {
			return v
		}
	}
	return nil
}

// ImageText returns the /.h9image text for an image: its identity and the
// parts of its config a runner needs, one "key value" line each, with each
// value as JSON.
func ImageText(manifest, platform string, cfg configView) []byte {
	var b bytes.Buffer
	line := func(k string, v any) {
		j, _ := json.Marshal(v)
		fmt.Fprintf(&b, "%s %s\n", k, j)
	}
	b.WriteString("h9image 1\n")
	line("manifest", manifest)
	line("platform", platform)
	line("env", nonNil(cfg.Env))
	line("entrypoint", nonNil(cfg.Entrypoint))
	line("cmd", nonNil(cfg.Cmd))
	line("workdir", cfg.WorkingDir)
	line("user", cfg.User)
	return b.Bytes()
}

type configView struct {
	Env, Entrypoint, Cmd []string
	WorkingDir, User     string
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

// indexKey is where an image's index is kept. It's keyed by manifest, not
// tree, since it lists /.h9image, whose contents are the manifest's.
func indexKey(manifest string) string {
	return "index/v1/" + strings.Replace(manifest, ":", "/", 1)
}

// addMeta sets up /.h9image and /.h9index for im, building the index and
// storing it the first time the image is served.
func (fs *FS) addMeta(ctx context.Context, im *image) error {
	c := im.rec.Config.Config
	img := &virtual{
		name: ImageName,
		data: ImageText(im.rec.Manifest, im.rec.Platform, configView{
			Env: c.Env, Entrypoint: c.Entrypoint, Cmd: c.Cmd, WorkingDir: c.WorkingDir, User: c.User,
		}),
		qid: im.tree.QidPathOf("/" + ImageName),
	}
	im.meta = []*virtual{img}

	st := fs.Puller.Store
	key := indexKey(im.rec.Manifest)
	data, err := st.Get(ctx, key)
	if errors.Is(err, store.ErrNotFound) {
		data, err = fs.buildIndex(ctx, im)
		if err != nil {
			return fmt.Errorf("index %s: %w", im.rec.Manifest, err)
		}
		err = st.Put(ctx, key, data)
	}
	if err != nil {
		return err
	}
	im.meta = append(im.meta, &virtual{name: IndexName, data: data, qid: im.tree.QidPathOf("/" + IndexName)})
	return nil
}

// buildIndex lists the image as it's served: symlinks appear as their
// targets, under their own names, and dangling links are left out. A link
// to a directory is listed but marked unindexed rather than followed, so a
// tree with links to its own ancestors stays finite. /.h9index itself is
// left out, as mkindex leaves it out.
func (fs *FS) buildIndex(ctx context.Context, im *image) ([]byte, error) {
	ix := &index.Index{}
	add := func(p string, f *file) error {
		d, err := f.Stat(ctx)
		if err != nil {
			return err
		}
		t := byte('-')
		if d.Qid.Type&ninep.QTDIR != 0 {
			t = 'd'
		}
		ix.Entries = append(ix.Entries, index.Entry{
			Path: p, QidPath: d.Qid.Path, QidVers: d.Qid.Vers, QidType: t,
			Mode: d.Mode, Length: d.Length, Mtime: d.Mtime, UID: d.UID, GID: d.GID,
		})
		return nil
	}

	var walk func(p string, dir *file) error
	walk = func(p string, dir *file) error {
		if err := add(p, dir); err != nil {
			return err
		}
		entries, err := im.tree.ReadDir(ctx, dir.path)
		if err != nil {
			return err
		}
		for _, e := range entries {
			if len(dir.path) == 1 && im.metaFile(e.Name) != nil {
				continue
			}
			if !index.Listable(e.Name) {
				ix.Unindexed = append(ix.Unindexed, p)
				continue
			}
			cp, err := im.tree.Resolve(ctx, dir.path, e)
			if err != nil {
				continue
			}
			child := &file{fs: fs, im: im, path: cp, name: e.Name}
			childPath := path.Join(p, e.Name)
			if cp.Entry().Kind != tree.Dir {
				if err := add(childPath, child); err != nil {
					return err
				}
				continue
			}
			if e.Kind == tree.Symlink {
				if err := add(childPath, child); err != nil {
					return err
				}
				ix.Unindexed = append(ix.Unindexed, childPath)
				continue
			}
			if err := walk(childPath, child); err != nil {
				return err
			}
		}
		return nil
	}
	root := &file{fs: fs, im: im, path: im.tree.RootPath(), name: "/"}
	if err := walk("/", root); err != nil {
		return nil, err
	}
	for _, v := range im.meta {
		d := v.stat(im)
		ix.Entries = append(ix.Entries, index.Entry{
			Path: "/" + v.name, QidPath: d.Qid.Path, QidType: '-',
			Mode: d.Mode, Length: d.Length, Mtime: d.Mtime, UID: d.UID, GID: d.GID,
		})
	}
	return index.Bytes(ix)
}

// Prepare readies a pulled image for serving, building and storing its
// index if it has none, so the first attach doesn't pay for it.
func (fs *FS) Prepare(ctx context.Context, rec *pull.Image) error {
	_, err := fs.image(ctx, rec)
	return err
}
