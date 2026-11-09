package pull

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/registry"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/remote"

	"github.com/progrium/h9cache/cas"
	"github.com/progrium/h9cache/store"
	"github.com/progrium/h9cache/store/dir"
)

// slowRegistry serves one image whose blob downloads are slow enough that
// concurrent pulls overlap, and counts them.
func slowRegistry(t *testing.T) (host string, blobGets *atomic.Int32) {
	blobGets = new(atomic.Int32)
	reg := registry.New(registry.Logger(log.New(io.Discard, "", 0)))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/blobs/") {
			blobGets.Add(1)
			time.Sleep(100 * time.Millisecond)
		}
		reg.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	host = strings.TrimPrefix(srv.URL, "http://")
	tag, _ := name.NewTag(host + "/library/test:1")
	if err := remote.Write(tag, testImage(t, "amd64")); err != nil {
		t.Fatal(err)
	}
	blobGets.Store(0)
	return host, blobGets
}

func instance(st store.Store, id string) *Puller {
	return &Puller{Store: st, CAS: cas.New(st, 1<<20), Platform: v1.Platform{OS: "linux", Architecture: "amd64"},
		TagTTL: time.Hour, Instance: id, PollInterval: 20 * time.Millisecond}
}

func TestClaimSharesPull(t *testing.T) {
	ctx := context.Background()
	host, blobGets := slowRegistry(t)
	st, _ := dir.New(t.TempDir())
	spec, _ := ParseSpec(host + "/library/test:1")

	// Four instances on one store, each attached by several clients.
	var wg sync.WaitGroup
	trees := make(chan string, 16)
	for i := range 4 {
		p := instance(st, string(rune('a'+i)))
		for range 4 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				img, err := p.Resolve(ctx, spec)
				if err != nil {
					t.Error(err)
					return
				}
				trees <- img.Tree
			}()
		}
	}
	wg.Wait()
	close(trees)
	first := ""
	for tr := range trees {
		if first == "" {
			first = tr
		} else if tr != first {
			t.Fatal("instances resolved different trees")
		}
	}
	// One pull: the config and two layers.
	if n := blobGets.Load(); n != 3 {
		t.Fatalf("%d blob downloads, want 3", n)
	}
	if left, _ := st.List(ctx, "pulling/"); len(left) != 0 {
		t.Fatalf("claims left behind: %v", left)
	}
}

func TestClaimTakeover(t *testing.T) {
	ctx := context.Background()
	host, _ := slowRegistry(t)
	st, _ := dir.New(t.TempDir())
	spec, _ := ParseSpec(host + "/library/test:1")
	p := instance(st, "b")
	p.ClaimTTL = 300 * time.Millisecond

	// An instance claimed the pull and died.
	top, err := p.resolveRef(ctx, spec.Ref)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(Claim{Instance: "a", Time: time.Now().UTC()})
	if err := st.Put(ctx, claimKey(top, p.Platform), b); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	if _, err := p.Resolve(ctx, spec); err != nil {
		t.Fatal(err)
	}
	if took := time.Since(start); took < p.ClaimTTL {
		t.Fatalf("took over a live claim after %v", took)
	}
	if left, _ := st.List(ctx, "pulling/"); len(left) != 0 {
		t.Fatalf("claims left behind: %v", left)
	}
}

func TestClaimRefresh(t *testing.T) {
	ctx := context.Background()
	st, _ := dir.New(t.TempDir())
	p := instance(st, "a")
	p.ClaimTTL = 100 * time.Millisecond
	key := "pulling/test"
	_, err := p.pullHolding(ctx, key, "a", func(ctx context.Context) (*Image, error) {
		time.Sleep(250 * time.Millisecond)
		var c Claim
		if ok, err := getJSON(ctx, st, key, &c); !ok || err != nil {
			t.Fatalf("claim missing: %v", err)
		}
		if age := time.Since(c.Time); age > p.ClaimTTL {
			t.Fatalf("claim not refreshed: %v old", age)
		}
		return nil, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if ok, _ := st.Has(ctx, key); ok {
		t.Fatal("claim not released")
	}
}
