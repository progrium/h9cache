package ninep

import (
	"errors"
	"io"
	"sync"
)

// Client is a minimal synchronous 9P2000 client, one request at a time.
// It exists for tests and debugging tools, not for performance.
type Client struct {
	rw    io.ReadWriteCloser
	mu    sync.Mutex
	msize uint32
	tag   uint16
}

// NewClient negotiates a version over rw.
func NewClient(rw io.ReadWriteCloser, msize uint32) (*Client, error) {
	c := &Client{rw: rw, msize: msize}
	r, err := c.RPC(&Fcall{Type: Tversion, Tag: NoTag, Msize: msize, Version: Version})
	if err != nil {
		return nil, err
	}
	if r.Version != Version {
		return nil, errors.New("server speaks " + r.Version)
	}
	c.msize = r.Msize
	return c, nil
}

// Msize is the negotiated message size.
func (c *Client) Msize() uint32 { return c.msize }

// RPC sends one request and returns its reply. An Rerror is returned as an
// error.
func (c *Client) RPC(f *Fcall) (*Fcall, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if f.Type != Tversion {
		c.tag++
		if c.tag == NoTag {
			c.tag = 0
		}
		f.Tag = c.tag
	}
	if _, err := c.rw.Write(Marshal(f)); err != nil {
		return nil, err
	}
	r, err := ReadMsg(c.rw, c.msize)
	if err != nil {
		return nil, err
	}
	if r.Tag != f.Tag {
		return nil, errors.New("9P: reply for another tag")
	}
	if r.Type == Rerror {
		return nil, errors.New(r.Ename)
	}
	if r.Type != f.Type+1 {
		return nil, errors.New("9P: unexpected reply type")
	}
	return r, nil
}

// ReadAll reads an open fid from off to the end.
func (c *Client) ReadAll(fid uint32, off uint64) ([]byte, error) {
	var out []byte
	for {
		r, err := c.RPC(&Fcall{Type: Tread, Fid: fid, Offset: off + uint64(len(out)), Count: c.msize - IOHdrSz})
		if err != nil {
			return out, err
		}
		if len(r.Data) == 0 {
			return out, nil
		}
		out = append(out, r.Data...)
	}
}

// Close closes the connection.
func (c *Client) Close() error { return c.rw.Close() }
