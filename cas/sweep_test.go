package cas

import (
	"bytes"
	"context"
	"testing"

	"github.com/progrium/h9reg/store/dir"
)

func TestSweepPacks(t *testing.T) {
	ctx := context.Background()
	st, _ := dir.New(t.TempDir())
	w := New(st, 1<<20)
	blob := func(c byte) []byte { return bytes.Repeat([]byte{c, c + 1, c + 2}, 30000) }
	b := w.NewBatch()
	var ids []string
	for _, c := range []byte{'a', 'd', 'g'} {
		id, err := b.Put(ctx, blob(c))
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	if err := b.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	b2 := w.NewBatch()
	dead, _ := b2.Put(ctx, blob('x'))
	b2.Flush(ctx)

	// A reader that loaded the indexes before the sweep.
	r := New(st, 0)
	if err := r.LoadPacks(ctx); err != nil {
		t.Fatal(err)
	}

	live := map[string]bool{ids[0]: true}
	stats, err := SweepPacks(ctx, st, func(id string) bool { return live[id] }, 0.5, 0, true, nil)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Deleted != 1 || stats.Repacked != 1 || stats.NewPacks != 1 {
		t.Fatalf("dry run: %+v", stats)
	}
	if keys, _ := st.List(ctx, "packs/"); len(keys) != 2 {
		t.Fatalf("dry run deleted packs: %v", keys)
	}

	if _, err := SweepPacks(ctx, st, func(id string) bool { return live[id] }, 0.5, 0, false, nil); err != nil {
		t.Fatal(err)
	}
	if keys, _ := st.List(ctx, "packs/"); len(keys) != 1 {
		t.Fatalf("after sweep: %v", keys)
	}
	// The stale reader finds the moved chunk.
	got, err := r.Get(ctx, ids[0])
	if err != nil || !bytes.Equal(got, blob('a')) {
		t.Fatalf("moved chunk: %v", err)
	}
	if _, err := r.Get(ctx, dead); err == nil {
		t.Fatal("dead chunk still readable")
	}
	if _, err := r.Get(ctx, ids[1]); err == nil {
		t.Fatal("unmarked chunk still readable")
	}
}
