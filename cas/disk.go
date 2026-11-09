package cas

import (
	"container/list"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"sync"

	core "github.com/fvs-lab/core"
)

// DiskCache is a bounded local cache of chunks, the tier between memory
// and the store. Chunks are kept uncompressed, one file each, and evicted
// least recently used first. It survives restarts.
type DiskCache struct {
	dir string
	max int64

	mu    sync.Mutex
	items map[string]*list.Element
	lru   *list.List
	bytes int64
}

type diskItem struct {
	id   string
	size int64
}

// OpenDiskCache opens (creating if needed) a cache in dir holding up to
// max bytes. Files already there are kept, oldest first in line for
// eviction.
func OpenDiskCache(dir string, max int64) (*DiskCache, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	d := &DiskCache{dir: dir, max: max, items: map[string]*list.Element{}, lru: list.New()}
	type found struct {
		id   string
		size int64
		mod  int64
	}
	var all []found
	filepath.WalkDir(dir, func(p string, e fs.DirEntry, err error) error {
		if err != nil || e.IsDir() {
			return nil
		}
		id := filepath.Base(filepath.Dir(p)) + e.Name()
		if !validID(id) {
			os.Remove(p) // a temporary file left by a crash
			return nil
		}
		if info, err := e.Info(); err == nil {
			all = append(all, found{id, info.Size(), info.ModTime().UnixNano()})
		}
		return nil
	})
	sort.Slice(all, func(i, j int) bool { return all[i].mod > all[j].mod })
	for _, f := range all {
		d.items[f.id] = d.lru.PushBack(&diskItem{f.id, f.size})
		d.bytes += f.size
	}
	d.mu.Lock()
	d.evictLocked()
	d.mu.Unlock()
	return d, nil
}

func (d *DiskCache) path(id string) string {
	return filepath.Join(d.dir, id[:2], id[2:])
}

// Get returns a cached chunk, checked against its id. A corrupt file is
// removed.
func (d *DiskCache) Get(id string) ([]byte, bool) {
	d.mu.Lock()
	e, ok := d.items[id]
	if ok {
		d.lru.MoveToFront(e)
	}
	d.mu.Unlock()
	if !ok {
		return nil, false
	}
	b, err := os.ReadFile(d.path(id))
	if err != nil || string(core.ContentID(b)) != id {
		d.remove(id)
		return nil, false
	}
	return b, true
}

// Put caches a chunk the caller has verified.
func (d *DiskCache) Put(id string, data []byte) {
	if int64(len(data)) > d.max {
		return
	}
	d.mu.Lock()
	if _, ok := d.items[id]; ok {
		d.mu.Unlock()
		return
	}
	d.mu.Unlock()
	p := d.path(id)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return
	}
	tmp, err := os.CreateTemp(filepath.Dir(p), ".tmp-*")
	if err != nil {
		return
	}
	_, werr := tmp.Write(data)
	cerr := tmp.Close()
	if werr != nil || cerr != nil || os.Rename(tmp.Name(), p) != nil {
		os.Remove(tmp.Name())
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if _, ok := d.items[id]; !ok {
		d.items[id] = d.lru.PushFront(&diskItem{id, int64(len(data))})
		d.bytes += int64(len(data))
	}
	d.evictLocked()
}

func (d *DiskCache) remove(id string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if e, ok := d.items[id]; ok {
		d.lru.Remove(e)
		delete(d.items, id)
		d.bytes -= e.Value.(*diskItem).size
	}
	os.Remove(d.path(id))
}

func (d *DiskCache) evictLocked() {
	for d.bytes > d.max && d.lru.Len() > 0 {
		e := d.lru.Back()
		it := e.Value.(*diskItem)
		d.lru.Remove(e)
		delete(d.items, it.id)
		d.bytes -= it.size
		os.Remove(d.path(it.id))
	}
}

// Size returns the bytes cached.
func (d *DiskCache) Size() int64 {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.bytes
}
