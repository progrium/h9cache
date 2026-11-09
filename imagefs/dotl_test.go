package imagefs

import (
	"errors"
	"net"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/hugelgupf/p9/linux"
	"github.com/hugelgupf/p9/p9"

	"github.com/progrium/h9reg/cas"
	"github.com/progrium/h9reg/ninep"
	"github.com/progrium/h9reg/pull"
	"github.com/progrium/h9reg/store/dir"
	"github.com/progrium/h9reg/tree"
)

// TestServeDotL serves the test image to an independent 9P2000.L client,
// hugelgupf/p9, which comes from gVisor's.
func TestServeDotL(t *testing.T) {
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
	c, err := p9.NewClient(b, p9.WithMessageSize(8192+ninep.IOHdrSz))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	root, err := c.Attach(ref)
	if err != nil {
		t.Fatal(err)
	}

	walk := func(path ...string) p9.File {
		t.Helper()
		_, f, err := root.Walk(path)
		if err != nil {
			t.Fatalf("walk %v: %v", path, err)
		}
		t.Cleanup(func() { f.Close() })
		return f
	}
	attr := func(f p9.File) (p9.QID, p9.Attr) {
		t.Helper()
		q, _, a, err := f.GetAttr(p9.AttrMaskAll)
		if err != nil {
			t.Fatal(err)
		}
		return q, a
	}

	// A symlink is a symlink, with its target as the layer gave it.
	sh := walk("bin", "sh")
	q, at := attr(sh)
	if !at.Mode.IsSymlink() || q.Type&p9.TypeSymlink == 0 || at.Size != uint64(len("/bin/busybox")) {
		t.Fatalf("bin/sh: %v %v", q, at)
	}
	if target, err := sh.Readlink(); err != nil || target != "/bin/busybox" {
		t.Fatalf("readlink bin/sh: %q %v", target, err)
	}
	// A walk stops at a symlink, as a partial walk: the client follows it.
	if qids, _, err := root.Walk([]string{"lib64", "busybox"}); err != nil || len(qids) != 1 || qids[0].Type&p9.TypeSymlink == 0 {
		t.Fatalf("walk through a symlink: %v %v", qids, err)
	}
	if target, err := walk("bin", "gone").Readlink(); err != nil || target != "/nowhere" {
		t.Fatalf("dangling link: %q %v", target, err)
	}

	// Owners are numbers, and devices carry their numbers.
	if _, a := attr(walk("home", "nobody")); a.UID != 65534 || a.GID != 65534 || a.Mode.Permissions() != 0o644 {
		t.Fatalf("home/nobody: %v", a)
	}
	if _, a := attr(walk("dev", "null")); !a.Mode.IsCharacterDevice() || a.RDev != 1<<8|3 {
		t.Fatalf("dev/null: %v", a)
	}

	// Reading a file.
	bb := walk("bin", "busybox")
	if _, _, err := bb.Open(p9.ReadOnly); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 64)
	if n, err := bb.ReadAt(buf, 0); string(buf[:n]) != "#!busybox" {
		t.Fatalf("read busybox: %q %v", buf[:n], err)
	}

	// Writes are refused.
	if _, _, err := walk("home", "nobody").Open(p9.WriteOnly); !errors.Is(err, linux.EROFS) {
		t.Fatalf("open for write: %v", err)
	}
	if _, err := walk("home").Mkdir("new", 0o755, 0, 0); !errors.Is(err, linux.EROFS) {
		t.Fatalf("mkdir: %v", err)
	}
	if _, _, err := root.Walk([]string{"nope"}); !errors.Is(err, linux.ENOENT) {
		t.Fatalf("walk to a missing name: %v", err)
	}

	// A listing shows links as links, dangling ones too, in small reads.
	bin := walk("bin")
	if _, _, err := bin.Open(p9.ReadOnly); err != nil {
		t.Fatal(err)
	}
	var names []string
	off := uint64(0)
	for {
		ents, err := bin.Readdir(off, 64)
		if err != nil {
			t.Fatal(err)
		}
		if len(ents) == 0 {
			break
		}
		for _, e := range ents {
			names = append(names, e.Name)
			off = e.Offset
		}
	}
	sort.Strings(names)
	if got := strings.Join(names, " "); got != ". .. busybox gone sh" {
		t.Fatalf("bin lists %q", got)
	}

	// The root lists the server's own files.
	rf := walk()
	rf.Open(p9.ReadOnly)
	ents, err := rf.Readdir(0, 4096)
	if err != nil {
		t.Fatal(err)
	}
	var top []string
	for _, e := range ents {
		top = append(top, e.Name)
	}
	if s := strings.Join(top, " "); !strings.Contains(s, ".h9index") || !strings.Contains(s, "lib64") {
		t.Fatalf("root lists %q", s)
	}
	if _, err := walk(".h9index").StatFS(); err != nil {
		t.Fatalf("statfs: %v", err)
	}
}
