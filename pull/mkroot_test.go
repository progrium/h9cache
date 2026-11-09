package pull

import (
	"archive/tar"
	"bytes"
	"context"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/registry"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/remote"

	"github.com/progrium/h9cache/cas"
	"github.com/progrium/h9cache/store/dir"
)

func TestMkrootCandidate(t *testing.T) {
	for _, tc := range []struct{ spec, want string }{
		{"progrium/9front", "ghcr.io/progrium/mkroot-9front:latest"},
		{"progrium/9front:11952", "ghcr.io/progrium/mkroot-9front:11952"},
		{"ghcr.io/progrium/9front:11952", "ghcr.io/progrium/mkroot-9front:11952"},
		{"ghcr.io/org/team/9front:1", "ghcr.io/org/team/mkroot-9front:1"},
		{"progrium/9front@sha256:" + strings.Repeat("a", 64), "ghcr.io/progrium/mkroot-9front@sha256:" + strings.Repeat("a", 64)},
		{"ghcr.io/progrium/mkroot-9front:1", ""},
		{"alpine", ""},
		{"library/alpine", ""},
		{"docker.io/progrium/9front", ""},
		{"quay.io/progrium/9front", ""},
		{"ghcr.io/9front", ""},
	} {
		spec, err := ParseSpec(tc.spec)
		if err != nil {
			t.Fatal(err)
		}
		got := ""
		if ref, ok := mkrootCandidate(spec, MkrootRegistry); ok {
			got = ref.String()
		}
		if got != tc.want {
			t.Errorf("%s: got %q, want %q", tc.spec, got, tc.want)
		}
	}
}

func TestLocate(t *testing.T) {
	ctx := context.Background()
	serve := func() (string, *atomic.Int32) {
		var heads atomic.Int32
		reg := registry.New(registry.Logger(log.New(io.Discard, "", 0)))
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodHead {
				heads.Add(1)
			}
			reg.ServeHTTP(w, r)
		}))
		t.Cleanup(srv.Close)
		return strings.TrimPrefix(srv.URL, "http://"), &heads
	}
	ghcr, ghcrHeads := serve()
	hub, _ := serve()
	push := func(ref string) {
		tag, _ := name.NewTag(ref)
		if err := remote.Write(tag, testImage(t, "amd64")); err != nil {
			t.Fatal(err)
		}
	}
	push(ghcr + "/progrium/mkroot-9front:1")
	push(hub + "/acme/app:1")

	st, _ := dir.New(t.TempDir())
	p := &Puller{Store: st, CAS: cas.New(st, 1<<20), Platform: v1.Platform{OS: "linux", Architecture: "amd64"},
		TagTTL: time.Hour, Mkroot: true, MkrootRegistry: ghcr}
	locate := func(s string) string {
		t.Helper()
		spec, err := ParseSpec(s, name.WithDefaultRegistry(hub))
		if err != nil {
			t.Fatal(err)
		}
		spec, err = p.Locate(ctx, spec)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := p.Resolve(ctx, spec); err != nil {
			t.Fatalf("%s: %v", s, err)
		}
		return RepoName(spec.Ref)
	}
	if got := locate("progrium/9front:1"); got != ghcr+"/progrium/mkroot-9front" {
		t.Fatalf("progrium/9front located as %s", got)
	}
	if got := locate(ghcr + "/progrium/9front:1"); got != ghcr+"/progrium/mkroot-9front" {
		t.Fatalf("ghcr progrium/9front located as %s", got)
	}
	if got := locate("acme/app:1"); got != hub+"/acme/app" {
		t.Fatalf("acme/app located as %s", got)
	}
	// Both answers are remembered for TagTTL.
	ghcrHeads.Store(0)
	locate("progrium/9front:1")
	locate("acme/app:1")
	if n := ghcrHeads.Load(); n != 0 {
		t.Fatalf("%d lookups within the TTL", n)
	}
	// Without Mkroot, references are pulled as written.
	p.Mkroot = false
	spec, _ := ParseSpec("progrium/9front:1", name.WithDefaultRegistry(hub))
	if spec, _ = p.Locate(ctx, spec); RepoName(spec.Ref) != hub+"/progrium/9front" {
		t.Fatalf("Mkroot off located %s", RepoName(spec.Ref))
	}
}

func TestPlan9Fallback(t *testing.T) {
	plats := candidatePlatforms(v1.Platform{OS: "linux", Architecture: "amd64"})
	if len(plats) != 2 || plats[1].OS != "plan9" || plats[1].Architecture != "amd64" {
		t.Fatalf("%v", plats)
	}
}

func TestPlan9Owners(t *testing.T) {
	for _, tc := range []struct {
		hdr  tar.Header
		want bool
	}{
		{tar.Header{Uname: "glenda", Gname: "sys"}, true},
		{tar.Header{Uname: "root", Gname: "root"}, false},
		{tar.Header{}, false},
		{tar.Header{Uid: 1000, Uname: "jeff"}, false},
	} {
		if plan9Owners(&tc.hdr) != tc.want {
			t.Errorf("%+v: got %v", tc.hdr, !tc.want)
		}
	}
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	tw.WriteHeader(&tar.Header{Name: "lib/", Typeflag: tar.TypeDir, Mode: 0o775, Uname: "sys", Gname: "sys"})
	tw.WriteHeader(&tar.Header{Name: "usr/glenda/lib/profile", Typeflag: tar.TypeReg, Mode: 0o664, Uname: "glenda", Gname: "glenda"})
	tw.Close()
	st, _ := dir.New(t.TempDir())
	n, err := ConvertLayer(context.Background(), cas.New(st, 1<<20).NewBatch(), &buf)
	if err != nil {
		t.Fatal(err)
	}
	p := n.Children["usr"].Children["glenda"].Children["lib"].Children["profile"]
	if p.User != "glenda" || p.Group != "glenda" || n.Children["lib"].User != "sys" {
		t.Fatalf("owners: %+v", p.Entry)
	}
}
