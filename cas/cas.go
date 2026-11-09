// Package cas stores file contents as content-defined chunks, named by the
// BLAKE3 of their bytes. Chunking and ids come from fvs core, so the same
// file chunks identically here and in fvs.
//
// Chunks are written in packs: one object holding many chunks, each
// compressed on its own, with a separate index object listing where each
// chunk is. A read is one ranged GET of the chunk's bytes, so an object
// store serves it with one request no matter how small the chunk. Pack
// indexes are loaded at startup and kept in memory.
//
// Reads go through two cache tiers before the store: memory, then an
// optional local disk cache. A chunk is verified against its id before it
// enters either.
package cas

import (
	"bufio"
	"container/list"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"time"

	core "github.com/fvs-lab/core"
	"github.com/klauspost/compress/zstd"

	"github.com/progrium/h9reg/store"
)

// Chunk is one piece of a file: its content id and length.
type Chunk struct {
	ID   string `json:"id"`
	Size int64  `json:"n"`
}

// ChunkPolicy is fvs core's chunking policy 1: text-like content gets
// smaller chunks.
const ChunkPolicy = 1

// DefaultPackTarget is the size at which a batch closes its pack.
const DefaultPackTarget = 32 << 20

const (
	encRaw  = 0
	encZstd = 1
)

var (
	encoder, _ = zstd.NewWriter(nil, zstd.WithEncoderLevel(zstd.SpeedDefault))
	decoder, _ = zstd.NewReader(nil)
)

// ErrNotFound is returned for a chunk that's in no pack and not stored
// loose.
var ErrNotFound = errors.New("chunk not found")

// CAS is a content-addressed chunk store.
type CAS struct {
	Store      store.Store
	PackTarget int64      // pack size a batch aims for; DefaultPackTarget if zero
	Disk       *DiskCache // optional second cache tier
	Prefetch   int        // chunks to read ahead after a read; 0 for none
	Log        *slog.Logger

	// The memory tier.
	mu       sync.Mutex
	cache    map[string]*list.Element
	lru      *list.List
	bytes    int64
	maxBytes int64

	// Where each packed chunk is.
	pmu         sync.RWMutex
	locs        map[string]loc
	packs       map[string]bool // pack ids whose index is loaded
	lastRefresh time.Time

	prefetching chan struct{} // bounds read-ahead goroutines
}

type loc struct {
	pack string
	off  int64
	n    int64 // encoded length
}

type cached struct {
	id   string
	data []byte
}

// New returns a CAS over st that keeps up to cacheBytes of chunks in
// memory. Call LoadPacks before serving reads.
func New(st store.Store, cacheBytes int64) *CAS {
	return &CAS{
		Store:       st,
		cache:       map[string]*list.Element{},
		lru:         list.New(),
		maxBytes:    cacheBytes,
		locs:        map[string]loc{},
		packs:       map[string]bool{},
		prefetching: make(chan struct{}, 4),
	}
}

func (c *CAS) log() *slog.Logger {
	if c.Log == nil {
		return slog.Default()
	}
	return c.Log
}

// LooseKey is the object key of a chunk stored on its own, as stores made
// before packs keep them.
func LooseKey(id string) string {
	return "chunks/" + id[:2] + "/" + id[2:]
}

func validID(id string) bool {
	if len(id) != 64 {
		return false
	}
	for _, c := range id {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

func encode(data []byte) []byte {
	enc := encoder.EncodeAll(data, []byte{encZstd})
	if len(enc) >= len(data)+1 {
		enc = append([]byte{encRaw}, data...)
	}
	return enc
}

func decode(id string, enc []byte) ([]byte, error) {
	if len(enc) == 0 {
		return nil, fmt.Errorf("cas: chunk %s: empty", id)
	}
	var data []byte
	var err error
	switch enc[0] {
	case encRaw:
		data = enc[1:]
	case encZstd:
		data, err = decoder.DecodeAll(enc[1:], nil)
		if err != nil {
			return nil, fmt.Errorf("cas: chunk %s: %w", id, err)
		}
	default:
		return nil, fmt.Errorf("cas: chunk %s: unknown encoding %d", id, enc[0])
	}
	if string(core.ContentID(data)) != id {
		return nil, fmt.Errorf("cas: chunk %s: %w", id, core.ErrBlockCorrupt)
	}
	return data, nil
}

// Has reports whether a chunk is in a loaded pack.
func (c *CAS) Has(id string) bool {
	c.pmu.RLock()
	defer c.pmu.RUnlock()
	_, ok := c.locs[id]
	return ok
}

// Get returns a chunk's bytes, verified against its id.
func (c *CAS) Get(ctx context.Context, id string) ([]byte, error) {
	if !validID(id) {
		return nil, fmt.Errorf("cas: bad chunk id %q", id)
	}
	if b, ok := c.lookup(id); ok {
		return b, nil
	}
	if c.Disk != nil {
		if b, ok := c.Disk.Get(id); ok {
			c.insert(id, b)
			return b, nil
		}
	}
	enc, err := c.fetch(ctx, id)
	if err != nil {
		return nil, err
	}
	data, err := decode(id, enc)
	if err != nil {
		return nil, err
	}
	c.keep(id, data)
	return data, nil
}

// keep puts a verified chunk in both cache tiers.
func (c *CAS) keep(id string, data []byte) {
	c.insert(id, data)
	if c.Disk != nil {
		c.Disk.Put(id, data)
	}
}

// fetch reads a chunk's encoded bytes from its pack, or loose.
func (c *CAS) fetch(ctx context.Context, id string) ([]byte, error) {
	l, ok := c.where(ctx, id)
	if ok {
		enc, err := c.Store.GetRange(ctx, PackKey(l.pack), l.off, l.n)
		if err != nil {
			return nil, fmt.Errorf("cas: chunk %s in pack %s: %w", id, l.pack, err)
		}
		if int64(len(enc)) != l.n {
			return nil, fmt.Errorf("cas: chunk %s in pack %s: short read", id, l.pack)
		}
		return enc, nil
	}
	enc, err := c.Store.Get(ctx, LooseKey(id))
	if errors.Is(err, store.ErrNotFound) {
		return nil, fmt.Errorf("cas: chunk %s: %w", id, ErrNotFound)
	}
	return enc, err
}

// where finds a chunk's pack. On a miss it reloads the pack list, at most
// once a second, in case another instance wrote the pack.
func (c *CAS) where(ctx context.Context, id string) (loc, bool) {
	c.pmu.RLock()
	l, ok := c.locs[id]
	recent := time.Since(c.lastRefresh) < time.Second
	c.pmu.RUnlock()
	if ok || recent {
		return l, ok
	}
	if err := c.LoadPacks(ctx); err != nil {
		c.log().Warn("reloading pack indexes", "err", err)
	}
	c.pmu.RLock()
	defer c.pmu.RUnlock()
	l, ok = c.locs[id]
	return l, ok
}

func (c *CAS) lookup(id string) ([]byte, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if e, ok := c.cache[id]; ok {
		c.lru.MoveToFront(e)
		return e.Value.(*cached).data, true
	}
	return nil, false
}

func (c *CAS) insert(id string, data []byte) {
	if int64(len(data)) > c.maxBytes {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.cache[id]; ok {
		return
	}
	c.cache[id] = c.lru.PushFront(&cached{id, data})
	c.bytes += int64(len(data))
	for c.bytes > c.maxBytes {
		e := c.lru.Back()
		v := e.Value.(*cached)
		c.lru.Remove(e)
		delete(c.cache, v.id)
		c.bytes -= int64(len(v.data))
	}
}

// WriteFile chunks r into a batch of its own and flushes it. A caller
// writing many files should share one Batch instead.
func (c *CAS) WriteFile(ctx context.Context, r io.Reader) ([]Chunk, int64, error) {
	b := c.NewBatch()
	chunks, size, err := b.WriteFile(ctx, r)
	if err != nil {
		return nil, 0, err
	}
	return chunks, size, b.Flush(ctx)
}

// ReadAt reads from a file made of chunks, as io.ReaderAt does, without
// fetching chunks outside the requested range. With Prefetch set, the
// chunks after the range are fetched in the background.
func (c *CAS) ReadAt(ctx context.Context, chunks []Chunk, p []byte, off int64) (int, error) {
	n := 0
	var pos int64
	last := -1
	for i, ch := range chunks {
		if len(p) == n {
			break
		}
		end := pos + ch.Size
		if off+int64(n) < end {
			data, err := c.Get(ctx, ch.ID)
			if err != nil {
				return n, err
			}
			if int64(len(data)) != ch.Size {
				return n, fmt.Errorf("cas: chunk %s: size %d, want %d", ch.ID, len(data), ch.Size)
			}
			n += copy(p[n:], data[off+int64(n)-pos:])
			last = i
		}
		pos = end
	}
	if last >= 0 && c.Prefetch > 0 && last+1 < len(chunks) {
		c.prefetch(chunks[last+1 : min(len(chunks), last+1+c.Prefetch)])
	}
	if n < len(p) {
		return n, io.EOF
	}
	return n, nil
}

// prefetch fetches chunks that aren't cached yet, in the background,
// reading runs of chunks that sit next to each other in one pack with a
// single ranged GET. It's skipped when enough prefetches are running.
func (c *CAS) prefetch(chunks []Chunk) {
	select {
	case c.prefetching <- struct{}{}:
	default:
		return
	}
	go func() {
		defer func() { <-c.prefetching }()
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		type want struct {
			id string
			l  loc
		}
		var run []want
		flush := func() {
			if len(run) == 0 {
				return
			}
			first, lastw := run[0].l, run[len(run)-1].l
			enc, err := c.Store.GetRange(ctx, PackKey(first.pack), first.off, lastw.off+lastw.n-first.off)
			if err == nil && int64(len(enc)) == lastw.off+lastw.n-first.off {
				for _, w := range run {
					if data, err := decode(w.id, enc[w.l.off-first.off:w.l.off-first.off+w.l.n]); err == nil {
						c.keep(w.id, data)
					}
				}
			}
			run = run[:0]
		}
		for _, ch := range chunks {
			if _, ok := c.lookup(ch.ID); ok {
				continue
			}
			c.pmu.RLock()
			l, ok := c.locs[ch.ID]
			c.pmu.RUnlock()
			if !ok {
				continue
			}
			if len(run) > 0 {
				prev := run[len(run)-1].l
				if prev.pack != l.pack || prev.off+prev.n != l.off {
					flush()
				}
			}
			run = append(run, want{ch.ID, l})
		}
		flush()
	}()
}

// readHead returns a reader over r and up to the first 8 KB of it, for
// choosing chunk sizes.
func readHead(r io.Reader) (*bufio.Reader, []byte, error) {
	br := bufio.NewReaderSize(r, 8192)
	head, err := br.Peek(8192)
	if err != nil && err != io.EOF && !errors.Is(err, bufio.ErrBufferFull) {
		return nil, nil, err
	}
	return br, head, nil
}
