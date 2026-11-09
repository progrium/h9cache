// Package dir is a store.Store backed by a local directory. Each key is a
// file under the root at the same path. Writes go to a temporary file that
// is renamed into place, so readers never see a partial object.
//
// It also works over any POSIX mount, a ZeroFS mount included.
package dir

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/progrium/h9reg/store"
)

// Store keeps objects as files under Root.
type Store struct {
	Root string
}

// New opens (creating if needed) a directory store at root.
func New(root string) (*Store, error) {
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Join(abs, ".tmp"), 0o755); err != nil {
		return nil, err
	}
	return &Store{Root: abs}, nil
}

func (s *Store) path(key string) (string, error) {
	if key == "" || strings.HasPrefix(key, "/") || strings.HasPrefix(key, ".tmp") {
		return "", fmt.Errorf("dir store: bad key %q", key)
	}
	for _, el := range strings.Split(key, "/") {
		if el == "" || el == "." || el == ".." {
			return "", fmt.Errorf("dir store: bad key %q", key)
		}
	}
	return filepath.Join(s.Root, filepath.FromSlash(key)), nil
}

func notFound(err error) error {
	if errors.Is(err, fs.ErrNotExist) {
		return store.ErrNotFound
	}
	return err
}

func (s *Store) Get(ctx context.Context, key string) ([]byte, error) {
	p, err := s.path(key)
	if err != nil {
		return nil, err
	}
	b, err := os.ReadFile(p)
	return b, notFound(err)
}

func (s *Store) GetRange(ctx context.Context, key string, off, length int64) ([]byte, error) {
	p, err := s.path(key)
	if err != nil {
		return nil, err
	}
	f, err := os.Open(p)
	if err != nil {
		return nil, notFound(err)
	}
	defer f.Close()
	buf := make([]byte, length)
	n, err := f.ReadAt(buf, off)
	if err != nil && err != io.EOF {
		return nil, err
	}
	return buf[:n], nil
}

func (s *Store) Has(ctx context.Context, key string) (bool, error) {
	p, err := s.path(key)
	if err != nil {
		return false, err
	}
	_, err = os.Stat(p)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	return err == nil, err
}

// temp writes data to a new temporary file and returns its path.
func (s *Store) temp(data []byte) (string, error) {
	f, err := os.CreateTemp(filepath.Join(s.Root, ".tmp"), "obj-*")
	if err != nil {
		return "", err
	}
	name := f.Name()
	if _, err := f.Write(data); err != nil {
		f.Close()
		os.Remove(name)
		return "", err
	}
	if err := f.Close(); err != nil {
		os.Remove(name)
		return "", err
	}
	return name, nil
}

func (s *Store) Put(ctx context.Context, key string, data []byte) error {
	p, err := s.path(key)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	tmp, err := s.temp(data)
	if err != nil {
		return err
	}
	if err := os.Rename(tmp, p); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

// PutIfAbsent links a temporary file into place. link(2) fails if the
// target exists, which makes the check and the write one atomic step.
func (s *Store) PutIfAbsent(ctx context.Context, key string, data []byte) (bool, error) {
	p, err := s.path(key)
	if err != nil {
		return false, err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return false, err
	}
	tmp, err := s.temp(data)
	if err != nil {
		return false, err
	}
	defer os.Remove(tmp)
	if err := os.Link(tmp, p); err != nil {
		if errors.Is(err, fs.ErrExist) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

func (s *Store) Delete(ctx context.Context, key string) error {
	p, err := s.path(key)
	if err != nil {
		return err
	}
	if err := os.Remove(p); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

func (s *Store) List(ctx context.Context, prefix string) ([]string, error) {
	// Walk from the deepest directory the prefix names.
	start := s.Root
	if i := strings.LastIndex(prefix, "/"); i >= 0 {
		start = filepath.Join(s.Root, filepath.FromSlash(prefix[:i]))
	}
	var keys []string
	err := filepath.WalkDir(start, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			return err
		}
		rel, _ := filepath.Rel(s.Root, p)
		rel = filepath.ToSlash(rel)
		if d.IsDir() {
			if rel == ".tmp" {
				return filepath.SkipDir
			}
			return nil
		}
		if strings.HasPrefix(rel, prefix) {
			keys = append(keys, rel)
		}
		return nil
	})
	sort.Strings(keys)
	return keys, err
}

var _ store.Store = (*Store)(nil)
