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
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/tarball"

	"github.com/progrium/h9reg/cas"
	"github.com/progrium/h9reg/store/dir"
	"github.com/progrium/h9reg/tree"
)

// TarEntry describes one member of a test layer.
type TarEntry struct {
	Name, Body, Link string
	Type             byte
}

func layer(t *testing.T, entries ...TarEntry) v1.Layer {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, e := range entries {
		hdr := &tar.Header{Name: e.Name, Typeflag: e.Type, Linkname: e.Link, Mode: 0o644, ModTime: time.Unix(1700000000, 0)}
		if e.Type == 0 {
			hdr.Typeflag = tar.TypeReg
		}
		if hdr.Typeflag == tar.TypeDir {
			hdr.Mode = 0o755
		}
		hdr.Size = int64(len(e.Body))
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatal(err)
		}
		tw.Write([]byte(e.Body))
	}
	tw.Close()
	b := buf.Bytes()
	l, err := tarball.LayerFromOpener(func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(b)), nil })
	if err != nil {
		t.Fatal(err)
	}
	return l
}

func testImage(t *testing.T, arch string) v1.Image {
	base := layer(t,
		TarEntry{Name: "bin/", Type: tar.TypeDir},
		TarEntry{Name: "bin/busybox", Body: "busybox-" + arch},
		TarEntry{Name: "bin/sh", Type: tar.TypeSymlink, Link: "busybox"},
		TarEntry{Name: "etc/passwd", Body: "root:x:0:0:root:/root:/bin/sh\n"},
		TarEntry{Name: "etc/old", Body: "old"},
		TarEntry{Name: "var/cache/x", Body: "x"},
	)
	top := layer(t,
		TarEntry{Name: "etc/.wh.old"},
		TarEntry{Name: "var/cache/.wh..wh..opq"},
		TarEntry{Name: "var/cache/y", Body: "y"},
		TarEntry{Name: "bin/ln", Type: tar.TypeLink, Link: "bin/busybox"},
	)
	img, err := mutate.AppendLayers(empty.Image, base, top)
	if err != nil {
		t.Fatal(err)
	}
	img, err = mutate.ConfigFile(img, &v1.ConfigFile{OS: "linux", Architecture: arch,
		Config: v1.Config{Env: []string{"PATH=/bin"}, Cmd: []string{"/bin/sh"}}})
	if err != nil {
		t.Fatal(err)
	}
	return img
}

func TestPull(t *testing.T) {
	ctx := context.Background()
	var manifestGets, heads atomic.Int32
	reg := registry.New(registry.Logger(log.New(io.Discard, "", 0)))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/manifests/") {
			if r.Method == http.MethodHead {
				heads.Add(1)
			} else {
				manifestGets.Add(1)
			}
		}
		reg.ServeHTTP(w, r)
	}))
	defer srv.Close()
	host := strings.TrimPrefix(srv.URL, "http://")

	idx := mutate.AppendManifests(empty.Index,
		mutate.IndexAddendum{Add: testImage(t, "amd64"), Descriptor: v1.Descriptor{Platform: &v1.Platform{OS: "linux", Architecture: "amd64"}}},
		mutate.IndexAddendum{Add: testImage(t, "arm64"), Descriptor: v1.Descriptor{Platform: &v1.Platform{OS: "linux", Architecture: "arm64", Variant: "v8"}}},
	)
	tag, _ := name.NewTag(host + "/library/test:1")
	if err := remote.WriteIndex(tag, idx); err != nil {
		t.Fatal(err)
	}
	heads.Store(0)
	manifestGets.Store(0)

	st, _ := dir.New(t.TempDir())
	p := &Puller{Store: st, CAS: cas.New(st, 1<<20), Platform: v1.Platform{OS: "linux", Architecture: "amd64"}, TagTTL: time.Hour}

	spec, err := ParseSpec(host + "/library/test:1")
	if err != nil {
		t.Fatal(err)
	}
	img, err := p.Resolve(ctx, spec)
	if err != nil {
		t.Fatal(err)
	}
	if img.Platform != "linux/amd64" || img.Config.Config.Cmd[0] != "/bin/sh" {
		t.Fatalf("image record: %+v", img)
	}
	ti := &tree.Image{Root: img.Root, Cache: tree.NewCache(st, 100)}
	for _, tc := range []struct{ path, want string }{
		{"bin/sh", "/bin/busybox"},
		{"bin/ln", "/bin/ln"},
		{"var/cache/y", "/var/cache/y"},
		{"etc/passwd", "/etc/passwd"},
	} {
		got, err := ti.WalkPath(ctx, tc.path)
		if err != nil || got.String() != tc.want {
			t.Fatalf("%s: %v %v", tc.path, got, err)
		}
	}
	for _, gone := range []string{"etc/old", "var/cache/x"} {
		if _, err := ti.WalkPath(ctx, gone); err == nil {
			t.Fatalf("%s survived its whiteout", gone)
		}
	}
	bb, _ := ti.WalkPath(ctx, "bin/busybox")
	ln, _ := ti.WalkPath(ctx, "bin/ln")
	if ti.QidPath(bb) != ti.QidPath(ln) {
		t.Fatal("hardlink has its own qid")
	}
	buf := make([]byte, 64)
	n, _ := p.CAS.ReadAt(ctx, ln.Entry().Chunks, buf, 0)
	if string(buf[:n]) != "busybox-amd64" {
		t.Fatalf("hardlink content %q", buf[:n])
	}

	// A warm resolve within the TTL makes no requests.
	heads.Store(0)
	manifestGets.Store(0)
	img2, err := p.Resolve(ctx, spec)
	if err != nil || img2.Tree != img.Tree {
		t.Fatalf("warm resolve: %v", err)
	}
	if heads.Load() != 0 || manifestGets.Load() != 0 {
		t.Fatalf("warm resolve made %d HEADs and %d GETs", heads.Load(), manifestGets.Load())
	}

	// Past the TTL, a resolve checks the tag with HEAD only.
	p.TagTTL = 0
	if _, err := p.Resolve(ctx, spec); err != nil {
		t.Fatal(err)
	}
	if heads.Load() != 1 || manifestGets.Load() != 0 {
		t.Fatalf("expired resolve made %d HEADs and %d GETs", heads.Load(), manifestGets.Load())
	}

	// Another platform of the same index shares nothing but works.
	arm, _ := ParseSpec(host + "/library/test:1!linux/arm64")
	armImg, err := p.Resolve(ctx, arm)
	if err != nil {
		t.Fatal(err)
	}
	if armImg.Tree == img.Tree || armImg.Platform != "linux/arm64/v8" {
		t.Fatalf("arm64 image: %+v", armImg)
	}

	// The upper layer is identical across platforms, so it converted once.
	layers, _ := st.List(ctx, "layers/")
	if len(layers) != 3 {
		t.Fatalf("%d layer records, want 3", len(layers))
	}
}

func TestParseSpec(t *testing.T) {
	for _, tc := range []struct{ in, ref, plat string }{
		{"alpine", "index.docker.io/library/alpine:latest", ""},
		{"alpine:3.20", "index.docker.io/library/alpine:3.20", ""},
		{"ghcr.io/a/b:c!linux/arm64/v8", "ghcr.io/a/b:c", "linux/arm64/v8"},
	} {
		s, err := ParseSpec(tc.in)
		if err != nil {
			t.Fatal(err)
		}
		if s.Ref.Name() != tc.ref {
			t.Fatalf("%s: ref %s", tc.in, s.Ref.Name())
		}
		if (s.Platform == nil) != (tc.plat == "") || (s.Platform != nil && s.Platform.String() != tc.plat) {
			t.Fatalf("%s: platform %v", tc.in, s.Platform)
		}
	}
	for _, bad := range []string{"", "!linux/amd64", "alpine!linux", "UPPER"} {
		if _, err := ParseSpec(bad); err == nil {
			t.Fatalf("%q accepted", bad)
		}
	}
}
