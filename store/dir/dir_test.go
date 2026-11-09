package dir

import (
	"context"
	"testing"

	"github.com/progrium/h9reg/store/storetest"
)

func TestStore(t *testing.T) {
	s, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	storetest.Run(t, s)
}

func TestBadKeys(t *testing.T) {
	s, _ := New(t.TempDir())
	for _, k := range []string{"../x", "/x", "a//b", ".tmp/x", ""} {
		if _, err := s.Get(context.Background(), k); err == nil {
			t.Fatalf("key %q accepted", k)
		}
	}
}
