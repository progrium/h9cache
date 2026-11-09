package tree

import (
	"path"
	"strings"
)

// Merge applies layer onto base, the way an overlay mount stacks image
// layers: a whiteout removes a name, an opaque directory hides what was
// below it, a directory merges into a directory, and anything else
// replaces what was there. base is changed in place; layer is not.
//
// Hardlinks in layer are resolved against base after the layer is applied,
// so a link reaches the file as it stands at that layer.
func Merge(base, layer *Node) {
	var links []string
	apply(base, layer, "/", &links)
	for _, p := range links {
		resolveHardlink(base, p)
	}
}

func apply(dst, src *Node, dir string, links *[]string) {
	if src.Opaque {
		dst.Children = map[string]*Node{}
	}
	for name, c := range src.Children {
		p := path.Join(dir, name)
		switch c.Kind {
		case Whiteout:
			delete(dst.Children, name)
		case Dir:
			d, ok := dst.Children[name]
			if ok && d.Kind == Dir {
				children := d.Children
				d.Entry = c.Entry
				d.Tree, d.Opaque = "", false
				d.Children = children
			} else {
				d = NewDir(c.Entry)
				dst.Children[name] = d
			}
			apply(d, c, p, links)
		case Hardlink:
			cp := *c
			dst.Children[name] = &cp
			*links = append(*links, p)
		default:
			cp := *c
			dst.Children[name] = &cp
		}
	}
}

// Lookup finds the node at an absolute image path, without following
// symlinks.
func Lookup(root *Node, p string) *Node {
	n := root
	for _, el := range strings.Split(strings.Trim(path.Clean("/"+p), "/"), "/") {
		if el == "" {
			continue
		}
		if n.Kind != Dir {
			return nil
		}
		n = n.Children[el]
		if n == nil {
			return nil
		}
	}
	return n
}

// resolveHardlink turns the hardlink at p into a copy of the file it links
// to, sharing that file's qid through Link. A link to a missing name or to
// a directory is dropped.
func resolveHardlink(root *Node, p string) {
	parent := Lookup(root, path.Dir(p))
	if parent == nil || parent.Kind != Dir {
		return
	}
	name := path.Base(p)
	n := parent.Children[name]
	if n == nil || n.Kind != Hardlink {
		return
	}
	target := path.Clean("/" + n.Target)
	t := Lookup(root, target)
	if t == nil || t.Kind == Dir || t.Kind == Hardlink {
		delete(parent.Children, name)
		return
	}
	e := t.Entry
	e.Name = name
	if e.Link == "" {
		e.Link = target
	}
	parent.Children[name] = &Node{Entry: e}
}
