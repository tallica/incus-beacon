package registry

import (
	"fmt"

	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

type Platform struct {
	OS           string
	Architecture string
	Variant      string
}

func (p Platform) String() string {
	s := p.OS + "/" + p.Architecture
	if p.Variant != "" {
		s += "/" + p.Variant
	}
	return s
}

// Keyed by Incus architecture names, as in image.architecture.
var incusPlatforms = map[string]Platform{
	"x86_64":  {"linux", "amd64", ""},
	"aarch64": {"linux", "arm64", "v8"},
	"armv7l":  {"linux", "arm", "v7"},
	"armv6l":  {"linux", "arm", "v6"},
	"i686":    {"linux", "386", ""},
	"ppc64le": {"linux", "ppc64le", ""},
	"riscv64": {"linux", "riscv64", ""},
	"s390x":   {"linux", "s390x", ""},
}

func PlatformFor(incusArch string) (Platform, error) {
	p, ok := incusPlatforms[incusArch]
	if !ok {
		return Platform{}, fmt.Errorf("architecture %q: no OCI platform known", incusArch)
	}
	return p, nil
}

// matches accepts a missing variant: arm64 images often leave out v8.
func (p Platform) matches(o *ocispec.Platform) bool {
	if o == nil || o.OS != p.OS || o.Architecture != p.Architecture {
		return false
	}
	return o.Variant == p.Variant || o.Variant == "" || p.Variant == ""
}
