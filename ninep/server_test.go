package ninep

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"testing"
	"time"
)

// memFile is a tiny in-memory tree for protocol tests.
type memFile struct {
	name     string
	path     uint64
	data     string
	children []*memFile
	parent   *memFile
	slow     chan struct{} // a read blocks until this closes
}

func (f *memFile) Qid() Qid {
	q := Qid{Path: f.path}
	if f.children != nil {
		q.Type = QTDIR
	}
	return q
}

func (f *memFile) Walk(ctx context.Context, name string) (File, error) {
	if name == ".." {
		if f.parent == nil {
			return f, nil
		}
		return f.parent, nil
	}
	for _, c := range f.children {
		if c.name == name {
			return c, nil
		}
	}
	return nil, errors.New("file does not exist")
}

func (f *memFile) Stat(ctx context.Context) (Dir, error) {
	d := Dir{Qid: f.Qid(), Name: f.name, Mode: 0o644, UID: "u", GID: "g", Length: uint64(len(f.data))}
	if f.children != nil {
		d.Mode = DMDIR | 0o755
		d.Length = 0
	}
	return d, nil
}

func (f *memFile) ReadAt(ctx context.Context, p []byte, off int64) (int, error) {
	if f.slow != nil {
		<-f.slow
	}
	if off >= int64(len(f.data)) {
		return 0, io.EOF
	}
	return copy(p, f.data[off:]), nil
}

func (f *memFile) ReadDir(ctx context.Context) ([]Dir, error) {
	var ds []Dir
	for _, c := range f.children {
		d, _ := c.Stat(ctx)
		ds = append(ds, d)
	}
	return ds, nil
}

type memFS struct{ root *memFile }

func (fs memFS) Attach(ctx context.Context, uname, aname string) (File, error) {
	if aname != "img" {
		return nil, fmt.Errorf("no image %q", aname)
	}
	return fs.root, nil
}

func tree(slow chan struct{}) *memFile {
	root := &memFile{name: "/", path: 1, children: []*memFile{}}
	etc := &memFile{name: "etc", path: 2, children: []*memFile{}, parent: root}
	hosts := &memFile{name: "hosts", path: 3, data: "127.0.0.1 localhost\n", parent: etc}
	big := &memFile{name: "big", path: 4, data: strings.Repeat("x", 100_000), parent: root}
	slowf := &memFile{name: "slow", path: 5, data: "zzz", parent: root, slow: slow}
	etc.children = append(etc.children, hosts)
	root.children = append(root.children, etc, big, slowf)
	for i := 0; i < 50; i++ {
		root.children = append(root.children, &memFile{name: fmt.Sprintf("file%02d", i), path: uint64(100 + i), parent: root})
	}
	return root
}

func dial(t *testing.T, fs FS) *Client {
	a, b := net.Pipe()
	go (&Server{FS: fs}).ServeConn(a)
	c, err := NewClient(b, 8192+IOHdrSz)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}

func TestProtocol(t *testing.T) {
	c := dial(t, memFS{tree(nil)})

	if _, err := c.RPC(&Fcall{Type: Tauth, Afid: 1, Uname: "u", Aname: "img"}); err == nil {
		t.Fatal("auth accepted")
	}
	if _, err := c.RPC(&Fcall{Type: Tattach, Fid: 0, Afid: NoFid, Uname: "u", Aname: "nope"}); err == nil || !strings.Contains(err.Error(), "no image") {
		t.Fatalf("bad attach: %v", err)
	}
	r, err := c.RPC(&Fcall{Type: Tattach, Fid: 0, Afid: NoFid, Uname: "u", Aname: "img"})
	if err != nil || r.Qid.Type != QTDIR {
		t.Fatalf("attach: %v %+v", err, r)
	}

	// A full walk sets the new fid; a partial one returns the qids it got.
	r, err = c.RPC(&Fcall{Type: Twalk, Fid: 0, Newfid: 1, Wname: []string{"etc", "hosts"}})
	if err != nil || len(r.Wqid) != 2 || r.Wqid[1].Path != 3 {
		t.Fatalf("walk: %v %+v", err, r)
	}
	r, err = c.RPC(&Fcall{Type: Twalk, Fid: 0, Newfid: 2, Wname: []string{"etc", "nope"}})
	if err != nil || len(r.Wqid) != 1 {
		t.Fatalf("partial walk: %v %+v", err, r)
	}
	if _, err := c.RPC(&Fcall{Type: Tstat, Fid: 2}); err == nil {
		t.Fatal("partial walk created its fid")
	}
	if _, err := c.RPC(&Fcall{Type: Twalk, Fid: 0, Newfid: 2, Wname: []string{"nope"}}); err == nil {
		t.Fatal("failed first walk element was not an error")
	}
	// Clone, then walk .. from etc back to the root.
	if _, err := c.RPC(&Fcall{Type: Twalk, Fid: 0, Newfid: 3, Wname: []string{"etc", ".."}}); err != nil {
		t.Fatal(err)
	}
	r, _ = c.RPC(&Fcall{Type: Tstat, Fid: 3})
	d, _, err := UnmarshalDir(r.Stat)
	if err != nil || d.Qid.Path != 1 {
		t.Fatalf("stat after ..: %v %+v", err, d)
	}

	// Writes are refused.
	if _, err := c.RPC(&Fcall{Type: Topen, Fid: 1, Mode: OWRITE}); err == nil || err.Error() != ErrReadOnly.Error() {
		t.Fatalf("open for write: %v", err)
	}
	if _, err := c.RPC(&Fcall{Type: Topen, Fid: 1, Mode: OREAD | OTRUNC}); err == nil {
		t.Fatal("open with truncate accepted")
	}
	if _, err := c.RPC(&Fcall{Type: Topen, Fid: 1, Mode: OREAD}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.RPC(&Fcall{Type: Topen, Fid: 1, Mode: OREAD}); err == nil {
		t.Fatal("second open accepted")
	}
	b, err := c.ReadAll(1, 0)
	if err != nil || string(b) != "127.0.0.1 localhost\n" {
		t.Fatalf("read: %q %v", b, err)
	}
	if _, err := c.RPC(&Fcall{Type: Twrite, Fid: 1, Data: []byte("x")}); err == nil {
		t.Fatal("write accepted")
	}

	// A big file reads in msize pieces.
	c.RPC(&Fcall{Type: Twalk, Fid: 0, Newfid: 4, Wname: []string{"big"}})
	c.RPC(&Fcall{Type: Topen, Fid: 4, Mode: OREAD})
	b, err = c.ReadAll(4, 0)
	if err != nil || len(b) != 100_000 {
		t.Fatalf("big read: %d %v", len(b), err)
	}

	// A directory read returns whole entries and must continue in order.
	c.RPC(&Fcall{Type: Twalk, Fid: 0, Newfid: 5})
	c.RPC(&Fcall{Type: Topen, Fid: 5, Mode: OREAD})
	r, err = c.RPC(&Fcall{Type: Tread, Fid: 5, Offset: 0, Count: 200})
	if err != nil || len(r.Data) == 0 || len(r.Data) > 200 {
		t.Fatalf("first directory read: %d bytes, %v", len(r.Data), err)
	}
	head := r.Data
	if _, err := c.RPC(&Fcall{Type: Tread, Fid: 5, Offset: 7, Count: 200}); err == nil {
		t.Fatal("directory read at a bad offset accepted")
	}
	rest, err := c.ReadAll(5, uint64(len(head)))
	if err != nil {
		t.Fatal(err)
	}
	b = append(head, rest...)
	var names []string
	for rest := b; len(rest) > 0; {
		var d Dir
		d, rest, err = UnmarshalDir(rest)
		if err != nil {
			t.Fatal(err)
		}
		names = append(names, d.Name)
	}
	if len(names) != 53 || names[0] != "etc" || names[52] != "file49" {
		t.Fatalf("directory listing: %d entries, %v", len(names), names[:3])
	}

	for _, fid := range []uint32{1, 3, 4, 5} {
		if _, err := c.RPC(&Fcall{Type: Tclunk, Fid: fid}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := c.RPC(&Fcall{Type: Tclunk, Fid: 1}); err == nil {
		t.Fatal("clunk of a clunked fid")
	}
}

// A flush is answered only after the request it flushes.
func TestFlush(t *testing.T) {
	slow := make(chan struct{})
	a, b := net.Pipe()
	go (&Server{FS: memFS{tree(slow)}}).ServeConn(a)
	defer b.Close()
	send := func(f *Fcall) { b.Write(Marshal(f)) }
	recv := func() *Fcall {
		f, err := ReadMsg(b, DefMsize)
		if err != nil {
			t.Fatal(err)
		}
		return f
	}
	send(&Fcall{Type: Tversion, Tag: NoTag, Msize: 8192, Version: Version})
	recv()
	send(&Fcall{Type: Tattach, Tag: 1, Fid: 0, Afid: NoFid, Aname: "img"})
	recv()
	send(&Fcall{Type: Twalk, Tag: 1, Fid: 0, Newfid: 1, Wname: []string{"slow"}})
	recv()
	send(&Fcall{Type: Topen, Tag: 1, Fid: 1})
	recv()
	send(&Fcall{Type: Tread, Tag: 7, Fid: 1, Count: 10})
	send(&Fcall{Type: Tflush, Tag: 8, Oldtag: 7})
	go func() {
		time.Sleep(20 * time.Millisecond)
		close(slow)
	}()
	if r := recv(); r.Tag != 7 || r.Type != Rread {
		t.Fatalf("first reply: %+v", r)
	}
	if r := recv(); r.Tag != 8 || r.Type != Rflush {
		t.Fatalf("second reply: %+v", r)
	}
}

func TestMarshalRoundTrip(t *testing.T) {
	for _, f := range []*Fcall{
		{Type: Tversion, Tag: NoTag, Msize: 8216, Version: Version},
		{Type: Tattach, Tag: 1, Fid: 2, Afid: NoFid, Uname: "jeff", Aname: "alpine:3.20"},
		{Type: Twalk, Tag: 3, Fid: 2, Newfid: 4, Wname: []string{"a", "b"}},
		{Type: Rwalk, Tag: 3, Wqid: []Qid{{QTDIR, 0, 9}, {0, 1, 10}}},
		{Type: Tread, Tag: 5, Fid: 4, Offset: 1 << 40, Count: 100},
		{Type: Rread, Tag: 5, Data: []byte("hello")},
		{Type: Rstat, Tag: 6, Stat: MarshalDir(Dir{Name: "x", UID: "root", Length: 5})},
		{Type: Rerror, Tag: 7, Ename: "no"},
	} {
		b := Marshal(f)
		g, err := Unmarshal(b[4:])
		if err != nil {
			t.Fatalf("%d: %v", f.Type, err)
		}
		if string(Marshal(g)) != string(b) {
			t.Fatalf("%d: round trip differs", f.Type)
		}
	}
}
