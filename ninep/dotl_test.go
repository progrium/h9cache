package ninep

import (
	"context"
	"encoding/binary"
	"errors"
	"net"
	"testing"
)

// lmemFS claims 9P2000.L but refuses every attach, which is enough to
// test negotiation and errors.
type lmemFS struct{ memFS }

func (lmemFS) AttachL(ctx context.Context, uname string, nuname uint32, aname string) (LFile, error) {
	return nil, errors.New("file does not exist")
}

func TestNegotiateDotL(t *testing.T) {
	for _, tc := range []struct {
		fs    FS
		offer string
		want  string
	}{
		{lmemFS{}, "9P2000.L", "9P2000.L"},
		{lmemFS{}, "9P2000.L.Google.7", "9P2000.L"},
		{lmemFS{}, "9P2000.L.Z", "9P2000"}, // h9
		{lmemFS{}, "9P2000", "9P2000"},
		{lmemFS{}, "9P2000.u", "9P2000"},
		{memFS{}, "9P2000.L", "9P2000"},
		{lmemFS{}, "NFS", "unknown"},
	} {
		a, b := net.Pipe()
		go (&Server{FS: tc.fs}).ServeConn(a)
		b.Write(Marshal(&Fcall{Type: Tversion, Tag: NoTag, Msize: 8192, Version: tc.offer}))
		r, err := ReadMsg(b, 8192)
		if err != nil || r.Version != tc.want {
			t.Errorf("%T offered %s: got %+v %v, want %s", tc.fs, tc.offer, r, err, tc.want)
		}
		if tc.want == VersionL {
			// Errors are errnos now, and attach carries n_uname.
			n := binary.LittleEndian.AppendUint32(nil, 1000)
			b.Write(Marshal(&Fcall{Type: Tattach, Tag: 1, Fid: 0, Afid: NoFid, Uname: "u", Aname: "x", Body: n}))
			r, err := ReadMsg(b, 8192)
			if err != nil || r.Type != Rlerror || binary.LittleEndian.Uint32(r.Body) != ENOENT {
				t.Errorf("attach: %+v %v", r, err)
			}
		}
		b.Close()
	}
}

func TestErrnoOf(t *testing.T) {
	for err, want := range map[error]uint32{
		ErrReadOnly:                      EROFS,
		ErrUnknownFid:                    EBADF,
		errors.New("'x' does not exist"): ENOENT,
		Errno(EISDIR):                    EISDIR,
		context.Canceled:                 EINTR,
		errors.New("ken has left"):       EIO,
	} {
		if got := ErrnoOf(err); got != want {
			t.Errorf("%v: errno %d, want %d", err, got, want)
		}
	}
}
