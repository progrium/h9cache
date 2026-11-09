// Package store defines the object store that everything in h9cache is kept
// in: chunks, tree objects, layer and image records, and refs. Keys are
// slash-separated paths such as "trees/ab/cdef…", so an S3 bucket and a
// directory share one layout.
package store

import (
	"context"
	"errors"
)

// ErrNotFound is returned when a key has no object.
var ErrNotFound = errors.New("object not found")

// Store is a flat key-value object store. Implementations must make Put
// atomic: a reader sees either the old object or the whole new one.
type Store interface {
	// Get returns the whole object at key.
	Get(ctx context.Context, key string) ([]byte, error)
	// GetRange returns length bytes of the object at key, starting at off.
	// A range past the end is cut short; it is not an error.
	GetRange(ctx context.Context, key string, off, length int64) ([]byte, error)
	// Has reports whether key has an object.
	Has(ctx context.Context, key string) (bool, error)
	// Put writes data at key, replacing any object there.
	Put(ctx context.Context, key string, data []byte) error
	// PutIfAbsent writes data at key only if key has no object. It reports
	// whether it wrote. Used for claims, so it must be atomic.
	PutIfAbsent(ctx context.Context, key string, data []byte) (bool, error)
	// Delete removes the object at key. Deleting a missing key is not an
	// error.
	Delete(ctx context.Context, key string) error
	// List returns every key that starts with prefix, sorted.
	List(ctx context.Context, prefix string) ([]string, error)
}
