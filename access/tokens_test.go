package access

import (
	"strings"
	"testing"
)

func TestTokens(t *testing.T) {
	tk, err := Parse(strings.NewReader(`
# team tokens
aaaaaaaaaaaaaaaa  docker.io/library/*  ghcr.io/acme/*
bbbbbbbbbbbbbbbb  *
`))
	if err != nil {
		t.Fatal(err)
	}
	allow, ok := tk.Allow("aaaaaaaaaaaaaaaa")
	if !ok {
		t.Fatal("known token refused")
	}
	for repo, want := range map[string]bool{
		"docker.io/library/alpine": true,
		"ghcr.io/acme/a/b":         true,
		"ghcr.io/other/x":          false,
		"docker.io/jeff/app":       false,
	} {
		if allow(repo) != want {
			t.Fatalf("%s: got %v", repo, !want)
		}
	}
	if all, _ := tk.Allow("bbbbbbbbbbbbbbbb"); !all("anything/at/all") {
		t.Fatal("* refused a repository")
	}
	for _, bad := range []string{"", "aaaaaaaaaaaaaaab", "aaaa"} {
		if _, ok := tk.Allow(bad); ok {
			t.Fatalf("token %q accepted", bad)
		}
	}
	for _, bad := range []string{"aaaaaaaaaaaaaaaa\n", "short docker.io/*\n"} {
		if _, err := Parse(strings.NewReader(bad)); err == nil {
			t.Fatalf("parsed %q", bad)
		}
	}
}

func TestMatch(t *testing.T) {
	for _, tc := range []struct {
		p, s string
		want bool
	}{
		{"a/b", "a/b", true},
		{"a/b", "a/bc", false},
		{"a/*/c", "a/x/y/c", true},
		{"*/alpine", "docker.io/library/alpine", true},
		{"*/alpine", "docker.io/library/alpine2", false},
		{"a*b*c", "abc", true},
		{"a*b*c", "acb", false},
	} {
		if Match(tc.p, tc.s) != tc.want {
			t.Fatalf("Match(%q, %q) != %v", tc.p, tc.s, tc.want)
		}
	}
}
