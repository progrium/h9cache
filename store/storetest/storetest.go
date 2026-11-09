// Package storetest checks that a store.Store behaves as h9reg expects.
// Every backend runs it.
package storetest

import (
	"context"
	"errors"
	"reflect"
	"sync"
	"testing"

	"github.com/progrium/h9reg/store"
)

// Run checks st, which must start empty.
func Run(t *testing.T, st store.Store) {
	ctx := context.Background()
	t.Run("GetPutHas", func(t *testing.T) {
		if _, err := st.Get(ctx, "a/missing"); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("get missing: %v", err)
		}
		if ok, err := st.Has(ctx, "a/missing"); ok || err != nil {
			t.Fatalf("has missing: %v %v", ok, err)
		}
		if err := st.Put(ctx, "a/b", []byte("hello world")); err != nil {
			t.Fatal(err)
		}
		if b, err := st.Get(ctx, "a/b"); err != nil || string(b) != "hello world" {
			t.Fatalf("get: %q %v", b, err)
		}
		if ok, err := st.Has(ctx, "a/b"); !ok || err != nil {
			t.Fatalf("has: %v %v", ok, err)
		}
		if err := st.Put(ctx, "a/b", []byte("replaced")); err != nil {
			t.Fatal(err)
		}
		if b, _ := st.Get(ctx, "a/b"); string(b) != "replaced" {
			t.Fatalf("after replace: %q", b)
		}
		if err := st.Put(ctx, "a/empty", nil); err != nil {
			t.Fatal(err)
		}
		if b, err := st.Get(ctx, "a/empty"); err != nil || len(b) != 0 {
			t.Fatalf("empty object: %q %v", b, err)
		}
	})
	t.Run("GetRange", func(t *testing.T) {
		st.Put(ctx, "r/x", []byte("0123456789"))
		for _, tc := range []struct {
			off, n int64
			want   string
		}{{0, 3, "012"}, {6, 4, "6789"}, {6, 100, "6789"}, {10, 5, ""}, {20, 5, ""}} {
			b, err := st.GetRange(ctx, "r/x", tc.off, tc.n)
			if err != nil || string(b) != tc.want {
				t.Fatalf("range %d+%d: %q %v, want %q", tc.off, tc.n, b, err, tc.want)
			}
		}
		if _, err := st.GetRange(ctx, "r/missing", 0, 1); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("range of missing: %v", err)
		}
	})
	t.Run("List", func(t *testing.T) {
		for _, k := range []string{"l/a/1", "l/a/2", "l/b/1", "lx"} {
			st.Put(ctx, k, []byte(k))
		}
		keys, err := st.List(ctx, "l/a/")
		if err != nil || !reflect.DeepEqual(keys, []string{"l/a/1", "l/a/2"}) {
			t.Fatalf("list l/a/: %v %v", keys, err)
		}
		keys, _ = st.List(ctx, "l/")
		if len(keys) != 3 {
			t.Fatalf("list l/: %v", keys)
		}
		keys, err = st.List(ctx, "nothing/")
		if err != nil || len(keys) != 0 {
			t.Fatalf("list of nothing: %v %v", keys, err)
		}
	})
	t.Run("Delete", func(t *testing.T) {
		st.Put(ctx, "d/x", []byte("x"))
		if err := st.Delete(ctx, "d/x"); err != nil {
			t.Fatal(err)
		}
		if ok, _ := st.Has(ctx, "d/x"); ok {
			t.Fatal("deleted object still there")
		}
		if err := st.Delete(ctx, "d/x"); err != nil {
			t.Fatalf("delete of missing: %v", err)
		}
	})
	t.Run("PutIfAbsent", func(t *testing.T) {
		var wg sync.WaitGroup
		var mu sync.Mutex
		wins := 0
		for i := 0; i < 16; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				ok, err := st.PutIfAbsent(ctx, "claim/x", []byte("me"))
				if err != nil {
					t.Error(err)
				}
				if ok {
					mu.Lock()
					wins++
					mu.Unlock()
				}
			}()
		}
		wg.Wait()
		if wins != 1 {
			t.Fatalf("%d writers won the claim", wins)
		}
		if ok, err := st.PutIfAbsent(ctx, "claim/x", []byte("again")); ok || err != nil {
			t.Fatalf("second claim: %v %v", ok, err)
		}
	})
}
