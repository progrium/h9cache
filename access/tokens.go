// Package access maps client bearer tokens to the repositories they may
// attach.
//
// A tokens file has one token per line, followed by the repository
// patterns it allows:
//
//	# comments and blank lines are ignored
//	3f9c…d1  docker.io/library/*  ghcr.io/acme/*
//	77ab…02  *
//
// A pattern is matched against registry/repository, with Docker Hub
// written docker.io, and * matches any run of characters, slashes
// included.
package access

import (
	"bufio"
	"crypto/subtle"
	"fmt"
	"io"
	"os"
	"strings"
)

// Tokens is a parsed tokens file.
type Tokens struct {
	entries []entry
}

type entry struct {
	token    string
	patterns []string
}

// Load reads a tokens file.
func Load(path string) (*Tokens, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return Parse(f)
}

// Parse reads tokens in the file format.
func Parse(r io.Reader) (*Tokens, error) {
	t := &Tokens{}
	sc := bufio.NewScanner(r)
	line := 0
	for sc.Scan() {
		line++
		f := strings.Fields(sc.Text())
		if len(f) == 0 || strings.HasPrefix(f[0], "#") {
			continue
		}
		if len(f) < 2 {
			return nil, fmt.Errorf("tokens: line %d: a token needs at least one repository pattern", line)
		}
		if len(f[0]) < 16 {
			return nil, fmt.Errorf("tokens: line %d: token shorter than 16 characters", line)
		}
		t.entries = append(t.entries, entry{token: f[0], patterns: f[1:]})
	}
	return t, sc.Err()
}

// Allow returns the access check for a token, and false if the token is
// unknown.
func (t *Tokens) Allow(token string) (func(repo string) bool, bool) {
	if token == "" {
		return nil, false
	}
	for _, e := range t.entries {
		if subtle.ConstantTimeCompare([]byte(e.token), []byte(token)) == 1 {
			patterns := e.patterns
			return func(repo string) bool {
				for _, p := range patterns {
					if Match(p, repo) {
						return true
					}
				}
				return false
			}, true
		}
	}
	return nil, false
}

// Match reports whether repo matches pattern, where * matches any run of
// characters.
func Match(pattern, repo string) bool {
	parts := strings.Split(pattern, "*")
	if len(parts) == 1 {
		return pattern == repo
	}
	if !strings.HasPrefix(repo, parts[0]) {
		return false
	}
	rest := repo[len(parts[0]):]
	for _, p := range parts[1 : len(parts)-1] {
		i := strings.Index(rest, p)
		if i < 0 {
			return false
		}
		rest = rest[i+len(p):]
	}
	return strings.HasSuffix(rest, parts[len(parts)-1])
}
