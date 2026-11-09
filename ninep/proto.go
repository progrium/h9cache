// Package ninep is a 9P2000 file server. It serves any tree that implements
// FS and File, handling the protocol: version negotiation, fids, walks,
// directory reads, and flushes. It knows nothing about images.
package ninep

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

// Message types.
const (
	Tversion = 100 + iota
	Rversion
	Tauth
	Rauth
	Tattach
	Rattach
	Terror // not used on the wire
	Rerror
	Tflush
	Rflush
	Twalk
	Rwalk
	Topen
	Ropen
	Tcreate
	Rcreate
	Tread
	Rread
	Twrite
	Rwrite
	Tclunk
	Rclunk
	Tremove
	Rremove
	Tstat
	Rstat
	Twstat
	Rwstat
)

// Protocol constants.
const (
	NoTag    = 0xFFFF
	NoFid    = 0xFFFFFFFF
	MaxWalk  = 16
	IOHdrSz  = 24
	Version  = "9P2000"
	DefMsize = 64*1024 + IOHdrSz

	QTDIR  = 0x80
	QTFILE = 0x00

	DMDIR = 0x80000000

	OREAD   = 0
	OWRITE  = 1
	ORDWR   = 2
	OEXEC   = 3
	OTRUNC  = 0x10
	ORCLOSE = 0x40
)

// Qid is a server's unique id for a file.
type Qid struct {
	Type uint8
	Vers uint32
	Path uint64
}

// Dir is a 9P2000 stat structure.
type Dir struct {
	Type   uint16
	Dev    uint32
	Qid    Qid
	Mode   uint32
	Atime  uint32
	Mtime  uint32
	Length uint64
	Name   string
	UID    string
	GID    string
	MUID   string
}

// Fcall is one 9P message. Only the fields of its type are used.
type Fcall struct {
	Type    uint8
	Tag     uint16
	Fid     uint32
	Msize   uint32
	Version string
	Afid    uint32
	Uname   string
	Aname   string
	Ename   string
	Oldtag  uint16
	Newfid  uint32
	Wname   []string
	Wqid    []Qid
	Qid     Qid
	Mode    uint8
	Iounit  uint32
	Offset  uint64
	Count   uint32
	Data    []byte
	Stat    []byte
}

var errShort = errors.New("9P: short message")

type decoder struct {
	b   []byte
	err error
}

func (d *decoder) need(n int) bool {
	if d.err != nil {
		return false
	}
	if len(d.b) < n {
		d.err = errShort
		return false
	}
	return true
}

func (d *decoder) u8() uint8 {
	if !d.need(1) {
		return 0
	}
	v := d.b[0]
	d.b = d.b[1:]
	return v
}

func (d *decoder) u16() uint16 {
	if !d.need(2) {
		return 0
	}
	v := binary.LittleEndian.Uint16(d.b)
	d.b = d.b[2:]
	return v
}

func (d *decoder) u32() uint32 {
	if !d.need(4) {
		return 0
	}
	v := binary.LittleEndian.Uint32(d.b)
	d.b = d.b[4:]
	return v
}

func (d *decoder) u64() uint64 {
	if !d.need(8) {
		return 0
	}
	v := binary.LittleEndian.Uint64(d.b)
	d.b = d.b[8:]
	return v
}

func (d *decoder) bytes(n int) []byte {
	if !d.need(n) {
		return nil
	}
	v := d.b[:n]
	d.b = d.b[n:]
	return v
}

func (d *decoder) str() string {
	n := d.u16()
	return string(d.bytes(int(n)))
}

func (d *decoder) qid() Qid {
	return Qid{Type: d.u8(), Vers: d.u32(), Path: d.u64()}
}

type encoder struct{ b []byte }

func (e *encoder) u8(v uint8)   { e.b = append(e.b, v) }
func (e *encoder) u16(v uint16) { e.b = binary.LittleEndian.AppendUint16(e.b, v) }
func (e *encoder) u32(v uint32) { e.b = binary.LittleEndian.AppendUint32(e.b, v) }
func (e *encoder) u64(v uint64) { e.b = binary.LittleEndian.AppendUint64(e.b, v) }
func (e *encoder) str(s string) {
	e.u16(uint16(len(s)))
	e.b = append(e.b, s...)
}
func (e *encoder) qid(q Qid) {
	e.u8(q.Type)
	e.u32(q.Vers)
	e.u64(q.Path)
}

// Unmarshal decodes one message, without its leading size field.
func Unmarshal(b []byte) (*Fcall, error) {
	d := &decoder{b: b}
	f := &Fcall{Type: d.u8(), Tag: d.u16()}
	switch f.Type {
	case Tversion, Rversion:
		f.Msize = d.u32()
		f.Version = d.str()
	case Tauth:
		f.Afid = d.u32()
		f.Uname = d.str()
		f.Aname = d.str()
	case Rauth, Rattach, Rcreate:
		f.Qid = d.qid()
		if f.Type == Rcreate {
			f.Iounit = d.u32()
		}
	case Tattach:
		f.Fid = d.u32()
		f.Afid = d.u32()
		f.Uname = d.str()
		f.Aname = d.str()
	case Rerror:
		f.Ename = d.str()
	case Tflush:
		f.Oldtag = d.u16()
	case Twalk:
		f.Fid = d.u32()
		f.Newfid = d.u32()
		n := d.u16()
		if n > MaxWalk {
			return nil, fmt.Errorf("9P: walk of %d names", n)
		}
		for i := 0; i < int(n); i++ {
			f.Wname = append(f.Wname, d.str())
		}
	case Rwalk:
		n := d.u16()
		for i := 0; i < int(n); i++ {
			f.Wqid = append(f.Wqid, d.qid())
		}
	case Topen:
		f.Fid = d.u32()
		f.Mode = d.u8()
	case Ropen:
		f.Qid = d.qid()
		f.Iounit = d.u32()
	case Tcreate:
		f.Fid = d.u32()
		f.Aname = d.str() // the new file's name
		d.u32()           // perm
		f.Mode = d.u8()
	case Tread:
		f.Fid = d.u32()
		f.Offset = d.u64()
		f.Count = d.u32()
	case Rread:
		f.Count = d.u32()
		f.Data = d.bytes(int(f.Count))
	case Twrite:
		f.Fid = d.u32()
		f.Offset = d.u64()
		f.Count = d.u32()
		f.Data = d.bytes(int(f.Count))
	case Rwrite:
		f.Count = d.u32()
	case Tclunk, Tremove, Tstat:
		f.Fid = d.u32()
	case Rstat:
		f.Stat = d.bytes(int(d.u16()))
	case Twstat:
		f.Fid = d.u32()
		f.Stat = d.bytes(int(d.u16()))
	case Rflush, Rclunk, Rremove, Rwstat:
	default:
		return nil, fmt.Errorf("9P: unknown message type %d", f.Type)
	}
	if d.err != nil {
		return nil, d.err
	}
	return f, nil
}

// Marshal encodes a message with its leading size field.
func Marshal(f *Fcall) []byte {
	e := &encoder{b: make([]byte, 4, 64+len(f.Data)+len(f.Stat))}
	e.u8(f.Type)
	e.u16(f.Tag)
	switch f.Type {
	case Tversion, Rversion:
		e.u32(f.Msize)
		e.str(f.Version)
	case Tauth:
		e.u32(f.Afid)
		e.str(f.Uname)
		e.str(f.Aname)
	case Rauth, Rattach:
		e.qid(f.Qid)
	case Tattach:
		e.u32(f.Fid)
		e.u32(f.Afid)
		e.str(f.Uname)
		e.str(f.Aname)
	case Rerror:
		e.str(f.Ename)
	case Tflush:
		e.u16(f.Oldtag)
	case Twalk:
		e.u32(f.Fid)
		e.u32(f.Newfid)
		e.u16(uint16(len(f.Wname)))
		for _, n := range f.Wname {
			e.str(n)
		}
	case Rwalk:
		e.u16(uint16(len(f.Wqid)))
		for _, q := range f.Wqid {
			e.qid(q)
		}
	case Topen:
		e.u32(f.Fid)
		e.u8(f.Mode)
	case Ropen, Rcreate:
		e.qid(f.Qid)
		e.u32(f.Iounit)
	case Tread:
		e.u32(f.Fid)
		e.u64(f.Offset)
		e.u32(f.Count)
	case Rread:
		e.u32(uint32(len(f.Data)))
		e.b = append(e.b, f.Data...)
	case Twrite:
		e.u32(f.Fid)
		e.u64(f.Offset)
		e.u32(uint32(len(f.Data)))
		e.b = append(e.b, f.Data...)
	case Rwrite:
		e.u32(f.Count)
	case Tclunk, Tremove, Tstat:
		e.u32(f.Fid)
	case Rstat:
		e.u16(uint16(len(f.Stat)))
		e.b = append(e.b, f.Stat...)
	case Twstat:
		e.u32(f.Fid)
		e.u16(uint16(len(f.Stat)))
		e.b = append(e.b, f.Stat...)
	}
	binary.LittleEndian.PutUint32(e.b, uint32(len(e.b)))
	return e.b
}

// ReadMsg reads one message from r. Messages over msize are refused.
func ReadMsg(r io.Reader, msize uint32) (*Fcall, error) {
	var sz [4]byte
	if _, err := io.ReadFull(r, sz[:]); err != nil {
		return nil, err
	}
	n := binary.LittleEndian.Uint32(sz[:])
	if n < 7 || n > msize {
		return nil, fmt.Errorf("9P: bad message size %d", n)
	}
	b := make([]byte, n-4)
	if _, err := io.ReadFull(r, b); err != nil {
		return nil, err
	}
	return Unmarshal(b)
}

// MarshalDir encodes a stat structure, its own size field included.
func MarshalDir(d Dir) []byte {
	e := &encoder{b: make([]byte, 2, 64+len(d.Name)+len(d.UID)+len(d.GID))}
	e.u16(d.Type)
	e.u32(d.Dev)
	e.qid(d.Qid)
	e.u32(d.Mode)
	e.u32(d.Atime)
	e.u32(d.Mtime)
	e.u64(d.Length)
	e.str(d.Name)
	e.str(d.UID)
	e.str(d.GID)
	e.str(d.MUID)
	binary.LittleEndian.PutUint16(e.b, uint16(len(e.b)-2))
	return e.b
}

// UnmarshalDir decodes one stat structure and returns the rest of b.
func UnmarshalDir(b []byte) (Dir, []byte, error) {
	d := &decoder{b: b}
	n := d.u16()
	body := d.bytes(int(n))
	if d.err != nil {
		return Dir{}, nil, d.err
	}
	rest := d.b
	d = &decoder{b: body}
	dir := Dir{
		Type: d.u16(), Dev: d.u32(), Qid: d.qid(), Mode: d.u32(),
		Atime: d.u32(), Mtime: d.u32(), Length: d.u64(),
		Name: d.str(), UID: d.str(), GID: d.str(), MUID: d.str(),
	}
	return dir, rest, d.err
}
