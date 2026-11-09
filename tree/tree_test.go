package tree

import (
	"context"
	"errors"
	"path"
	"sort"
	"strings"
	"testing"

	"github.com/progrium/h9cache/cas"
	"github.com/progrium/h9cache/store/dir"
)

// build makes a layer tree from "path kind [target]" lines. Directories
// along each path are created.
func build(t *testing.T, lines ...string) *Node {
	root := NewDir(Entry{Mode: 0o755})
	for _, l := range lines {
		f := strings.Fields(l)
		p, kind := f[0], Kind(f[1])
		parent := root
		parts := strings.Split(strings.Trim(p, "/"), "/")
		for _, d := range parts[:len(parts)-1] {
			c := parent.Children[d]
			if c == nil {
				c = NewDir(Entry{Name: d, Mode: 0o755})
				parent.Children[d] = c
			}
			parent = c
		}
		name := parts[len(parts)-1]
		e := Entry{Name: name, Kind: kind, Mode: 0o644}
		switch kind {
		case Dir:
			n := parent.Children[name]
			if n == nil {
				n = NewDir(e)
				parent.Children[name] = n
			}
			if len(f) > 2 && f[2] == "opaque" {
				n.Opaque = true
			}
			continue
		case Symlink, Hardlink:
			e.Target = f[2]
		case File:
			if len(f) > 2 {
				e.Chunks = []cas.Chunk{{ID: strings.Repeat(f[2], 64)[:64], Size: 1}}
				e.Size = 1
			}
		}
		parent.Children[name] = &Node{Entry: e}
	}
	return root
}

func listing(n *Node, dir string, out *[]string) {
	for name, c := range n.Children {
		p := path.Join(dir, name)
		s := p + " " + string(c.Kind)
		if c.Link != "" {
			s += " link=" + c.Link
		}
		*out = append(*out, s)
		if c.Kind == Dir {
			listing(c, p, out)
		}
	}
	sort.Strings(*out)
}

func dump(n *Node) string {
	var out []string
	listing(n, "/", &out)
	return strings.Join(out, "\n")
}

func TestMerge(t *testing.T) {
	base := NewDir(Entry{Mode: 0o755})
	Merge(base, build(t,
		"/bin/sh file",
		"/bin/ls file",
		"/etc/conf file",
		"/var/cache/a file",
		"/var/cache/b file",
		"/opt/x file",
	))
	Merge(base, build(t,
		"/bin/.keep whiteout", // whiteout of a missing name is harmless
		"/bin/ls whiteout",
		"/var/cache dir opaque",
		"/var/cache/c file",
		"/opt file", // a file replaces a directory
		"/etc/conf dir",
		"/etc/conf/d file",
		"/bin/ln hardlink /bin/sh",
		"/bin/bad hardlink /nope",
	))
	want := strings.Join([]string{
		"/bin dir",
		"/bin/ln file link=/bin/sh",
		"/bin/sh file",
		"/etc dir",
		"/etc/conf dir",
		"/etc/conf/d file",
		"/opt file",
		"/var dir",
		"/var/cache dir",
		"/var/cache/c file",
	}, "\n")
	if got := dump(base); got != want {
		t.Fatalf("merged tree:\n%s\nwant:\n%s", got, want)
	}
}

func TestWriteLoadDedup(t *testing.T) {
	ctx := context.Background()
	st, _ := dir.New(t.TempDir())
	a := build(t, "/usr/lib/libc file a", "/usr/bin/sh file b", "/etc/a file c")
	b := build(t, "/usr/lib/libc file a", "/usr/bin/sh file b", "/etc/b file c")
	ida, err := Write(ctx, st, a)
	if err != nil {
		t.Fatal(err)
	}
	keysA, _ := st.List(ctx, "trees/")
	idb, _ := Write(ctx, st, b)
	keysB, _ := st.List(ctx, "trees/")
	if ida == idb {
		t.Fatal("different trees got one id")
	}
	// Only the root and /etc differ; /usr and below are shared.
	if len(keysB)-len(keysA) != 2 {
		t.Fatalf("second tree added %d objects, want 2", len(keysB)-len(keysA))
	}
	n, err := Load(ctx, st, ida, Entry{})
	if err != nil {
		t.Fatal(err)
	}
	if dump(n) != dump(a) {
		t.Fatalf("loaded:\n%s\nwant:\n%s", dump(n), dump(a))
	}
}

func TestImageWalk(t *testing.T) {
	ctx := context.Background()
	st, _ := dir.New(t.TempDir())
	root := NewDir(Entry{Mode: 0o755})
	Merge(root, build(t,
		"/bin/busybox file a",
		"/bin/sh symlink /bin/busybox",
		"/bin/ash symlink busybox",
		"/lib/ld file b",
		"/lib64 symlink lib",
		"/usr/lib symlink ../lib",
		"/up symlink ../../../bin/sh",
		"/loop symlink loop",
		"/dangling symlink /missing",
		"/etc/f file c",
		"/etc/hl hardlink /etc/f",
	))
	id, err := Write(ctx, st, root)
	if err != nil {
		t.Fatal(err)
	}
	img := &Image{Root: Entry{Kind: Dir, Mode: 0o755, Tree: id}, Cache: NewCache(st, 100)}

	for _, tc := range []struct {
		in, want string
		err      error
	}{
		{"bin/sh", "/bin/busybox", nil},
		{"bin/ash", "/bin/busybox", nil},
		{"lib64/ld", "/lib/ld", nil},
		{"usr/lib/ld", "/lib/ld", nil},
		{"usr/lib/..", "/", nil}, // .. after a link is from the target
		{"up", "/bin/busybox", nil},
		{"..", "/", nil},
		{"loop", "", ErrLoop},
		{"dangling", "", ErrNotExist},
		{"etc/f/x", "", ErrNotDir},
	} {
		p, err := img.WalkPath(ctx, tc.in)
		if !errors.Is(err, tc.err) {
			t.Fatalf("%s: err %v, want %v", tc.in, err, tc.err)
		}
		if err == nil && p.String() != tc.want {
			t.Fatalf("%s: got %s, want %s", tc.in, p, tc.want)
		}
	}

	sh, _ := img.WalkPath(ctx, "bin/sh")
	bb, _ := img.WalkPath(ctx, "bin/busybox")
	if img.QidPath(sh) != img.QidPath(bb) {
		t.Fatal("symlink and target have different qids")
	}
	f, _ := img.WalkPath(ctx, "etc/f")
	hl, _ := img.WalkPath(ctx, "etc/hl")
	if img.QidPath(f) != img.QidPath(hl) {
		t.Fatal("hardlinked names have different qids")
	}
	if img.QidPath(f) == img.QidPath(bb) {
		t.Fatal("different files share a qid")
	}
}
