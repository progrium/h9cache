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
	spec := Spec{Ref: ref}
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
