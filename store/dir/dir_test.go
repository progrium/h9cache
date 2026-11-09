package dir

import (
	"context"
	"errors"
	"reflect"
	"sync"
	"testing"

	"github.com/progrium/h9reg/store"
)

func TestStore(t *testing.T) {
	ctx := context.Background()
	s, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Get(ctx, "a/b"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("get missing: %v", err)
	}
	if err := s.Put(ctx, "a/b", []byte("hello world")); err != nil {
		t.Fatal(err)
	}
	if b, _ := s.Get(ctx, "a/b"); string(b) != "hello world" {
		t.Fatalf("get: %q", b)
	}
	if b, _ := s.GetRange(ctx, "a/b", 6, 100); string(b) != "world" {
		t.Fatalf("range: %q", b)
	}
	if ok, _ := s.Has(ctx, "a/b"); !ok {
		t.Fatal("has")
	}
	s.Put(ctx, "a/c", nil)
	s.Put(ctx, "b/d", nil)
	keys, _ := s.List(ctx, "a/")
	if !reflect.DeepEqual(keys, []string{"a/b", "a/c"}) {
		t.Fatalf("list: %v", keys)
	}
	if err := s.Delete(ctx, "a/b"); err != nil {
		t.Fatal(err)
	}
	if err := s.Delete(ctx, "a/b"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Get(ctx, "../x"); err == nil {
		t.Fatal("escaping key accepted")
	}
}

func TestPutIfAbsent(t *testing.T) {
	ctx := context.Background()
	s, _ := New(t.TempDir())
	var wg sync.WaitGroup
	var mu sync.Mutex
	wins := 0
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ok, err := s.PutIfAbsent(ctx, "claim/x", []byte("me"))
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
}
