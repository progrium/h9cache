package pull

import (
	"archive/tar"
	"context"
	"fmt"
	"io"
	"path"
	"strings"

	"github.com/progrium/h9reg/cas"
	"github.com/progrium/h9reg/tree"
)

const (
	whiteoutPrefix = ".wh."
	opaqueMarker   = ".wh..wh..opq"
)

// ConvertLayer reads an uncompressed layer tar and returns it as a layer
// tree, adding each regular file's chunks to b. Whiteouts and opaque
// markers are kept as entries for the merge. The caller flushes b.
func ConvertLayer(ctx context.Context, b *cas.Batch, r io.Reader) (*tree.Node, error) {
	root := tree.NewDir(tree.Entry{Mode: 0o755})
	tr := tar.NewReader(r)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return root, nil
		}
		if err != nil {
			return nil, fmt.Errorf("layer tar: %w", err)
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		p := path.Clean("/" + hdr.Name)
		if p == "/" {
			if hdr.Typeflag == tar.TypeDir {
				setAttrs(&root.Entry, hdr)
			}
			continue
		}
		dir, base := path.Split(p)
		parent := ensureDir(root, dir)

		if base == opaqueMarker {
			parent.Opaque = true
			continue
		}
		if strings.HasPrefix(base, whiteoutPrefix) {
			name := base[len(whiteoutPrefix):]
			parent.Children[name] = &tree.Node{Entry: tree.Entry{Name: name, Kind: tree.Whiteout}}
			continue
		}

		e := tree.Entry{Name: base}
		setAttrs(&e, hdr)
		switch hdr.Typeflag {
		case tar.TypeDir:
			if d := parent.Children[base]; d != nil && d.Kind == tree.Dir {
				opaque := d.Opaque
				d.Entry = e
				d.Kind, d.Opaque = tree.Dir, opaque
			} else {
				parent.Children[base] = tree.NewDir(e)
			}
			continue
		case tar.TypeReg, tar.TypeRegA, tar.TypeGNUSparse:
			e.Kind = tree.File
			chunks, size, err := b.WriteFile(ctx, tr)
			if err != nil {
				return nil, fmt.Errorf("%s: %w", p, err)
			}
			e.Chunks, e.Size = chunks, size
		case tar.TypeSymlink:
			e.Kind = tree.Symlink
			e.Target = hdr.Linkname
			e.Size = int64(len(hdr.Linkname))
		case tar.TypeLink:
			e.Kind = tree.Hardlink
			e.Target = path.Clean("/" + hdr.Linkname)
		case tar.TypeChar:
			e.Kind = tree.Char
		case tar.TypeBlock:
			e.Kind = tree.Block
		case tar.TypeFifo:
			e.Kind = tree.FIFO
		default:
			// Global headers and other metadata-only entries.
			continue
		}
		parent.Children[base] = &tree.Node{Entry: e}
	}
}

func setAttrs(e *tree.Entry, hdr *tar.Header) {
	e.Mode = uint32(hdr.Mode) & 0o7777
	e.UID = hdr.Uid
	e.GID = hdr.Gid
	e.MTime = hdr.ModTime.Unix()
}

// ensureDir returns the directory node at dir, creating it and its parents
// (with default attributes) if the tar hasn't listed them yet.
func ensureDir(root *tree.Node, dir string) *tree.Node {
	n := root
	for _, el := range strings.Split(strings.Trim(dir, "/"), "/") {
		if el == "" {
			continue
		}
		c := n.Children[el]
		if c == nil || c.Kind != tree.Dir {
			c = tree.NewDir(tree.Entry{Name: el, Mode: 0o755})
			n.Children[el] = c
		}
		n = c
	}
	return n
}
