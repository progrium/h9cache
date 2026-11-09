package ninep

import (
	"context"
	"errors"
	"io/fs"
	"strings"
)

// 9P2000.L is Linux's dialect (v9fs, QEMU, diod): 9P2000's framing and a
// few of its messages, with the rest replaced by Linux's file operations,
// numeric owners, errno errors, and real symlinks. The server speaks it to
// a client that asks for it and to an FS that implements LFS.

// Versions.
const (
	VersionL  = "9P2000.L"
	VersionLZ = "9P2000.L.Z" // ZeroFS's extension, which h9 offers
)

// 9P2000.L message types. None collide with 9P2000's.
const (
	Tlerror = 6 + iota
	Rlerror
	Tstatfs
	Rstatfs
	_
	_
	Tlopen
	Rlopen
	Tlcreate
	Rlcreate
	Tsymlink
	Rsymlink
	Tmknod
	Rmknod
	Trename
	Rrename
	Treadlink
	Rreadlink
	Tgetattr
	Rgetattr
	Tsetattr
	Rsetattr
)

const (
	Txattrwalk   = 30
	Txattrcreate = 32
	Treaddir     = 40
	Rreaddir     = 41
	Tfsync       = 50
	Rfsync       = 51
	Tlock        = 52
	Rlock        = 53
	Tgetlock     = 54
	Rgetlock     = 55
	Tlink        = 70
	Tmkdir       = 72
	Trenameat    = 74
	Tunlinkat    = 76
)

func isL(t uint8) bool { return t < Tversion }

// Linux file types and flags.
const (
	S_IFMT   = 0o170000
	S_IFIFO  = 0o010000
	S_IFCHR  = 0o020000
	S_IFDIR  = 0o040000
	S_IFBLK  = 0o060000
	S_IFREG  = 0o100000
	S_IFLNK  = 0o120000
	S_IFSOCK = 0o140000

	QTSYMLINK = 0x02

	lO_ACCMODE = 0o3
	lO_CREAT   = 0o100
	lO_TRUNC   = 0o1000

	getattrBasic = 0x7ff
	v9fsMagic    = 0x01021997
	lockSuccess  = 0
	lockUnlock   = 2
)

// Errnos, as Linux numbers them.
const (
	EPERM      = 1
	ENOENT     = 2
	EINTR      = 4
	EIO        = 5
	EBADF      = 9
	EACCES     = 13
	EBUSY      = 16
	ENOTDIR    = 20
	EISDIR     = 21
	EINVAL     = 22
	EROFS      = 30
	ELOOP      = 40
	ENODATA    = 61
	EOPNOTSUPP = 95
)

// Attr is a file's attributes as 9P2000.L's Rgetattr carries them.
type Attr struct {
	Mode     uint32 // st_mode: S_IFMT type and permission bits
	UID, GID uint32
	Nlink    uint64
	Rdev     uint64
	Size     uint64
	Mtime    int64 // seconds; also given as atime and ctime
}

// DirEntL is one entry of a 9P2000.L directory read.
type DirEntL struct {
	Name string
	Qid  Qid
	Type uint32 // the S_IFMT bits of the entry's mode
}

// LFS is an FS that can serve 9P2000.L. Its trees are LFiles, which show
// symlinks as themselves rather than following them.
type LFS interface {
	FS
	AttachL(ctx context.Context, uname string, nuname uint32, aname string) (LFile, error)
}

// LFile is a File in a 9P2000.L tree. Walk returns LFiles and stops at a
// symlink, whose qid has QTSYMLINK; walking on from one fails.
type LFile interface {
	File
	Attr(ctx context.Context) (Attr, error)
	// Readlink returns a symlink's target.
	Readlink(ctx context.Context) (string, error)
	// ReadDirL lists a directory without following symlinks, and
	// without "." and "..", which the server adds.
	ReadDirL(ctx context.Context) ([]DirEntL, error)
}

// Errno is an error that carries its 9P2000.L errno.
type Errno uint32

func (e Errno) Error() string { return "errno " + itoa(uint32(e)) }

func itoa(n uint32) string {
	var b [10]byte
	i := len(b)
	for {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
		if n == 0 {
			return string(b[i:])
		}
	}
}

// errnos maps the error strings servers use, Plan 9's and this package's,
// to errnos. The first match in an error's text wins.
var errnos = []struct {
	text  string
	errno uint32
}{
	{"does not exist", ENOENT},
	{"not found", ENOENT},
	{"not a directory", ENOTDIR},
	{"walk in non-directory", ENOTDIR},
	{"symbolic links", ELOOP},
	{"permission denied", EACCES},
	{"read-only", EROFS},
	{"unknown fid", EBADF},
	{"fid already in use", EBADF},
	{"fid not open", EBADF},
	{"fid already open", EBUSY},
	{"bad offset", EINVAL},
	{"authentication not required", EOPNOTSUPP},
}

// ErrnoOf returns the errno a 9P2000.L client gets for err.
func ErrnoOf(err error) uint32 {
	var e Errno
	switch {
	case errors.As(err, &e):
		return uint32(e)
	case errors.Is(err, context.Canceled):
		return EINTR
	case errors.Is(err, fs.ErrNotExist):
		return ENOENT
	case errors.Is(err, fs.ErrPermission):
		return EACCES
	}
	msg := err.Error()
	for _, m := range errnos {
		if strings.Contains(msg, m.text) {
			return m.errno
		}
	}
	return EIO
}

func rlerror(tag uint16, err error) *Fcall {
	e := &encoder{}
	e.u32(ErrnoOf(err))
	return &Fcall{Type: Rlerror, Tag: tag, Body: e.b}
}

// rl is a .L reply of type t with the encoded body.
func rl(t uint8, body []byte) *Fcall { return &Fcall{Type: t, Body: body} }

func (c *conn) lfile(n uint32) (*fid, LFile, error) {
	fd, err := c.getFid(n)
	if err != nil {
		return nil, nil, err
	}
	lf, ok := fd.file.(LFile)
	if !ok {
		return nil, nil, Errno(EOPNOTSUPP)
	}
	return fd, lf, nil
}

func (c *conn) dispatchL(f *Fcall) (*Fcall, error) {
	ctx := c.ctx
	d := &decoder{b: f.Body}
	switch f.Type {
	case Tattach:
		if f.Afid != NoFid {
			return nil, ErrNoAuth
		}
		c.mu.Lock()
		_, inUse := c.fids[f.Fid]
		c.mu.Unlock()
		if inUse {
			return nil, ErrFidInUse
		}
		root, err := c.srv.FS.(LFS).AttachL(ctx, f.Uname, f.NUname, f.Aname)
		if err != nil {
			c.srv.log().Info("attach failed", "aname", f.Aname, "uname", f.Uname, "err", err)
			return nil, err
		}
		if err := c.newFid(f.Fid, root); err != nil {
			return nil, err
		}
		return &Fcall{Type: Rattach, Qid: root.Qid()}, nil
	case Tauth:
		return nil, ErrNoAuth
	case Tflush, Twalk, Tclunk:
		return c.dispatch(f)
	case Tread:
		fd, err := c.getFid(f.Fid)
		if err != nil {
			return nil, err
		}
		fd.mu.Lock()
		isDir := fd.isDir
		fd.mu.Unlock()
		if isDir {
			return nil, Errno(EISDIR)
		}
		return c.read(ctx, f)
	case Tremove:
		c.mu.Lock()
		delete(c.fids, f.Fid)
		c.mu.Unlock()
		return nil, Errno(EROFS)
	case Twrite, Tlcreate, Tsymlink, Tmknod, Trename, Tsetattr, Txattrcreate,
		Tlink, Tmkdir, Trenameat, Tunlinkat:
		return nil, Errno(EROFS)
	case Txattrwalk:
		// No extended attributes: Linux takes this as no xattr support.
		return nil, Errno(EOPNOTSUPP)
	case Tstatfs:
		if _, err := c.getFid(d.u32()); err != nil {
			return nil, err
		}
		e := &encoder{}
		e.u32(v9fsMagic)
		e.u32(4096) // bsize
		e.u64(0)    // blocks
		e.u64(0)    // bfree
		e.u64(0)    // bavail
		e.u64(0)    // files
		e.u64(0)    // ffree
		e.u64(0)    // fsid
		e.u32(255)  // namelen
		return rl(Rstatfs, e.b), nil
	case Tlopen:
		n, flags := d.u32(), d.u32()
		if d.err != nil {
			return nil, d.err
		}
		fd, lf, err := c.lfile(n)
		if err != nil {
			return nil, err
		}
		fd.mu.Lock()
		defer fd.mu.Unlock()
		if fd.open {
			return nil, ErrIsOpen
		}
		if flags&lO_ACCMODE != 0 || flags&(lO_CREAT|lO_TRUNC) != 0 {
			return nil, Errno(EROFS)
		}
		q := lf.Qid()
		if q.Type&QTSYMLINK != 0 {
			return nil, Errno(ELOOP)
		}
		if o, ok := lf.(Opener); ok {
			if err := o.Open(ctx, OREAD); err != nil {
				return nil, err
			}
		}
		fd.open = true
		fd.isDir = q.Type&QTDIR != 0
		fd.ents = nil
		e := &encoder{}
		e.qid(q)
		e.u32(0) // iounit: msize less the header
		return rl(Rlopen, e.b), nil
	case Treadlink:
		_, lf, err := c.lfile(d.u32())
		if err != nil {
			return nil, err
		}
		if lf.Qid().Type&QTSYMLINK == 0 {
			return nil, Errno(EINVAL)
		}
		t, err := lf.Readlink(ctx)
		if err != nil {
			return nil, err
		}
		e := &encoder{}
		e.str(t)
		return rl(Rreadlink, e.b), nil
	case Tgetattr:
		_, lf, err := c.lfile(d.u32())
		if err != nil {
			return nil, err
		}
		a, err := lf.Attr(ctx)
		if err != nil {
			return nil, err
		}
		return rl(Rgetattr, encodeAttr(lf.Qid(), a)), nil
	case Treaddir:
		n, off, count := d.u32(), d.u64(), d.u32()
		if d.err != nil {
			return nil, d.err
		}
		return c.readdir(ctx, n, off, count)
	case Tfsync:
		if _, err := c.getFid(d.u32()); err != nil {
			return nil, err
		}
		return rl(Rfsync, nil), nil
	case Tlock:
		if _, err := c.getFid(d.u32()); err != nil {
			return nil, err
		}
		// Nothing can change, so every lock is granted.
		return rl(Rlock, []byte{lockSuccess}), nil
	case Tgetlock:
		if _, err := c.getFid(d.u32()); err != nil {
			return nil, err
		}
		d.u8() // type
		start, length, proc := d.u64(), d.u64(), d.u32()
		client := d.str()
		if d.err != nil {
			return nil, d.err
		}
		e := &encoder{}
		e.u8(lockUnlock)
		e.u64(start)
		e.u64(length)
		e.u32(proc)
		e.str(client)
		return rl(Rgetlock, e.b), nil
	}
	return nil, Errno(EOPNOTSUPP)
}

func encodeAttr(q Qid, a Attr) []byte {
	e := &encoder{}
	e.u64(getattrBasic) // valid
	e.qid(q)
	e.u32(a.Mode)
	e.u32(a.UID)
	e.u32(a.GID)
	e.u64(a.Nlink)
	e.u64(a.Rdev)
	e.u64(a.Size)
	e.u64(4096)                 // blksize
	e.u64((a.Size + 511) / 512) // blocks
	for range 3 {               // atime, mtime, ctime
		e.u64(uint64(a.Mtime))
		e.u64(0)
	}
	e.u64(0) // btime
	e.u64(0)
	e.u64(0) // gen
	e.u64(0) // data_version
	return e.b
}

// readdir answers a Treaddir. The listing is read once, when a read starts
// at offset 0, and kept on the fid; an entry's offset is its index plus one.
func (c *conn) readdir(ctx context.Context, n uint32, off uint64, count uint32) (*Fcall, error) {
	fd, lf, err := c.lfile(n)
	if err != nil {
		return nil, err
	}
	fd.mu.Lock()
	defer fd.mu.Unlock()
	if !fd.open {
		return nil, ErrNotOpen
	}
	if !fd.isDir {
		return nil, Errno(ENOTDIR)
	}
	if off == 0 || fd.ents == nil {
		ents, err := lf.ReadDirL(ctx)
		if err != nil {
			return nil, err
		}
		up, err := lf.Walk(ctx, "..")
		if err != nil {
			return nil, err
		}
		fd.ents = append([]DirEntL{{".", lf.Qid(), S_IFDIR}, {"..", up.Qid(), S_IFDIR}}, ents...)
	}
	if max := c.msize - IOHdrSz; count > max {
		count = max
	}
	e := &encoder{}
	for i := off; i < uint64(len(fd.ents)); i++ {
		ent := fd.ents[i]
		if len(e.b)+13+8+1+2+len(ent.Name) > int(count) {
			break
		}
		e.qid(ent.Qid)
		e.u64(i + 1)
		e.u8(uint8(ent.Type >> 12)) // DT_* is the S_IFMT type shifted down
		e.str(ent.Name)
	}
	body := (&encoder{}).with(uint32(len(e.b)), e.b)
	return rl(Rreaddir, body), nil
}

func (e *encoder) with(n uint32, b []byte) []byte {
	e.u32(n)
	return append(e.b, b...)
}
