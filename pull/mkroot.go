package pull

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/remote/transport"
)

// Root images, such as 9front's, are built by repositories named
// mkroot-NAME and published to GitHub's registry as
// ghcr.io/OWNER/mkroot-NAME. With Puller.Mkroot set, a reference that
// names OWNER/NAME on ghcr.io, or names no registry at all, is looked up
// there first, so "progrium/9front" attaches ghcr.io/progrium/mkroot-9front
// and falls back to Docker Hub's progrium/9front only if that doesn't
// exist.
const (
	MkrootRegistry = "ghcr.io"
	MkrootPrefix   = "mkroot-"
)

// mkrootCandidate returns the mkroot reference to try before spec's own,
// on registry mreg.
func mkrootCandidate(spec Spec, mreg string) (name.Reference, bool) {
	repo := spec.Ref.Context()
	reg, path := repo.RegistryStr(), repo.RepositoryStr()
	dir, base, ok := cutLast(path, "/")
	if !ok || strings.HasPrefix(base, MkrootPrefix) {
		return nil, false
	}
	switch {
	case reg == mreg:
	case spec.Implicit && dir != "library":
	default:
		return nil, false
	}
	cand := mreg + "/" + dir + "/" + MkrootPrefix + base
	var ref name.Reference
	var err error
	if d, isDigest := spec.Ref.(name.Digest); isDigest {
		ref, err = name.NewDigest(cand + "@" + d.DigestStr())
	} else {
		ref, err = name.NewTag(cand + ":" + spec.Ref.Identifier())
	}
	return ref, err == nil
}

func cutLast(s, sep string) (string, string, bool) {
	i := strings.LastIndex(s, sep)
	if i < 0 {
		return "", s, false
	}
	return s[:i], s[i+len(sep):], true
}

// located is the stored result of an mkroot lookup, under mkroot/.
type located struct {
	Ref     string    `json:"ref"` // the mkroot reference; empty if it doesn't exist
	Checked time.Time `json:"checked"`
}

func mkrootKey(ref name.Reference) string {
	return "mkroot/" + RepoName(ref) + "/" + strings.ReplaceAll(ref.Identifier(), ":", "_")
}

// Locate returns the spec Resolve should pull: spec itself, or, with
// Mkroot set, its mkroot image when one exists. Lookups are remembered
// for TagTTL, found or not.
func (p *Puller) Locate(ctx context.Context, spec Spec) (Spec, error) {
	if !p.Mkroot {
		return spec, nil
	}
	mreg := p.MkrootRegistry
	if mreg == "" {
		mreg = MkrootRegistry
	}
	cand, ok := mkrootCandidate(spec, mreg)
	if !ok {
		return spec, nil
	}
	key := mkrootKey(spec.Ref)
	var rec located
	found, err := getJSON(ctx, p.Store, key, &rec)
	if err != nil {
		return spec, err
	}
	if !found || time.Since(rec.Checked) >= p.TagTTL {
		_, herr := remote.Head(cand, append(p.Remote, remote.WithContext(ctx))...)
		switch {
		case herr == nil:
			rec = located{Ref: cand.String(), Checked: time.Now().UTC()}
		case missing(herr):
			rec = located{Checked: time.Now().UTC()}
		case found:
			p.log().Warn("mkroot check failed, using last result", "ref", cand.String(), "err", herr)
		default:
			// Without an answer, use the reference as written.
			p.log().Warn("mkroot check failed", "ref", cand.String(), "err", herr)
			return spec, nil
		}
		if err := putJSON(ctx, p.Store, key, rec); err != nil {
			return spec, err
		}
	}
	if rec.Ref == "" {
		return spec, nil
	}
	ref, err := name.ParseReference(rec.Ref)
	if err != nil {
		return spec, err
	}
	out := spec
	out.Ref, out.Implicit = ref, false
	return out, nil
}

// missing reports whether a registry error means the image isn't there.
// GitHub's registry answers 401 or 403, not 404, for a package that
// doesn't exist, since it can't tell that from a private one.
func missing(err error) bool {
	var te *transport.Error
	if !errors.As(err, &te) {
		return false
	}
	switch te.StatusCode {
	case http.StatusNotFound, http.StatusUnauthorized, http.StatusForbidden:
		return true
	}
	return false
}
