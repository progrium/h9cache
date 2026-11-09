// Package imagefs serves pulled images as 9P trees. The attach spec names
// the image, and the attach's root is the image's merged filesystem.
// Over 9P2000, which has no symlinks, the server follows them. Over
// 9P2000.L they're served as symlinks, and the client follows them.
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

	"github.com/progrium/h9cache/cas"
	"github.com/progrium/h9cache/ninep"
	"github.com/progrium/h9cache/pull"
	"github.com/progrium/h9cache/tree"
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

// AttachL is Attach for a 9P2000.L client. Its tree doesn't follow
// symlinks, and shows numeric owners.
func (fs *FS) AttachL(ctx context.Context, uname string, nuname uint32, aname string) (ninep.LFile, error) {
	root, err := fs.Attach(ctx, uname, aname)
	if err != nil {
		return nil, err
	}
	f := root.(*file)
	f.l = true
	return f, nil
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
	// Access is checked against the repository actually pulled.
	if spec, err = fs.Puller.Locate(ctx, spec); err != nil {
		return nil, err
	}
	if err := accessFrom(ctx).check(spec); err != nil {
		return nil, err
	}
	rec, err := fs.Puller.Resolve(ctx, spec)
	if err != nil {
		return nil, err
	}
	fs.Puller.Touch(ctx, rec)
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

// owners names an entry's owner and group: the names its layer carried,
// else its ids as the image's passwd and group files name them.
func (im *image) owners(e tree.Entry) (string, string) {
	u, g := e.User, e.Group
	if u == "" {
		u = idName(im.users, e.UID)
	}
	if g == "" {
		g = idName(im.groups, e.GID)
	}
	return u, g
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
	l    bool     // served over 9P2000.L: symlinks aren't followed
}

func (f *file) Qid() ninep.Qid {
	if f.virt != nil {
		return ninep.Qid{Path: f.virt.qid}
	}
	q := ninep.Qid{Path: f.im.tree.QidPath(f.path)}
	switch f.path.Entry().Kind {
	case tree.Dir:
		q.Type = ninep.QTDIR
	case tree.Symlink:
		q.Type = ninep.QTSYMLINK
	}
	return q
}

func (f *file) Walk(ctx context.Context, name string) (ninep.File, error) {
	if len(f.path) == 1 && f.virt == nil {
		if v := f.im.metaFile(name); v != nil {
			return &file{fs: f.fs, im: f.im, path: f.path, name: name, virt: v, l: f.l}, nil
		}
	}
	walk := f.im.tree.Walk
	if f.l {
		walk = f.im.tree.Step
	}
	p, err := walk(ctx, f.path, name)
	if err != nil {
		return nil, err
	}
	if name == ".." || name == "." {
		name = p[len(p)-1].Name
		if len(p) == 1 {
			name = "/"
		}
	}
	return &file{fs: f.fs, im: f.im, path: p, name: name, l: f.l}, nil
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
	}
	d.UID, d.GID = f.im.owners(e)
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

// Attr returns a file's attributes for 9P2000.L.
func (f *file) Attr(ctx context.Context) (ninep.Attr, error) {
	if f.virt != nil {
		return ninep.Attr{Mode: ninep.S_IFREG | 0o444, Nlink: 1, Size: uint64(len(f.virt.data))}, nil
	}
	e := f.path.Entry()
	a := ninep.Attr{
		Mode:  linuxType(e.Kind) | e.Mode&0o7777,
		UID:   uint32(e.UID),
		GID:   uint32(e.GID),
		Nlink: 1,
		Mtime: e.MTime,
	}
	switch e.Kind {
	case tree.File:
		a.Size = uint64(e.Size)
	case tree.Symlink:
		a.Size = uint64(len(e.Target))
	case tree.Char, tree.Block:
		a.Rdev = encodeDev(e.Major, e.Minor)
	}
	return a, nil
}

// Readlink returns a symlink's target, as the layer gave it.
func (f *file) Readlink(ctx context.Context) (string, error) {
	if f.virt != nil || f.path.Entry().Kind != tree.Symlink {
		return "", ninep.Errno(ninep.EINVAL)
	}
	return f.path.Entry().Target, nil
}

// ReadDirL lists a directory for 9P2000.L, with symlinks as themselves.
func (f *file) ReadDirL(ctx context.Context) ([]ninep.DirEntL, error) {
	entries, err := f.im.tree.ReadDir(ctx, f.path)
	if err != nil {
		return nil, err
	}
	out := make([]ninep.DirEntL, 0, len(entries)+len(f.im.meta))
	root := len(f.path) == 1
	if root {
		for _, v := range f.im.meta {
			out = append(out, ninep.DirEntL{Name: v.name, Qid: ninep.Qid{Path: v.qid}, Type: ninep.S_IFREG})
		}
	}
	for _, e := range entries {
		if root && f.im.metaFile(e.Name) != nil {
			continue
		}
		c := &file{fs: f.fs, im: f.im, path: append(append(tree.Path(nil), f.path...), tree.Step{Name: e.Name, Entry: e}), l: true}
		out = append(out, ninep.DirEntL{Name: e.Name, Qid: c.Qid(), Type: linuxType(e.Kind)})
	}
	return out, nil
}

// encodeDev packs device numbers as Linux's new_encode_dev does, which is
// how v9fs decodes st_rdev.
func encodeDev(major, minor int64) uint64 {
	ma, mi := uint64(major), uint64(minor)
	return mi&0xff | ma<<8 | (mi&^0xff)<<12
}

func linuxType(k tree.Kind) uint32 {
	switch k {
	case tree.Dir:
		return ninep.S_IFDIR
	case tree.Symlink:
		return ninep.S_IFLNK
	case tree.Char:
		return ninep.S_IFCHR
	case tree.Block:
		return ninep.S_IFBLK
	case tree.FIFO:
		return ninep.S_IFIFO
	}
	return ninep.S_IFREG
}
