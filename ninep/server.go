package ninep

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"strings"
	"sync"
)

// FS is a tree of files a client can attach to.
type FS interface {
	// Attach returns the root of the tree named by aname, for user uname.
	// It may take a long time: an image attach can pull the image first.
	Attach(ctx context.Context, uname, aname string) (File, error)
}

// File is one file in a tree. Files are read-only.
type File interface {
	Qid() Qid
	// Walk returns the file name in this directory. ".." walks to the
	// parent, and stops at the root.
	Walk(ctx context.Context, name string) (File, error)
	Stat(ctx context.Context) (Dir, error)
	// ReadAt reads a regular file.
	ReadAt(ctx context.Context, p []byte, off int64) (int, error)
	// ReadDir lists a directory.
	ReadDir(ctx context.Context) ([]Dir, error)
}

// Opener is implemented by files that can refuse an open, such as device
// nodes that exist only as names.
type Opener interface {
	Open(ctx context.Context, mode uint8) error
}

// Errors reported to clients.
var (
	ErrReadOnly   = errors.New("read-only file system")
	ErrUnknownFid = errors.New("unknown fid")
	ErrFidInUse   = errors.New("fid already in use")
	ErrNotOpen    = errors.New("fid not open")
	ErrIsOpen     = errors.New("fid already open")
	ErrBadOffset  = errors.New("bad offset in directory read")
	ErrNoAuth     = errors.New("authentication not required")
	ErrWalkNoDir  = errors.New("walk in non-directory")
)

// Server serves an FS over 9P2000, and over 9P2000.L to a client that asks
// for it when the FS is an LFS.
type Server struct {
	FS    FS
	Log   *slog.Logger
	Msize uint32 // largest message accepted; DefMsize if zero
}

func (s *Server) log() *slog.Logger {
	if s.Log == nil {
		return slog.Default()
	}
	return s.Log
}

// Serve accepts connections on l until it fails.
func (s *Server) Serve(l net.Listener) error {
	for {
		c, err := l.Accept()
		if err != nil {
			return err
		}
		go s.ServeConn(c)
	}
}

type fid struct {
	mu     sync.Mutex
	file   File
	open   bool
	isDir  bool
	dir    []byte    // encoded entries of an open directory, from dirOff on
	dirOff uint64    // offset the next directory read must use
	ents   []DirEntL // a .L directory's listing, from its first read
}

type conn struct {
	srv   *Server
	rw    io.ReadWriteCloser
	ctx   context.Context
	msize uint32
	dotl  bool // 9P2000.L was negotiated

	wmu sync.Mutex

	mu   sync.Mutex
	fids map[uint32]*fid
	tags map[uint16]chan struct{}
	wg   sync.WaitGroup
}

// ServeConn serves one client connection until it closes.
func (s *Server) ServeConn(rw io.ReadWriteCloser) {
	s.ServeConnContext(context.Background(), rw)
}

// ServeConnContext serves one connection with requests made under ctx, so
// a transport can pass what it knows about the client, such as its
// credentials, to the FS.
func (s *Server) ServeConnContext(parent context.Context, rw io.ReadWriteCloser) {
	defer rw.Close()
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	max := s.Msize
	if max == 0 {
		max = DefMsize
	}
	c := &conn{srv: s, rw: rw, ctx: ctx, msize: max,
		fids: map[uint32]*fid{}, tags: map[uint16]chan struct{}{}}
	defer c.wg.Wait()
	for {
		f, err := ReadMsg(rw, c.msize)
		if err != nil {
			if !errors.Is(err, io.EOF) && !errors.Is(err, net.ErrClosed) {
				s.log().Debug("9P read", "err", err)
			}
			return
		}
		if f.Type == Tversion {
			// A version resets the session, so wait for everything in flight.
			c.wg.Wait()
			c.reply(c.version(f))
			continue
		}
		c.mu.Lock()
		if _, dup := c.tags[f.Tag]; dup {
			c.mu.Unlock()
			c.reply(c.errReply(f.Tag, errors.New("tag in use")))
			continue
		}
		done := make(chan struct{})
		c.tags[f.Tag] = done
		c.mu.Unlock()
		c.wg.Add(1)
		go func() {
			defer c.wg.Done()
			r := c.handle(f)
			// Free the tag before the reply goes out, since the client may
			// reuse it as soon as it reads the reply. Holding the write lock
			// keeps a flush's reply from overtaking this one.
			c.wmu.Lock()
			c.mu.Lock()
			delete(c.tags, f.Tag)
			c.mu.Unlock()
			c.write(r)
			c.wmu.Unlock()
			close(done)
		}()
	}
}

func (c *conn) reply(f *Fcall) {
	c.wmu.Lock()
	defer c.wmu.Unlock()
	c.write(f)
}

// write sends a message; the caller holds wmu.
func (c *conn) write(f *Fcall) {
	if _, err := c.rw.Write(Marshal(f)); err != nil {
		c.srv.log().Debug("9P write", "err", err)
	}
}

func rerror(tag uint16, err error) *Fcall {
	return &Fcall{Type: Rerror, Tag: tag, Ename: err.Error()}
}

func (c *conn) errReply(tag uint16, err error) *Fcall {
	if c.dotl {
		return rlerror(tag, err)
	}
	return rerror(tag, err)
}

// version negotiates the dialect. A client that asks for 9P2000.L gets it
// if the FS can serve it. h9 asks for 9P2000.L.Z and gets 9P2000, which
// its translator passes through: 9P2000 gives it owner names and whole
// directories in one read, where .L would cost a round trip per entry.
func (c *conn) version(f *Fcall) *Fcall {
	c.mu.Lock()
	c.fids = map[uint32]*fid{}
	c.mu.Unlock()
	msize := f.Msize
	if msize > c.msize {
		msize = c.msize
	}
	if msize < IOHdrSz+256 {
		return rerror(f.Tag, errors.New("msize too small"))
	}
	c.msize = msize
	_, canL := c.srv.FS.(LFS)
	v := Version
	c.dotl = false
	switch {
	case !strings.HasPrefix(f.Version, Version):
		v = "unknown"
	case canL && (f.Version == VersionL || strings.HasPrefix(f.Version, VersionL+".") && f.Version != VersionLZ):
		v = VersionL
		c.dotl = true
	}
	return &Fcall{Type: Rversion, Tag: f.Tag, Msize: msize, Version: v}
}

func (c *conn) getFid(n uint32) (*fid, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	f, ok := c.fids[n]
	if !ok {
		return nil, ErrUnknownFid
	}
	return f, nil
}

func (c *conn) newFid(n uint32, file File) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.fids[n]; ok {
		return ErrFidInUse
	}
	c.fids[n] = &fid{file: file}
	return nil
}

func (c *conn) handle(f *Fcall) *Fcall {
	var r *Fcall
	var err error
	if c.dotl {
		r, err = c.dispatchL(f)
	} else {
		r, err = c.dispatch(f)
	}
	if err != nil {
		return c.errReply(f.Tag, err)
	}
	r.Tag = f.Tag
	return r
}

func (c *conn) dispatch(f *Fcall) (*Fcall, error) {
	ctx := c.ctx
	switch f.Type {
	case Tauth:
		return nil, ErrNoAuth
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
		root, err := c.srv.FS.Attach(ctx, f.Uname, f.Aname)
		if err != nil {
			c.srv.log().Info("attach failed", "aname", f.Aname, "uname", f.Uname, "err", err)
			return nil, err
		}
		if err := c.newFid(f.Fid, root); err != nil {
			return nil, err
		}
		return &Fcall{Type: Rattach, Qid: root.Qid()}, nil
	case Tflush:
		c.mu.Lock()
		done, ok := c.tags[f.Oldtag]
		c.mu.Unlock()
		if ok && f.Oldtag != f.Tag {
			<-done
		}
		return &Fcall{Type: Rflush}, nil
	case Twalk:
		return c.walk(ctx, f)
	case Topen:
		fd, err := c.getFid(f.Fid)
		if err != nil {
			return nil, err
		}
		fd.mu.Lock()
		defer fd.mu.Unlock()
		if fd.open {
			return nil, ErrIsOpen
		}
		if m := f.Mode & 3; m == OWRITE || m == ORDWR || f.Mode&(OTRUNC|ORCLOSE) != 0 {
			return nil, ErrReadOnly
		}
		if o, ok := fd.file.(Opener); ok {
			if err := o.Open(ctx, f.Mode); err != nil {
				return nil, err
			}
		}
		q := fd.file.Qid()
		fd.open = true
		fd.isDir = q.Type&QTDIR != 0
		return &Fcall{Type: Ropen, Qid: q}, nil
	case Tcreate, Twrite, Tremove, Twstat:
		if f.Type == Tremove {
			// remove clunks the fid even when it fails.
			c.mu.Lock()
			delete(c.fids, f.Fid)
			c.mu.Unlock()
		}
		return nil, ErrReadOnly
	case Tread:
		return c.read(ctx, f)
	case Tclunk:
		c.mu.Lock()
		defer c.mu.Unlock()
		if _, ok := c.fids[f.Fid]; !ok {
			return nil, ErrUnknownFid
		}
		delete(c.fids, f.Fid)
		return &Fcall{Type: Rclunk}, nil
	case Tstat:
		fd, err := c.getFid(f.Fid)
		if err != nil {
			return nil, err
		}
		d, err := fd.file.Stat(ctx)
		if err != nil {
			return nil, err
		}
		return &Fcall{Type: Rstat, Stat: MarshalDir(d)}, nil
	}
	return nil, errors.New("unexpected message")
}

func (c *conn) walk(ctx context.Context, f *Fcall) (*Fcall, error) {
	fd, err := c.getFid(f.Fid)
	if err != nil {
		return nil, err
	}
	fd.mu.Lock()
	open, file := fd.open, fd.file
	fd.mu.Unlock()
	if open {
		return nil, ErrIsOpen
	}
	if f.Newfid != f.Fid {
		c.mu.Lock()
		_, inUse := c.fids[f.Newfid]
		c.mu.Unlock()
		if inUse {
			return nil, ErrFidInUse
		}
	}
	if len(f.Wname) > 0 && file.Qid().Type&QTDIR == 0 {
		return nil, ErrWalkNoDir
	}
	qids := make([]Qid, 0, len(f.Wname))
	cur := file
	for i, name := range f.Wname {
		if cur.Qid().Type&QTDIR == 0 {
			break
		}
		next, err := cur.Walk(ctx, name)
		if err != nil {
			c.srv.log().Debug("walk failed", "name", name, "err", err)
			if i == 0 {
				return nil, err
			}
			break
		}
		cur = next
		qids = append(qids, cur.Qid())
	}
	if len(qids) == len(f.Wname) {
		c.mu.Lock()
		if f.Newfid != f.Fid {
			if _, inUse := c.fids[f.Newfid]; inUse {
				c.mu.Unlock()
				return nil, ErrFidInUse
			}
		}
		c.fids[f.Newfid] = &fid{file: cur}
		c.mu.Unlock()
	}
	return &Fcall{Type: Rwalk, Wqid: qids}, nil
}

func (c *conn) read(ctx context.Context, f *Fcall) (*Fcall, error) {
	fd, err := c.getFid(f.Fid)
	if err != nil {
		return nil, err
	}
	fd.mu.Lock()
	defer fd.mu.Unlock()
	if !fd.open {
		return nil, ErrNotOpen
	}
	count := f.Count
	if max := c.msize - IOHdrSz; count > max {
		count = max
	}
	if !fd.isDir {
		buf := make([]byte, count)
		n, err := fd.file.ReadAt(ctx, buf, int64(f.Offset))
		if err != nil && !errors.Is(err, io.EOF) {
			return nil, err
		}
		return &Fcall{Type: Rread, Data: buf[:n]}, nil
	}

	if f.Offset == 0 {
		dirs, err := fd.file.ReadDir(ctx)
		if err != nil {
			return nil, err
		}
		fd.dir = fd.dir[:0]
		for _, d := range dirs {
			fd.dir = append(fd.dir, MarshalDir(d)...)
		}
		fd.dirOff = 0
	} else if f.Offset != fd.dirOff {
		return nil, ErrBadOffset
	}
	// Return whole entries only.
	n := 0
	for n < len(fd.dir) {
		sz := int(fd.dir[n]) | int(fd.dir[n+1])<<8
		if n+2+sz > int(count) {
			break
		}
		n += 2 + sz
	}
	data := append([]byte(nil), fd.dir[:n]...)
	fd.dir = fd.dir[n:]
	fd.dirOff += uint64(n)
	return &Fcall{Type: Rread, Data: data}, nil
}
