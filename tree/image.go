package tree

import (
	"container/list"
	"context"
	"encoding/binary"
	"errors"
	"sort"
	"strings"
	"sync"

	"github.com/zeebo/blake3"

	"github.com/progrium/h9reg/store"
)

// Errors from walking an image.
var (
	ErrNotExist = errors.New("file does not exist")
	ErrNotDir   = errors.New("not a directory")
	ErrLoop     = errors.New("too many levels of symbolic links")
)

// MaxSymlinks bounds how many links one walk follows, as Linux does.
const MaxSymlinks = 40

// DirObject is a decoded tree object, ready for lookups.
type DirObject struct {
	Entries []Entry // sorted by name
}

// Lookup finds an entry by name.
func (d *DirObject) Lookup(name string) (Entry, bool) {
	i := sort.Search(len(d.Entries), func(i int) bool { return d.Entries[i].Name >= name })
	if i < len(d.Entries) && d.Entries[i].Name == name {
		return d.Entries[i], true
	}
	return Entry{}, false
}

// Cache keeps decoded tree objects in memory. Tree objects never change,
// so entries never go stale.
type Cache struct {
	Store store.Store

	mu    sync.Mutex
	items map[string]*list.Element
	lru   *list.List
	max   int
}

type cacheItem struct {
	id  string
	dir *DirObject
}

// NewCache returns a cache of up to max tree objects over st.
func NewCache(st store.Store, max int) *Cache {
	return &Cache{Store: st, items: map[string]*list.Element{}, lru: list.New(), max: max}
}

// Dir returns the decoded tree object id.
func (c *Cache) Dir(ctx context.Context, id string) (*DirObject, error) {
	c.mu.Lock()
	if e, ok := c.items[id]; ok {
		c.lru.MoveToFront(e)
		c.mu.Unlock()
		return e.Value.(*cacheItem).dir, nil
	}
	c.mu.Unlock()
	entries, err := Read(ctx, c.Store, id)
	if err != nil {
		return nil, err
	}
	d := &DirObject{Entries: entries}
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.items[id]; !ok {
		c.items[id] = c.lru.PushFront(&cacheItem{id, d})
		for c.lru.Len() > c.max {
			e := c.lru.Back()
			c.lru.Remove(e)
			delete(c.items, e.Value.(*cacheItem).id)
		}
	}
	return d, nil
}

// Image is a merged image tree, read for serving.
type Image struct {
	Root  Entry // the root directory's entry; Root.Tree is the image tree id
	Cache *Cache
}

// Step is one element of a resolved path.
type Step struct {
	Name  string
	Entry Entry
}

// Path is a resolved location in an image: the chain of directories from
// the root, with no symlinks in it. The last step is the file itself.
type Path []Step

// String returns the canonical image path, "/" for the root.
func (p Path) String() string {
	if len(p) <= 1 {
		return "/"
	}
	names := make([]string, 0, len(p)-1)
	for _, s := range p[1:] {
		names = append(names, s.Name)
	}
	return "/" + strings.Join(names, "/")
}

// Entry returns the entry at the end of the path.
func (p Path) Entry() Entry { return p[len(p)-1].Entry }

// RootPath returns the path of the image root.
func (img *Image) RootPath() Path {
	return Path{{Name: "", Entry: img.Root}}
}

// Walk moves one name from p. ".." stops at the root. A symlink is
// followed inside the image: an absolute target starts at the image root.
func (img *Image) Walk(ctx context.Context, p Path, name string) (Path, error) {
	depth := 0
	return img.resolve(ctx, p, []string{name}, &depth)
}

// WalkPath resolves a slash-separated path from the root.
func (img *Image) WalkPath(ctx context.Context, p string) (Path, error) {
	depth := 0
	return img.resolve(ctx, img.RootPath(), strings.Split(p, "/"), &depth)
}

// Resolve follows the symlink entry e found in the directory at dir.
func (img *Image) Resolve(ctx context.Context, dir Path, e Entry) (Path, error) {
	if e.Kind != Symlink {
		return append(append(Path(nil), dir...), Step{e.Name, e}), nil
	}
	depth := 1
	return img.follow(ctx, dir, e.Target, &depth)
}

func (img *Image) follow(ctx context.Context, dir Path, target string, depth *int) (Path, error) {
	start := dir
	if strings.HasPrefix(target, "/") {
		start = dir[:1]
	}
	return img.resolve(ctx, start, strings.Split(target, "/"), depth)
}

func (img *Image) resolve(ctx context.Context, p Path, names []string, depth *int) (Path, error) {
	cur := append(Path(nil), p...)
	for _, name := range names {
		switch name {
		case "", ".":
			continue
		case "..":
			if len(cur) > 1 {
				cur = cur[:len(cur)-1]
			}
			continue
		}
		dir := cur.Entry()
		if dir.Kind != Dir {
			return nil, ErrNotDir
		}
		d, err := img.Cache.Dir(ctx, dir.Tree)
		if err != nil {
			return nil, err
		}
		e, ok := d.Lookup(name)
		if !ok {
			return nil, ErrNotExist
		}
		if e.Kind == Symlink {
			*depth++
			if *depth > MaxSymlinks {
				return nil, ErrLoop
			}
			cur, err = img.follow(ctx, cur, e.Target, depth)
			if err != nil {
				return nil, err
			}
			continue
		}
		cur = append(cur, Step{name, e})
	}
	return cur, nil
}

// ReadDir returns the entries of the directory at p.
func (img *Image) ReadDir(ctx context.Context, p Path) ([]Entry, error) {
	e := p.Entry()
	if e.Kind != Dir {
		return nil, ErrNotDir
	}
	d, err := img.Cache.Dir(ctx, e.Tree)
	if err != nil {
		return nil, err
	}
	return d.Entries, nil
}

// QidPath derives a file's qid path from the image tree id and the file's
// canonical path, so every server instance and every connection gives the
// same file the same qid. Names of one hardlinked file share a qid.
func (img *Image) QidPath(p Path) uint64 {
	key := p.String()
	if l := p.Entry().Link; l != "" {
		key = l
	}
	return img.QidPathOf(key)
}

// QidPathOf derives the qid path for an image path that may not be in the
// tree, such as a file the server adds.
func (img *Image) QidPathOf(key string) uint64 {
	h := blake3.New()
	h.WriteString(img.Root.Tree)
	h.Write([]byte{0})
	h.WriteString(key)
	return binary.BigEndian.Uint64(h.Sum(nil)[:8])
}
