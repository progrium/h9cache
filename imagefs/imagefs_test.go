package imagefs

import (
	"archive/tar"
	"bytes"
	"context"
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
	"github.com/progrium/h9reg/index"
	"github.com/progrium/h9reg/ninep"
	"github.com/progrium/h9reg/pull"
	"github.com/progrium/h9reg/store"
	"github.com/progrium/h9reg/store/dir"
	s3store "github.com/progrium/h9reg/store/s3"
	"github.com/progrium/h9reg/store/s3/s3fake"
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
	add(&tar.Header{Name: "lib64", Typeflag: tar.TypeSymlink, Linkname: "bin"}, "")
	add(&tar.Header{Name: "home/odd name", Typeflag: tar.TypeReg}, "")
	tw.Close()
	b := buf.Bytes()
	l, err := tarball.LayerFromOpener(func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(b)), nil })
	if err != nil {
		t.Fatal(err)
	}
	img, _ := mutate.AppendLayers(empty.Image, l)
	img, _ = mutate.ConfigFile(img, &v1.ConfigFile{OS: "linux", Architecture: "amd64",
		Config: v1.Config{Env: []string{"PATH=/bin"}, Cmd: []string{"/bin/sh"}, WorkingDir: "/root"}})

	srv := httptest.NewServer(registry.New(registry.Logger(log.New(io.Discard, "", 0))))
	t.Cleanup(srv.Close)
	ref := strings.TrimPrefix(srv.URL, "http://") + "/test/img:1"
	tag, _ := name.NewTag(ref)
	if err := remote.Write(tag, img); err != nil {
		t.Fatal(err)
	}
	return ref
}

func connect(t *testing.T) (*ninep.Client, string) {
	st, _ := dir.New(t.TempDir())
	return connectStore(t, st)
}

func connectStore(t *testing.T, st store.Store) (*ninep.Client, string) {
	ref := pushImage(t)
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
	t.Cleanup(func() { c.Close() })
	return c, ref
}

func TestServeImage(t *testing.T) {
	c, ref := connect(t)

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

func readFile(t *testing.T, c *ninep.Client, path ...string) []byte {
	t.Helper()
	if _, err := c.RPC(&ninep.Fcall{Type: ninep.Twalk, Fid: 0, Newfid: 50, Wname: path}); err != nil {
		t.Fatalf("walk %v: %v", path, err)
	}
	defer c.RPC(&ninep.Fcall{Type: ninep.Tclunk, Fid: 50})
	if _, err := c.RPC(&ninep.Fcall{Type: ninep.Topen, Fid: 50, Mode: ninep.OREAD}); err != nil {
		t.Fatal(err)
	}
	b, err := c.ReadAll(50, 0)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// Every entry in /.h9index must match what the server says for that path,
// since h9 answers stats from the index and opens from the server.
func TestIndexMatchesServer(t *testing.T) {
	c, ref := connect(t)
	if _, err := c.RPC(&ninep.Fcall{Type: ninep.Tattach, Fid: 0, Afid: ninep.NoFid, Aname: ref}); err != nil {
		t.Fatal(err)
	}
	ix, err := index.Parse(bytes.NewReader(readFile(t, c, ".h9index")))
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, e := range ix.Entries {
		seen[e.Path] = true
		var wname []string
		if e.Path != "/" {
			wname = strings.Split(e.Path[1:], "/")
		}
		if _, err := c.RPC(&ninep.Fcall{Type: ninep.Twalk, Fid: 0, Newfid: 60, Wname: wname}); err != nil {
			t.Fatalf("walk %s: %v", e.Path, err)
		}
		r, err := c.RPC(&ninep.Fcall{Type: ninep.Tstat, Fid: 60})
		c.RPC(&ninep.Fcall{Type: ninep.Tclunk, Fid: 60})
		if err != nil {
			t.Fatal(err)
		}
		d, _, _ := ninep.UnmarshalDir(r.Stat)
		isDir := d.Qid.Type&ninep.QTDIR != 0
		if d.Qid.Path != e.QidPath || d.Qid.Vers != e.QidVers || isDir != (e.QidType == 'd') ||
			d.Mode != e.Mode || d.Length != e.Length || d.Mtime != e.Mtime || d.UID != e.UID || d.GID != e.GID {
			t.Fatalf("%s: index %+v, server %+v", e.Path, e, d)
		}
	}
	for _, p := range []string{"/", "/.h9image", "/bin/sh", "/lib64", "/mnt/term/cwd", "/home/nobody"} {
		if !seen[p] {
			t.Fatalf("%s missing from the index", p)
		}
	}
	for _, p := range []string{"/.h9index", "/bin/gone", "/lib64/busybox", "/home/odd name"} {
		if seen[p] {
			t.Fatalf("%s is in the index", p)
		}
	}
	if strings.Join(ix.Unindexed, " ") != "/home /lib64" {
		t.Fatalf("unindexed: %v", ix.Unindexed)
	}

	img := string(readFile(t, c, ".h9image"))
	for _, line := range []string{"h9image 1", `platform "linux/amd64"`, `cmd ["/bin/sh"]`, `env ["PATH=/bin"]`, `workdir "/root"`, `entrypoint []`, "manifest \"sha256:"} {
		if !strings.Contains(img, line) {
			t.Fatalf(".h9image lacks %q:\n%s", line, img)
		}
	}
}

// The same image served from S3, through packs, reads the same.
func TestServeFromS3(t *testing.T) {
	st, err := s3store.Open(context.Background(), s3fake.URL(t, "cache"))
	if err != nil {
		t.Fatal(err)
	}
	c, ref := connectStore(t, st)
	if _, err := c.RPC(&ninep.Fcall{Type: ninep.Tattach, Fid: 0, Afid: ninep.NoFid, Aname: ref}); err != nil {
		t.Fatal(err)
	}
	if got := string(readFile(t, c, "bin", "sh")); got != "#!busybox" {
		t.Fatalf("bin/sh: %q", got)
	}
	if _, err := index.Parse(bytes.NewReader(readFile(t, c, ".h9index"))); err != nil {
		t.Fatal(err)
	}
	keys, _ := st.List(context.Background(), "packs/")
	if len(keys) == 0 {
		t.Fatal("no packs in the bucket")
	}
}
