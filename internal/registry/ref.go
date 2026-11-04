// Package registry reads digests, layer fingerprints and tags from registries.
package registry

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
)

const dockerHub = "docker.io"

type Ref struct {
	Registry   string
	Repository string
	Tag        string
	Digest     string
}

var (
	repoPattern = regexp.MustCompile(`^[a-z0-9]+(?:(?:[._]|__|-+)[a-z0-9]+)*(?:/[a-z0-9]+(?:(?:[._]|__|-+)[a-z0-9]+)*)*$`)
	tagPattern  = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.-]{0,127}$`)
)

// ParseRef applies Docker's defaults: docker.io, library/ and latest.
func ParseRef(s string) (Ref, error) {
	if s == "" {
		return Ref{}, errors.New("empty image ref")
	}
	var r Ref
	name := s
	if before, digest, ok := strings.Cut(name, "@"); ok {
		name, r.Digest = before, digest
		if !strings.HasPrefix(digest, "sha256:") {
			return Ref{}, fmt.Errorf("image ref %q: unsupported digest", s)
		}
	}
	if i := strings.LastIndex(name, ":"); i > strings.LastIndex(name, "/") {
		name, r.Tag = name[:i], name[i+1:]
	}

	first, rest, hasSlash := strings.Cut(name, "/")
	if hasSlash && (strings.ContainsAny(first, ".:") || first == "localhost") {
		r.Registry, r.Repository = first, rest
	} else {
		r.Registry, r.Repository = dockerHub, name
	}
	if r.Registry == "index.docker.io" || r.Registry == "registry-1.docker.io" {
		r.Registry = dockerHub
	}
	if r.Registry == dockerHub && !strings.Contains(r.Repository, "/") {
		r.Repository = "library/" + r.Repository
	}
	if r.Tag == "" && r.Digest == "" {
		r.Tag = "latest"
	}

	if !repoPattern.MatchString(r.Repository) {
		return Ref{}, fmt.Errorf("image ref %q: invalid repository %q", s, r.Repository)
	}
	if r.Tag != "" && !tagPattern.MatchString(r.Tag) {
		return Ref{}, fmt.Errorf("image ref %q: invalid tag %q", s, r.Tag)
	}
	return r, nil
}

func (r Ref) Name() string { return r.Registry + "/" + r.Repository }

// Reference is the tag, or the digest for a ref pinned without a tag.
func (r Ref) Reference() string {
	if r.Tag != "" {
		return r.Tag
	}
	return r.Digest
}

func (r Ref) String() string {
	s := r.Name()
	if r.Tag != "" {
		s += ":" + r.Tag
	}
	if r.Digest != "" {
		s += "@" + r.Digest
	}
	return s
}

// apiHost: Docker Hub serves its /v2 API away from docker.io.
func apiHost(registry string) string {
	if registry == dockerHub {
		return "registry-1.docker.io"
	}
	return registry
}
