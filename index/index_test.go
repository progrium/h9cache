package index

import (
	"bytes"
	"reflect"
	"strings"
	"testing"
)

func TestRoundTrip(t *testing.T) {
	ix := &Index{
		Entries: []Entry{
			{Path: "/bin/ls", QidPath: 0x2a1f, QidType: '-', Mode: 0o775, Length: 81920, Mtime: 1730851200, UID: "glenda", GID: "sys"},
			{Path: "/", QidPath: 1, QidVers: 7, QidType: 'd', Mode: DMDIR | 0o775, Mtime: 1730851200, UID: "sys", GID: "sys"},
			{Path: "/bin", QidPath: 2, QidVers: 3, QidType: 'd', Mode: DMDIR | 0o775, Mtime: 1730851200, UID: "sys", GID: "sys"},
		},
		Unindexed: []string{"/odd"},
	}
	b, err := Bytes(ix)
	if err != nil {
		t.Fatal(err)
	}
	want := `h9index 1
!unindexed /odd
/ 0000000000000001 7 d 020000000775 0 1730851200 sys sys
/bin 0000000000000002 3 d 020000000775 0 1730851200 sys sys
/bin/ls 0000000000002a1f 0 - 0775 81920 1730851200 glenda sys
`
	if string(b) != want {
		t.Fatalf("got:\n%s\nwant:\n%s", b, want)
	}
	back, err := Parse(bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	if len(back.Entries) != 3 || back.Entries[0].Path != "/" || !reflect.DeepEqual(back.Unindexed, ix.Unindexed) {
		t.Fatalf("parsed: %+v", back)
	}
	b2, _ := Bytes(back)
	if !bytes.Equal(b, b2) {
		t.Fatal("re-encoding differs")
	}
}

func TestRejects(t *testing.T) {
	if _, err := Bytes(&Index{Entries: []Entry{{Path: "/a b"}}}); err == nil {
		t.Fatal("path with a space written")
	}
	for _, bad := range []string{
		"",
		"h9index 2\n",
		"h9index 1\n/ 1 0 d 0755 0 0 a\n",
		"h9index 1\n/ zz 0 d 0755 0 0 a b\n",
		"h9index 1\n/b 1 0 - 0644 0 0 a b\n/a 2 0 - 0644 0 0 a b\n",
	} {
		if _, err := Parse(strings.NewReader(bad)); err == nil {
			t.Fatalf("accepted %q", bad)
		}
	}
	for _, name := range []string{"a b", "a\nb", "\xff", ""} {
		if Listable(name) {
			t.Fatalf("%q listable", name)
		}
	}
}
