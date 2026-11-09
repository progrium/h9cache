package imagefs

import (
	"context"
	"errors"

	"github.com/progrium/h9reg/pull"
)

// ErrDenied is returned when a client may not attach a repository.
var ErrDenied = errors.New("permission denied")

// Access is what a transport knows about a connection's client.
type Access struct {
	// Spec is used when an attach names no image, as when a WebSocket's
	// URL path names it instead.
	Spec string
	// Allowed reports whether the client may attach a repository, named
	// as pull.RepoName names it. Nil allows every repository.
	Allowed func(repo string) bool
}

type accessKey struct{}

// WithAccess returns a context whose attaches are checked against a.
func WithAccess(ctx context.Context, a Access) context.Context {
	return context.WithValue(ctx, accessKey{}, a)
}

func accessFrom(ctx context.Context) Access {
	a, _ := ctx.Value(accessKey{}).(Access)
	return a
}

// check reports whether a may attach spec's repository.
func (a Access) check(spec pull.Spec) error {
	if a.Allowed != nil && !a.Allowed(pull.RepoName(spec.Ref)) {
		return ErrDenied
	}
	return nil
}
