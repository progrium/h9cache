// Package tree holds filesystem trees as Merkle objects: each directory is
// one immutable object, named by the BLAKE3 of its encoding, listing its
// entries with their attributes and either a child tree, a chunk list, or a
// symlink target. Two images that share a subtree share its objects.
//
// Layer trees, which keep whiteouts and opaque markers, and merged image
// trees, which don't, use the same encoding.
package tree

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"

	core "github.com/fvs-lab/core"

	"github.com/progrium/h9cache/cas"
	"github.com/progrium/h9cache/store"
)

// Kind is the type of a tree entry.
type Kind string

const (
	Dir      Kind = "dir"
	File     Kind = "file"
	Symlink  Kind = "symlink"
	Char     Kind = "char"
	Block    Kind = "block"
	FIFO     Kind = "fifo"
	Whiteout Kind = "whiteout" // layer trees only: removes the name below
	Hardlink Kind = "hardlink" // layer trees only: resolved at merge
)

// Entry is one name in a directory.
type Entry struct {
	Name string `json:"name"`
	Kind Kind   `json:"kind"`
	Mode uint32 `json:"mode"` // permission bits with setuid, setgid, and sticky
	UID  int    `json:"uid"`
	GID  int    `json:"gid"`
	// User and Group are owner names, from a layer that carries them, as
	// a Plan 9 tree's must: Plan 9 has no numeric ids.
	User  string `json:"user,omitempty"`
	Group string `json:"group,omitempty"`
	MTime int64  `json:"mtime"`
	Size  int64  `json:"size,omitempty"`
	// Major and Minor are a device node's numbers.
	Major int64 `json:"major,omitempty"`
	Minor int64 `json:"minor,omitempty"`

	Tree   string      `json:"tree,omitempty"`   // Dir: the child tree's id
	Opaque bool        `json:"opaque,omitempty"` // Dir in a layer: hides everything below
	Chunks []cas.Chunk `json:"chunks,omitempty"` // File
	Target string      `json:"target,omitempty"` // Symlink: link text; Hardlink: path it links to
	// Link is set on a file reached by a hardlink: the image path of the
	// name it was linked to, which gives every name of the file one qid.
	Link string `json:"link,omitempty"`
}

// Object is the stored form of one directory.
type Object struct {
	Version int     `json:"v"`
	Entries []Entry `json:"entries"`
}

// FormatVersion is the tree object encoding version.
const FormatVersion = 1

// Key returns the object key of a tree.
func Key(id string) string {
	return "trees/" + id[:2] + "/" + id[2:]
}

// Encode returns a directory's canonical encoding and its id.
func Encode(entries []Entry) ([]byte, string, error) {
	sorted := append([]Entry(nil), entries...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Name < sorted[j].Name })
	for i := 1; i < len(sorted); i++ {
		if sorted[i].Name == sorted[i-1].Name {
			return nil, "", fmt.Errorf("tree: duplicate name %q", sorted[i].Name)
		}
	}
	if sorted == nil {
		sorted = []Entry{}
	}
	b, err := json.Marshal(Object{Version: FormatVersion, Entries: sorted})
	if err != nil {
		return nil, "", err
	}
	return b, string(core.ContentID(b)), nil
}

// Decode parses a stored tree object and checks it against id.
func Decode(id string, b []byte) ([]Entry, error) {
	if string(core.ContentID(b)) != id {
		return nil, fmt.Errorf("tree %s: %w", id, core.ErrBlockCorrupt)
	}
	var obj Object
	if err := json.Unmarshal(b, &obj); err != nil {
		return nil, fmt.Errorf("tree %s: %w", id, err)
	}
	if obj.Version != FormatVersion {
		return nil, fmt.Errorf("tree %s: unknown version %d", id, obj.Version)
	}
	return obj.Entries, nil
}

// Node is an in-memory tree, used while converting and merging layers.
type Node struct {
	Entry
	Children map[string]*Node // Dir only
}

// NewDir returns an empty directory node with e's attributes.
func NewDir(e Entry) *Node {
	e.Kind = Dir
	e.Tree = ""
	e.Opaque = false
	return &Node{Entry: e, Children: map[string]*Node{}}
}

// Write stores n and everything below it, and returns n's tree id. Objects
// already in the store are not written again.
func Write(ctx context.Context, st store.Store, n *Node) (string, error) {
	entries := make([]Entry, 0, len(n.Children))
	for name, c := range n.Children {
		e := c.Entry
		e.Name = name
		if c.Kind == Dir {
			id, err := Write(ctx, st, c)
			if err != nil {
				return "", err
			}
			e.Tree = id
		}
		entries = append(entries, e)
	}
	b, id, err := Encode(entries)
	if err != nil {
		return "", err
	}
	if ok, err := st.Has(ctx, Key(id)); err != nil || ok {
		return id, err
	}
	return id, st.Put(ctx, Key(id), b)
}

// Read loads one directory object.
func Read(ctx context.Context, st store.Store, id string) ([]Entry, error) {
	if len(id) != 64 {
		return nil, fmt.Errorf("tree: bad id %q", id)
	}
	b, err := st.Get(ctx, Key(id))
	if err != nil {
		return nil, fmt.Errorf("tree %s: %w", id, err)
	}
	return Decode(id, b)
}

// Load reads the whole tree under id into memory. root supplies the
// attributes of the returned node.
func Load(ctx context.Context, st store.Store, id string, root Entry) (*Node, error) {
	entries, err := Read(ctx, st, id)
	if err != nil {
		return nil, err
	}
	n := &Node{Entry: root, Children: make(map[string]*Node, len(entries))}
	n.Kind = Dir
	n.Tree = id
	for _, e := range entries {
		if e.Kind == Dir {
			c, err := Load(ctx, st, e.Tree, e)
			if err != nil {
				return nil, err
			}
			n.Children[e.Name] = c
		} else {
			n.Children[e.Name] = &Node{Entry: e}
		}
	}
	return n, nil
}
