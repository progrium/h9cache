// Package imagefs serves pulled images as 9P trees. The attach spec names
// the image, and the attach's root is the image's merged filesystem.
// Symlinks are followed by the server, since 9P2000 has none.
package imagefs

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"sync"

	"github.com/progrium/h9reg/cas"
	"github.com/progrium/h9reg/ninep"
	"github.com/progrium/h9reg/pull"
	"github.com/progrium/h9reg/tree"
)

// ErrDevice is returned when opening a device node or FIFO, which images
// carry as names only.
var ErrDevice = errors.New("device files are not served")

// FS attaches clients to images.
type FS struct {
	Puller *pull.Puller
	CAS    *cas.CAS
	Trees  *tree.Cache

	mu     sync.Mutex
	images map[string]*image // by manifest digest
}

type image struct {
	tree   *tree.Image
	rec    *pull.Image
	users  map[int]string
	groups map[int]string
	meta   []*virtual // files the server adds at the root
}

// Attach resolves aname as an image reference, pulling the image if it
// isn't stored, and returns its root.
func (fs *FS) Attach(ctx context.Context, uname, aname string) (ninep.File, error) {
	if aname == "" {
		aname = accessFrom(ctx).Spec
	}
	im, err := fs.Open(ctx, aname)
	if err != nil {
		return nil, err
	}
	return &file{fs: fs, im: im, path: im.tree.RootPath(), name: "/"}, nil
}

// Open resolves an attach spec to a servable image, pulling it if needed,
// after checking it against the context's Access.
func (fs *FS) Open(ctx context.Context, aname string) (*Image, error) {
	if aname == "" {
		return nil, errors.New("attach spec must name an image, such as alpine:3.20")
	}
	spec, err := pull.ParseSpec(aname)
	if err != nil {
		return nil, fmt.Errorf("attach spec %q: %w", aname, err)
	}
	if err := accessFrom(ctx).check(spec); err != nil {
		return nil, err
	}
	rec, err := fs.Puller.Resolve(ctx, spec)
	if err != nil {
		return nil, err
	}
	return fs.image(ctx, rec)
}

// Image is a pulled image ready to serve.
type Image = image

// Manifest returns the image's manifest digest.
func (im *image) Manifest() string { return im.rec.Manifest }

// MetaFile returns the contents of a file the server adds at the root,
// /.h9index or /.h9image.
func (im *image) MetaFile(name string) ([]byte, bool) {
	if v := im.metaFile(name); v != nil {
		return v.data, true
	}
	return nil, false
}

func (fs *FS) image(ctx context.Context, rec *pull.Image) (*image, error) {
	fs.mu.Lock()
	im, ok := fs.images[rec.Manifest]
	fs.mu.Unlock()
	if ok {
		return im, nil
	}
	im = &image{tree: &tree.Image{Root: rec.Root, Cache: fs.Trees}, rec: rec}
	im.users = fs.idNames(ctx, im, "etc/passwd")
	im.groups = fs.idNames(ctx, im, "etc/group")
	if err := fs.addMeta(ctx, im); err != nil {
		return nil, err
	}
	fs.mu.Lock()
	defer fs.mu.Unlock()
	if fs.images == nil {
		fs.images = map[string]*image{}
	}
	if prev, ok := fs.images[rec.Manifest]; ok {
		return prev, nil
	}
	fs.images[rec.Manifest] = im
	return im, nil
}

// idNames reads an image's passwd or group file into a map of id to name.
// Both files keep the name in field 1 and the id in field 3.
func (fs *FS) idNames(ctx context.Context, im *image, p string) map[int]string {
	names := map[int]string{}
	path, err := im.tree.WalkPath(ctx, p)
	if err != nil {
		return names
	}
	e := path.Entry()
	if e.Kind != tree.File || e.Size > 1<<20 {
		return names
	}
	buf := make([]byte, e.Size)
	n, err := fs.CAS.ReadAt(ctx, e.Chunks, buf, 0)
	if err != nil && err != io.EOF {
		return names
	}
	sc := bufio.NewScanner(bytes.NewReader(buf[:n]))
	for sc.Scan() {
		f := strings.Split(sc.Text(), ":")
		if len(f) < 3 {
			continue
		}
		id, err := strconv.Atoi(f[2])
		if err != nil {
			continue
		}
		if _, dup := names[id]; !dup {
			names[id] = f[0]
		}
	}
	return names
}

func idName(m map[int]string, id int) string {
	if n, ok := m[id]; ok {
		return n
	}
	return strconv.Itoa(id)
}

type file struct {
	fs   *FS
	im   *image
	path tree.Path
	name string   // the name this file was reached by
	virt *virtual // set for a file the server adds
}

func (f *file) Qid() ninep.Qid {
	if f.virt != nil {
		return ninep.Qid{Path: f.virt.qid}
	}
	q := ninep.Qid{Path: f.im.tree.QidPath(f.path)}
	if f.path.Entry().Kind == tree.Dir {
		q.Type = ninep.QTDIR
	}
	return q
}

func (f *file) Walk(ctx context.Context, name string) (ninep.File, error) {
	if len(f.path) == 1 && f.virt == nil {
		if v := f.im.metaFile(name); v != nil {
			return &file{fs: f.fs, im: f.im, path: f.path, name: name, virt: v}, nil
		}
	}
	p, err := f.im.tree.Walk(ctx, f.path, name)
	if err != nil {
		return nil, err
	}
	if name == ".." || name == "." {
		name = p[len(p)-1].Name
		if len(p) == 1 {
			name = "/"
		}
	}
	return &file{fs: f.fs, im: f.im, path: p, name: name}, nil
}

func (f *file) Stat(ctx context.Context) (ninep.Dir, error) {
	if f.virt != nil {
		return f.virt.stat(f.im), nil
	}
	e := f.path.Entry()
	d := ninep.Dir{
		Qid:   f.Qid(),
		Mode:  e.Mode & 0o777,
		Mtime: uint32(e.MTime),
		Atime: uint32(e.MTime),
		Name:  f.name,
		UID:   idName(f.im.users, e.UID),
		GID:   idName(f.im.groups, e.GID),
	}
	d.MUID = d.UID
	switch e.Kind {
	case tree.Dir:
		d.Mode |= ninep.DMDIR
	case tree.File:
		d.Length = uint64(e.Size)
	}
	return d, nil
}

func (f *file) Open(ctx context.Context, mode uint8) error {
	if f.virt != nil {
		return nil
	}
	switch f.path.Entry().Kind {
	case tree.Char, tree.Block, tree.FIFO:
		return ErrDevice
	}
	return nil
}

func (f *file) ReadAt(ctx context.Context, p []byte, off int64) (int, error) {
	if f.virt != nil {
		if off >= int64(len(f.virt.data)) {
			return 0, io.EOF
		}
		return copy(p, f.virt.data[off:]), nil
	}
	e := f.path.Entry()
	if e.Kind != tree.File || off >= e.Size {
		return 0, io.EOF
	}
	if rem := e.Size - off; int64(len(p)) > rem {
		p = p[:rem]
	}
	return f.fs.CAS.ReadAt(ctx, e.Chunks, p, off)
}

func (f *file) ReadDir(ctx context.Context) ([]ninep.Dir, error) {
	entries, err := f.im.tree.ReadDir(ctx, f.path)
	if err != nil {
		return nil, err
	}
	dirs := make([]ninep.Dir, 0, len(entries)+len(f.im.meta))
	root := len(f.path) == 1
	if root {
		for _, v := range f.im.meta {
			dirs = append(dirs, v.stat(f.im))
		}
	}
	for _, e := range entries {
		if root && f.im.metaFile(e.Name) != nil {
			continue // shadowed by the server's own file
		}
		p, err := f.im.tree.Resolve(ctx, f.path, e)
		if err != nil {
			// A dangling or looping symlink has nothing to show.
			continue
		}
		c := &file{fs: f.fs, im: f.im, path: p, name: e.Name}
		d, err := c.Stat(ctx)
		if err != nil {
			return nil, err
		}
		dirs = append(dirs, d)
	}
	return dirs, nil
}
