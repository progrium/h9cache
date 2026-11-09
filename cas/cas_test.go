package cas

import (
	"bytes"
	"context"
	"errors"
	"io"
	"math/rand"
	"sync/atomic"
	"testing"
	"time"

	"github.com/progrium/h9cache/store"
	"github.com/progrium/h9cache/store/dir"
)

func randomBytes(n int, seed int64) []byte {
	b := make([]byte, n)
	rand.New(rand.NewSource(seed)).Read(b)
	return b
}

// counting wraps a store and counts reads, to check what the caches save.
type counting struct {
	store.Store
	gets, ranges atomic.Int32
}

func (c *counting) Get(ctx context.Context, k string) ([]byte, error) {
	c.gets.Add(1)
	return c.Store.Get(ctx, k)
}

func (c *counting) GetRange(ctx context.Context, k string, off, n int64) ([]byte, error) {
	c.ranges.Add(1)
	return c.Store.GetRange(ctx, k, off, n)
}

func TestWriteReadFile(t *testing.T) {
	ctx := context.Background()
	st, _ := dir.New(t.TempDir())
	c := New(st, 1<<20)

	data := randomBytes(300_000, 1)
	chunks, size, err := c.WriteFile(ctx, bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	if size != int64(len(data)) || len(chunks) < 2 {
		t.Fatalf("size %d, %d chunks", size, len(chunks))
	}
	packs, _ := st.List(ctx, "packs/")
	if len(packs) != 1 {
		t.Fatalf("%d packs, want 1", len(packs))
	}

	// Same content again stores nothing new.
	again, _, _ := c.WriteFile(ctx, bytes.NewReader(data))
	packs, _ = st.List(ctx, "packs/")
	if len(packs) != 1 || again[0] != chunks[0] {
		t.Fatal("rewrite was not deduplicated")
	}

	// A fresh CAS over the same store finds the chunks once it loads packs.
	fresh := New(st, 1<<20)
	if err := fresh.LoadPacks(ctx); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ off, n int }{{0, 10}, {1000, 70000}, {299_990, 10}, {299_990, 100}, {5, 300_000}} {
		buf := make([]byte, tc.n)
		n, err := fresh.ReadAt(ctx, chunks, buf, int64(tc.off))
		want := data[tc.off:min(tc.off+tc.n, len(data))]
		if n != len(want) || !bytes.Equal(buf[:n], want) {
			t.Fatalf("read %d@%d: got %d bytes", tc.n, tc.off, n)
		}
		if n < tc.n && err != io.EOF {
			t.Fatalf("short read without EOF: %v", err)
		}
	}
}

func TestPackTarget(t *testing.T) {
	ctx := context.Background()
	st, _ := dir.New(t.TempDir())
	c := New(st, 1<<20)
	c.PackTarget = 100_000
	b := c.NewBatch()
	var files [][]Chunk
	for i := 0; i < 5; i++ {
		ch, _, err := b.WriteFile(ctx, bytes.NewReader(randomBytes(60_000, int64(i))))
		if err != nil {
			t.Fatal(err)
		}
		files = append(files, ch)
	}
	if err := b.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	packs, _ := st.List(ctx, "packs/")
	if len(packs) < 3 {
		t.Fatalf("%d packs for 300 KB at a 100 KB target", len(packs))
	}
	for i, ch := range files {
		buf := make([]byte, 60_000)
		if n, _ := c.ReadAt(ctx, ch, buf, 0); n != 60_000 || !bytes.Equal(buf, randomBytes(60_000, int64(i))) {
			t.Fatalf("file %d read back wrong", i)
		}
	}
}

// Another instance's pack is found on a miss, by reloading the indexes.
func TestOtherWriter(t *testing.T) {
	ctx := context.Background()
	st, _ := dir.New(t.TempDir())
	reader := New(st, 1<<20)
	reader.LoadPacks(ctx)
	writer := New(st, 1<<20)
	chunks, _, _ := writer.WriteFile(ctx, bytes.NewReader([]byte("written elsewhere")))
	reader.lastRefresh = time.Time{}
	if b, err := reader.Get(ctx, chunks[0].ID); err != nil || string(b) != "written elsewhere" {
		t.Fatalf("%q %v", b, err)
	}
}

func TestLooseChunks(t *testing.T) {
	ctx := context.Background()
	st, _ := dir.New(t.TempDir())
	c := New(st, 0)
	data := []byte("a chunk from a store made before packs")
	id := "f3d6bfca7c1b4b6b4b1d1e0d7c7a8d5bf1e1fca1d0e2b8f9a3c1d1e2f3a4b5c6"
	if _, err := c.Get(ctx, id); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing chunk: %v", err)
	}
	b := c.NewBatch()
	real, _ := b.Put(ctx, data)
	st.Put(ctx, LooseKey(real), encode(data)) // stored loose, never flushed into a pack
	if got, err := c.Get(ctx, real); err != nil || !bytes.Equal(got, data) {
		t.Fatalf("loose read: %q %v", got, err)
	}
	st.Put(ctx, LooseKey(real), []byte{encRaw, 'x'})
	c2 := New(st, 0)
	if _, err := c2.Get(ctx, real); err == nil {
		t.Fatal("corrupt chunk was returned")
	}
}

func TestDiskCacheTier(t *testing.T) {
	ctx := context.Background()
	base, _ := dir.New(t.TempDir())
	cs := &counting{Store: base}
	disk, err := OpenDiskCache(t.TempDir(), 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	c := New(cs, 0) // no memory tier
	c.Disk = disk
	data := randomBytes(50_000, 9)
	chunks, _, _ := c.WriteFile(ctx, bytes.NewReader(data))

	buf := make([]byte, len(data))
	c.ReadAt(ctx, chunks, buf, 0)
	first := cs.ranges.Load()
	c.ReadAt(ctx, chunks, buf, 0)
	if cs.ranges.Load() != first || !bytes.Equal(buf, data) {
		t.Fatalf("second read went to the store: %d, then %d ranged reads", first, cs.ranges.Load())
	}

	// The cache survives a restart, and evicts to its bound.
	again, _ := OpenDiskCache(disk.dir, 1<<20)
	if again.Size() != disk.Size() || again.Size() == 0 {
		t.Fatalf("reopened cache holds %d bytes, had %d", again.Size(), disk.Size())
	}
	small, _ := OpenDiskCache(disk.dir, 20_000)
	if small.Size() > 20_000 {
		t.Fatalf("cache over its bound: %d", small.Size())
	}
}

func TestPrefetch(t *testing.T) {
	ctx := context.Background()
	base, _ := dir.New(t.TempDir())
	cs := &counting{Store: base}
	w := New(base, 0)
	data := randomBytes(400_000, 3)
	chunks, _, _ := w.WriteFile(ctx, bytes.NewReader(data))
	if len(chunks) < 6 {
		t.Fatalf("only %d chunks", len(chunks))
	}

	c := New(cs, 4<<20)
	c.Prefetch = 4
	c.LoadPacks(ctx)
	buf := make([]byte, 10)
	c.ReadAt(ctx, chunks, buf, 0) // reads chunk 0, prefetches 1-4 in one GET
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, ok := c.lookup(chunks[4].ID); ok {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("prefetch never filled the cache")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if n := cs.ranges.Load(); n != 2 {
		t.Fatalf("%d ranged reads for a read and its prefetch, want 2", n)
	}
}
