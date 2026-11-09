// Package cas stores file contents as content-defined chunks, named by the
// BLAKE3 of their bytes, in a store.Store. Chunking and ids come from fvs
// core, so the same file chunks identically here and in fvs.
//
// Each chunk is one object under chunks/ for now. Packs, which group chunks
// so S3 serves them with ranged GETs, arrive with the S3 backend.
package cas

import (
	"bufio"
	"container/list"
	"context"
	"errors"
	"fmt"
	"io"
	"sync"

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

const (
	encRaw  = 0
	encZstd = 1
)

var (
	encoder, _ = zstd.NewWriter(nil, zstd.WithEncoderLevel(zstd.SpeedDefault))
	decoder, _ = zstd.NewReader(nil)
)

// CAS is a content-addressed chunk store with an in-memory cache.
type CAS struct {
	Store store.Store

	mu       sync.Mutex
	cache    map[string]*list.Element
	lru      *list.List
	bytes    int64
	maxBytes int64
}

type cached struct {
	id   string
	data []byte
}

// New returns a CAS over st that keeps up to cacheBytes of chunks in memory.
func New(st store.Store, cacheBytes int64) *CAS {
	return &CAS{
		Store:    st,
		cache:    map[string]*list.Element{},
		lru:      list.New(),
		maxBytes: cacheBytes,
	}
}

// Key returns the object key of a chunk.
func Key(id string) string {
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

// Put stores data as one chunk and returns its id. Data already stored is
// not written again.
func (c *CAS) Put(ctx context.Context, data []byte) (string, error) {
	id := string(core.ContentID(data))
	ok, err := c.Store.Has(ctx, Key(id))
	if err != nil || ok {
		return id, err
	}
	enc := encoder.EncodeAll(data, []byte{encZstd})
	if len(enc) >= len(data)+1 {
		enc = append([]byte{encRaw}, data...)
	}
	return id, c.Store.Put(ctx, Key(id), enc)
}

// Get returns a chunk's bytes, verified against its id.
func (c *CAS) Get(ctx context.Context, id string) ([]byte, error) {
	if !validID(id) {
		return nil, fmt.Errorf("cas: bad chunk id %q", id)
	}
	if b, ok := c.lookup(id); ok {
		return b, nil
	}
	enc, err := c.Store.Get(ctx, Key(id))
	if err != nil {
		return nil, err
	}
	if len(enc) == 0 {
		return nil, fmt.Errorf("cas: chunk %s: empty object", id)
	}
	var data []byte
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
	c.insert(id, data)
	return data, nil
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

// WriteFile chunks r, stores every chunk, and returns the chunk list and
// total size.
func (c *CAS) WriteFile(ctx context.Context, r io.Reader) ([]Chunk, int64, error) {
	br := bufio.NewReaderSize(r, 8192)
	head, err := br.Peek(8192)
	if err != nil && err != io.EOF && !errors.Is(err, bufio.ErrBufferFull) {
		return nil, 0, err
	}
	params := core.ParamsForContent(ChunkPolicy, core.DefaultChunkParams(), head)
	ch, err := core.NewChunker(br, params)
	if err != nil {
		return nil, 0, err
	}
	var chunks []Chunk
	var total int64
	for {
		b, err := ch.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, 0, err
		}
		id, err := c.Put(ctx, b)
		if err != nil {
			return nil, 0, err
		}
		chunks = append(chunks, Chunk{ID: id, Size: int64(len(b))})
		total += int64(len(b))
	}
	return chunks, total, nil
}

// ReadAt reads from a file made of chunks, as io.ReaderAt does, without
// fetching chunks outside the requested range.
func (c *CAS) ReadAt(ctx context.Context, chunks []Chunk, p []byte, off int64) (int, error) {
	n := 0
	var pos int64
	for _, ch := range chunks {
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
		}
		pos = end
	}
	if n < len(p) {
		return n, io.EOF
	}
	return n, nil
}
