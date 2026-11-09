package cas

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	core "github.com/fvs-lab/core"
	"github.com/zeebo/blake3"
)

// A pack is the concatenation of its chunks' encoded bytes, stored at
// packs/<id>, where id is the BLAKE3 of the pack. Its index, at
// packidx/<id>, is a header line and then one 48-byte record per chunk:
// the chunk id (32 bytes), its offset in the pack (8), its encoded
// length (4), and its size (4), little-endian.

// packIndexHeader keeps the project's first name: it's in every stored
// pack index, so changing it would orphan existing stores.
const packIndexHeader = "h9reg-packidx 1\n"

const packRecord = 48

// PackKey is the object key of a pack.
func PackKey(id string) string { return "packs/" + id }

// PackIndexKey is the object key of a pack's index.
func PackIndexKey(id string) string { return "packidx/" + id }

type packEntry struct {
	id   string
	off  int64
	n    int64
	size int64
}

func encodePackIndex(entries []packEntry) ([]byte, error) {
	b := make([]byte, 0, len(packIndexHeader)+packRecord*len(entries))
	b = append(b, packIndexHeader...)
	for _, e := range entries {
		raw, err := hex.DecodeString(e.id)
		if err != nil || len(raw) != 32 {
			return nil, fmt.Errorf("cas: bad chunk id %q", e.id)
		}
		b = append(b, raw...)
		b = binary.LittleEndian.AppendUint64(b, uint64(e.off))
		b = binary.LittleEndian.AppendUint32(b, uint32(e.n))
		b = binary.LittleEndian.AppendUint32(b, uint32(e.size))
	}
	return b, nil
}

func decodePackIndex(b []byte) ([]packEntry, error) {
	if !bytes.HasPrefix(b, []byte(packIndexHeader)) {
		return nil, fmt.Errorf("cas: pack index: bad header")
	}
	b = b[len(packIndexHeader):]
	if len(b)%packRecord != 0 {
		return nil, fmt.Errorf("cas: pack index: truncated")
	}
	entries := make([]packEntry, 0, len(b)/packRecord)
	for ; len(b) > 0; b = b[packRecord:] {
		entries = append(entries, packEntry{
			id:   hex.EncodeToString(b[:32]),
			off:  int64(binary.LittleEndian.Uint64(b[32:])),
			n:    int64(binary.LittleEndian.Uint32(b[40:])),
			size: int64(binary.LittleEndian.Uint32(b[44:])),
		})
	}
	return entries, nil
}

// LoadPacks reads the index of every pack not loaded yet. Indexes are
// immutable, so each is read once.
func (c *CAS) LoadPacks(ctx context.Context) error {
	keys, err := c.Store.List(ctx, "packidx/")
	if err != nil {
		return err
	}
	c.pmu.Lock()
	c.lastRefresh = time.Now()
	first := len(c.packs) == 0 && c.fullLoad.IsZero()
	var todo []string
	for _, k := range keys {
		id := strings.TrimPrefix(k, "packidx/")
		if !c.packs[id] {
			todo = append(todo, id)
		}
	}
	c.pmu.Unlock()

	// Read new indexes a few at a time, which matters on S3 at startup.
	var wg sync.WaitGroup
	var mu sync.Mutex
	var firstErr error
	sem := make(chan struct{}, 16)
	for _, id := range todo {
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer func() { <-sem; wg.Done() }()
			b, err := c.Store.Get(ctx, PackIndexKey(id))
			var entries []packEntry
			if err == nil {
				entries, err = decodePackIndex(b)
			}
			if err != nil {
				mu.Lock()
				if firstErr == nil {
					firstErr = fmt.Errorf("pack %s: %w", id, err)
				}
				mu.Unlock()
				return
			}
			c.register(id, entries)
		}()
	}
	wg.Wait()
	if first && firstErr == nil {
		c.pmu.Lock()
		if c.fullLoad.IsZero() {
			c.fullLoad = c.lastRefresh
		}
		c.pmu.Unlock()
	}
	return firstErr
}

// ReloadPacks forgets every loaded index and reads them all again, after
// garbage collection deleted or rewrote packs.
func (c *CAS) ReloadPacks(ctx context.Context) error {
	start := time.Now()
	c.pmu.Lock()
	c.locs = map[string]loc{}
	c.packs = map[string]bool{}
	c.pmu.Unlock()
	if err := c.LoadPacks(ctx); err != nil {
		return err
	}
	c.pmu.Lock()
	c.fullLoad = start
	c.pmu.Unlock()
	return nil
}

// PacksLoadedBefore reports whether the indexes were last loaded whole
// before t.
func (c *CAS) PacksLoadedBefore(t time.Time) bool {
	c.pmu.RLock()
	defer c.pmu.RUnlock()
	return c.fullLoad.Before(t)
}

func (c *CAS) register(pack string, entries []packEntry) {
	c.pmu.Lock()
	defer c.pmu.Unlock()
	for _, e := range entries {
		if _, ok := c.locs[e.id]; !ok {
			c.locs[e.id] = loc{pack: pack, off: e.off, n: e.n}
		}
	}
	c.packs[pack] = true
}

// PackStats reports how many packs and packed chunks are loaded.
func (c *CAS) PackStats() (packs, chunks int) {
	c.pmu.RLock()
	defer c.pmu.RUnlock()
	return len(c.packs), len(c.locs)
}

// Batch gathers new chunks into packs. A pull uses one batch per layer and
// flushes it before recording the layer, so nothing refers to a chunk
// that isn't stored yet. A Batch is not safe for concurrent use.
type Batch struct {
	c       *CAS
	buf     bytes.Buffer
	entries []packEntry
	pending map[string]bool
}

// NewBatch starts a batch.
func (c *CAS) NewBatch() *Batch {
	return &Batch{c: c, pending: map[string]bool{}}
}

// Put adds one chunk, unless it's stored already, and returns its id. The
// batch is flushed when it reaches the pack target.
func (b *Batch) Put(ctx context.Context, data []byte) (string, error) {
	id := string(core.ContentID(data))
	if b.pending[id] || b.c.Has(id) {
		return id, nil
	}
	enc := encode(data)
	b.entries = append(b.entries, packEntry{id: id, off: int64(b.buf.Len()), n: int64(len(enc)), size: int64(len(data))})
	b.buf.Write(enc)
	b.pending[id] = true
	target := b.c.PackTarget
	if target <= 0 {
		target = DefaultPackTarget
	}
	if int64(b.buf.Len()) >= target {
		return id, b.Flush(ctx)
	}
	return id, nil
}

// WriteFile chunks r into the batch and returns the chunk list and total
// size.
func (b *Batch) WriteFile(ctx context.Context, r io.Reader) ([]Chunk, int64, error) {
	br, head, err := readHead(r)
	if err != nil {
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
		data, err := ch.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, 0, err
		}
		id, err := b.Put(ctx, data)
		if err != nil {
			return nil, 0, err
		}
		chunks = append(chunks, Chunk{ID: id, Size: int64(len(data))})
		total += int64(len(data))
	}
	return chunks, total, nil
}

// Flush writes the pending chunks as one pack, then its index, and makes
// them readable.
func (b *Batch) Flush(ctx context.Context) error {
	if len(b.entries) == 0 {
		return nil
	}
	data := b.buf.Bytes()
	sum := blake3.Sum256(data)
	id := hex.EncodeToString(sum[:])
	idx, err := encodePackIndex(b.entries)
	if err != nil {
		return err
	}
	// The pack goes first: an index is only ever visible for a whole pack.
	if err := b.c.Store.Put(ctx, PackKey(id), data); err != nil {
		return err
	}
	if err := b.c.Store.Put(ctx, PackIndexKey(id), idx); err != nil {
		return err
	}
	b.c.register(id, b.entries)
	b.c.log().Debug("wrote pack", "pack", id, "chunks", len(b.entries), "bytes", len(data))
	b.buf.Reset()
	b.entries = nil
	b.pending = map[string]bool{}
	return nil
}
