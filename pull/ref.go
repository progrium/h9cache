package pull

import (
	"fmt"
	"strings"

	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
)

// Spec is a parsed attach spec: an image reference and an optional
// platform, written "REF" or "REF!OS/ARCH[/VARIANT]".
type Spec struct {
	Ref      name.Reference
	Platform *v1.Platform // nil means the puller's default
	// Implicit is set when the reference names no registry, as in
	// "progrium/9front", so Docker Hub is only the default.
	Implicit bool
}

// ParseSpec parses an attach spec. Short names expand as Docker expands
// them: "alpine" is "index.docker.io/library/alpine:latest".
func ParseSpec(s string, opts ...name.Option) (Spec, error) {
	s = strings.TrimSpace(s)
	refStr, platStr, hasPlat := strings.Cut(s, "!")
	if refStr == "" {
		return Spec{}, fmt.Errorf("empty image reference")
	}
	ref, err := name.ParseReference(refStr, opts...)
	if err != nil {
		return Spec{}, err
	}
	spec := Spec{Ref: ref, Implicit: implicitRegistry(refStr)}
	if hasPlat {
		p, err := v1.ParsePlatform(platStr)
		if err != nil {
			return Spec{}, fmt.Errorf("platform %q: %w", platStr, err)
		}
		if p.OS == "" || p.Architecture == "" {
			return Spec{}, fmt.Errorf("platform %q: want OS/ARCH", platStr)
		}
		spec.Platform = p
	}
	return spec, nil
}

// RepoName names a reference's repository as access rules match it:
// registry/repository, with Docker Hub written docker.io.
func RepoName(ref name.Reference) string {
	reg := ref.Context().RegistryStr()
	if reg == name.DefaultRegistry {
		reg = "docker.io"
	}
	return reg + "/" + ref.Context().RepositoryStr()
}

// implicitRegistry reports whether a reference leaves its registry to the
// default, by Docker's rule: the first element names a registry when it
// has a dot or a colon, or is localhost.
func implicitRegistry(ref string) bool {
	first, _, ok := strings.Cut(ref, "/")
	if !ok {
		return true
	}
	return !strings.ContainsAny(first, ".:") && first != "localhost"
}
