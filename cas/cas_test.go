package cas

import (
	"bytes"
	"context"
	"io"
	"math/rand"
	"testing"

	"github.com/progrium/h9reg/store/dir"
)

func TestWriteReadFile(t *testing.T) {
	ctx := context.Background()
	st, _ := dir.New(t.TempDir())
	c := New(st, 1<<20)

	data := make([]byte, 300_000)
	rand.New(rand.NewSource(1)).Read(data)
	chunks, size, err := c.WriteFile(ctx, bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	if size != int64(len(data)) || len(chunks) < 2 {
		t.Fatalf("size %d, %d chunks", size, len(chunks))
	}

	// Same content again stores nothing new.
	before, _ := st.List(ctx, "chunks/")
	again, _, _ := c.WriteFile(ctx, bytes.NewReader(data))
	after, _ := st.List(ctx, "chunks/")
	if len(before) != len(after) || again[0] != chunks[0] {
		t.Fatal("rewrite was not deduplicated")
	}

	for _, tc := range []struct{ off, n int }{{0, 10}, {1000, 70000}, {299_990, 10}, {299_990, 100}, {5, 300_000}} {
		buf := make([]byte, tc.n)
		n, err := c.ReadAt(ctx, chunks, buf, int64(tc.off))
		want := data[tc.off:min(tc.off+tc.n, len(data))]
		if n != len(want) || !bytes.Equal(buf[:n], want) {
			t.Fatalf("read %d@%d: got %d bytes", tc.n, tc.off, n)
		}
		if n < tc.n && err != io.EOF {
			t.Fatalf("short read without EOF: %v", err)
		}
	}
}

func TestEmptyAndCorrupt(t *testing.T) {
	ctx := context.Background()
	st, _ := dir.New(t.TempDir())
	c := New(st, 0)
	chunks, size, err := c.WriteFile(ctx, bytes.NewReader(nil))
	if err != nil || size != 0 || len(chunks) != 0 {
		t.Fatalf("empty: %v %d %d", err, size, len(chunks))
	}
	id, _ := c.Put(ctx, []byte("some text that compresses some text that compresses"))
	st.Put(ctx, Key(id), []byte{encRaw, 'x'})
	if _, err := c.Get(ctx, id); err == nil {
		t.Fatal("corrupt chunk was returned")
	}
}
