package imagefs

import (
	"archive/tar"
	"bytes"
	"io"
	"log"
	"net"
	"net/http/httptest"
	"strings"
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
	"github.com/progrium/h9reg/ninep"
	"github.com/progrium/h9reg/pull"
	"github.com/progrium/h9reg/store/dir"
	"github.com/progrium/h9reg/tree"
)

func pushImage(t *testing.T) string {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	add := func(h *tar.Header, body string) {
		h.Size = int64(len(body))
		h.ModTime = time.Unix(1700000000, 0)
		if h.Mode == 0 {
			h.Mode = 0o644
		}
		tw.WriteHeader(h)
		tw.Write([]byte(body))
	}
	add(&tar.Header{Name: "bin/", Typeflag: tar.TypeDir, Mode: 0o755}, "")
	add(&tar.Header{Name: "bin/busybox", Typeflag: tar.TypeReg, Mode: 0o755}, "#!busybox")
	add(&tar.Header{Name: "bin/sh", Typeflag: tar.TypeSymlink, Linkname: "/bin/busybox"}, "")
	add(&tar.Header{Name: "bin/gone", Typeflag: tar.TypeSymlink, Linkname: "/nowhere"}, "")
	add(&tar.Header{Name: "etc/passwd", Typeflag: tar.TypeReg}, "root:x:0:0::/root:/bin/sh\nnobody:x:65534:65534::/:/bin/false\n")
	add(&tar.Header{Name: "etc/group", Typeflag: tar.TypeReg}, "root:x:0:\nnogroup:x:65534:\n")
	add(&tar.Header{Name: "home/nobody", Typeflag: tar.TypeReg, Uid: 65534, Gid: 65534}, "hi")
	add(&tar.Header{Name: "home/stranger", Typeflag: tar.TypeReg, Uid: 1234, Gid: 1234}, "")
	add(&tar.Header{Name: "dev/null", Typeflag: tar.TypeChar, Devmajor: 1, Devminor: 3}, "")
	tw.Close()
	b := buf.Bytes()
	l, err := tarball.LayerFromOpener(func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(b)), nil })
	if err != nil {
		t.Fatal(err)
	}
	img, _ := mutate.AppendLayers(empty.Image, l)
	img, _ = mutate.ConfigFile(img, &v1.ConfigFile{OS: "linux", Architecture: "amd64"})

	srv := httptest.NewServer(registry.New(registry.Logger(log.New(io.Discard, "", 0))))
	t.Cleanup(srv.Close)
	ref := strings.TrimPrefix(srv.URL, "http://") + "/test/img:1"
	tag, _ := name.NewTag(ref)
	if err := remote.Write(tag, img); err != nil {
		t.Fatal(err)
	}
	return ref
}

func TestServeImage(t *testing.T) {
	ref := pushImage(t)
	st, _ := dir.New(t.TempDir())
	chunks := cas.New(st, 1<<20)
	fs := &FS{
		Puller: &pull.Puller{Store: st, CAS: chunks, Platform: pull.DefaultPlatform(), TagTTL: time.Hour},
		CAS:    chunks,
		Trees:  tree.NewCache(st, 100),
	}
	a, b := net.Pipe()
	go (&ninep.Server{FS: fs}).ServeConn(a)
	c, err := ninep.NewClient(b, 8192+ninep.IOHdrSz)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	if _, err := c.RPC(&ninep.Fcall{Type: ninep.Tattach, Fid: 0, Afid: ninep.NoFid, Aname: ""}); err == nil {
		t.Fatal("empty attach spec accepted")
	}
	if _, err := c.RPC(&ninep.Fcall{Type: ninep.Tattach, Fid: 0, Afid: ninep.NoFid, Aname: ref}); err != nil {
		t.Fatal(err)
	}

	stat := func(path ...string) ninep.Dir {
		t.Helper()
		if _, err := c.RPC(&ninep.Fcall{Type: ninep.Twalk, Fid: 0, Newfid: 9, Wname: path}); err != nil {
			t.Fatalf("walk %v: %v", path, err)
		}
		defer c.RPC(&ninep.Fcall{Type: ninep.Tclunk, Fid: 9})
		r, err := c.RPC(&ninep.Fcall{Type: ninep.Tstat, Fid: 9})
		if err != nil {
			t.Fatal(err)
		}
		d, _, err := ninep.UnmarshalDir(r.Stat)
		if err != nil {
			t.Fatal(err)
		}
		return d
	}

	// A symlink stats as its target under its own name, with the target's qid.
	sh, bb := stat("bin", "sh"), stat("bin", "busybox")
	if sh.Name != "sh" || sh.Length != 9 || sh.Mode != 0o755 || sh.Qid != bb.Qid {
		t.Fatalf("bin/sh: %+v (busybox %+v)", sh, bb)
	}
	if d := stat("home", "nobody"); d.UID != "nobody" || d.GID != "nogroup" {
		t.Fatalf("owner names: %+v", d)
	}
	if d := stat("home", "stranger"); d.UID != "1234" || d.GID != "1234" {
		t.Fatalf("unknown owner: %+v", d)
	}
	if d := stat("mnt", "term", "cwd"); d.Mode&ninep.DMDIR == 0 {
		t.Fatalf("mount point: %+v", d)
	}

	// Reading through the link gives busybox's bytes.
	c.RPC(&ninep.Fcall{Type: ninep.Twalk, Fid: 0, Newfid: 1, Wname: []string{"bin", "sh"}})
	if _, err := c.RPC(&ninep.Fcall{Type: ninep.Topen, Fid: 1, Mode: ninep.OEXEC}); err != nil {
		t.Fatal(err)
	}
	if data, err := c.ReadAll(1, 0); err != nil || string(data) != "#!busybox" {
		t.Fatalf("read bin/sh: %q %v", data, err)
	}

	// Device nodes are names only.
	c.RPC(&ninep.Fcall{Type: ninep.Twalk, Fid: 0, Newfid: 2, Wname: []string{"dev", "null"}})
	if _, err := c.RPC(&ninep.Fcall{Type: ninep.Topen, Fid: 2, Mode: ninep.OREAD}); err == nil {
		t.Fatal("device node opened")
	}

	// A listing follows links and leaves out dangling ones.
	c.RPC(&ninep.Fcall{Type: ninep.Twalk, Fid: 0, Newfid: 3, Wname: []string{"bin"}})
	c.RPC(&ninep.Fcall{Type: ninep.Topen, Fid: 3, Mode: ninep.OREAD})
	data, err := c.ReadAll(3, 0)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for len(data) > 0 {
		var d ninep.Dir
		d, data, _ = ninep.UnmarshalDir(data)
		names = append(names, d.Name)
		if d.Name == "sh" && d.Length != 9 {
			t.Fatalf("listed sh: %+v", d)
		}
	}
	if strings.Join(names, " ") != "busybox sh" {
		t.Fatalf("bin lists %v", names)
	}
}
