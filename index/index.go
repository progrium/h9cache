// Package index writes and reads the .h9index text format from hyper9's
// mount-index proposal: one line per name in a tree, with its qid, mode,
// length, mtime, and owner, and none of the contents.
//
//	h9index 1
//	/ 0000000000000001 0 d 020000000755 0 1730851200 root root
//	/bin/ls 0000000000002a1f 0 - 0755 81920 1730851200 root root
package index

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"
)

// Header is the format line.
const Header = "h9index 1"

// DMDIR is Plan 9's directory mode bit.
const DMDIR = 0x80000000

// Entry is one name in the index.
type Entry struct {
	Path    string // absolute, "/" for the root
	QidPath uint64
	QidVers uint32
	QidType byte // 'd', 'a', 'l', or '-'
	Mode    uint32
	Length  uint64
	Mtime   uint32
	UID     string
	GID     string
}

// Index is a whole tree's entries and the directories it can't answer for.
type Index struct {
	Entries   []Entry
	Unindexed []string // directories h9 must not answer from the index
}

// Listable reports whether a name can appear in a path in the index: the
// path ends at the first space, and the file is one name per line.
func Listable(name string) bool {
	return name != "" && utf8.ValidString(name) && !strings.ContainsAny(name, " \n\r")
}

func validField(s string) bool {
	return s != "" && utf8.ValidString(s) && !strings.ContainsAny(s, " \n\r\t")
}

// Write encodes ix, sorting its entries by path in byte order.
func Write(w io.Writer, ix *Index) error {
	entries := append([]Entry(nil), ix.Entries...)
	sort.Slice(entries, func(i, j int) bool { return entries[i].Path < entries[j].Path })
	un := append([]string(nil), ix.Unindexed...)
	sort.Strings(un)

	bw := bufio.NewWriter(w)
	fmt.Fprintln(bw, Header)
	for _, d := range un {
		if !Listable(strings.ReplaceAll(d, "/", "x")) && d != "/" {
			return fmt.Errorf("index: unlistable directory %q", d)
		}
		fmt.Fprintf(bw, "!unindexed %s\n", d)
	}
	for _, e := range entries {
		if e.Path != "/" && !Listable(strings.ReplaceAll(e.Path, "/", "x")) {
			return fmt.Errorf("index: unlistable path %q", e.Path)
		}
		uid, gid := e.UID, e.GID
		if !validField(uid) {
			uid = "none"
		}
		if !validField(gid) {
			gid = "none"
		}
		t := e.QidType
		if t == 0 {
			t = '-'
		}
		fmt.Fprintf(bw, "%s %016x %d %c 0%o %d %d %s %s\n",
			e.Path, e.QidPath, e.QidVers, t, e.Mode, e.Length, e.Mtime, uid, gid)
	}
	return bw.Flush()
}

// Bytes encodes ix.
func Bytes(ix *Index) ([]byte, error) {
	var buf bytes.Buffer
	err := Write(&buf, ix)
	return buf.Bytes(), err
}

// Parse reads an index. Any line it can't parse fails the whole index, as
// h9 treats it.
func Parse(r io.Reader) (*Index, error) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	if !sc.Scan() || sc.Text() != Header {
		return nil, fmt.Errorf("index: missing %q header", Header)
	}
	ix := &Index{}
	line := 1
	for sc.Scan() {
		line++
		s := sc.Text()
		if d, ok := strings.CutPrefix(s, "!unindexed "); ok {
			ix.Unindexed = append(ix.Unindexed, d)
			continue
		}
		f := strings.Split(s, " ")
		if len(f) != 9 || len(f[3]) != 1 {
			return nil, fmt.Errorf("index: line %d: want 9 fields", line)
		}
		var e Entry
		var err error
		e.Path = f[0]
		if e.QidPath, err = strconv.ParseUint(f[1], 16, 64); err != nil {
			return nil, fmt.Errorf("index: line %d: qid path: %w", line, err)
		}
		v, err := strconv.ParseUint(f[2], 10, 32)
		if err != nil {
			return nil, fmt.Errorf("index: line %d: qid vers: %w", line, err)
		}
		e.QidVers = uint32(v)
		e.QidType = f[3][0]
		m, err := strconv.ParseUint(f[4], 8, 32)
		if err != nil {
			return nil, fmt.Errorf("index: line %d: mode: %w", line, err)
		}
		e.Mode = uint32(m)
		if e.Length, err = strconv.ParseUint(f[5], 10, 64); err != nil {
			return nil, fmt.Errorf("index: line %d: length: %w", line, err)
		}
		mt, err := strconv.ParseUint(f[6], 10, 32)
		if err != nil {
			return nil, fmt.Errorf("index: line %d: mtime: %w", line, err)
		}
		e.Mtime = uint32(mt)
		e.UID, e.GID = f[7], f[8]
		if n := len(ix.Entries); n > 0 && ix.Entries[n-1].Path >= e.Path {
			return nil, fmt.Errorf("index: line %d: paths out of order", line)
		}
		ix.Entries = append(ix.Entries, e)
	}
	return ix, sc.Err()
}
