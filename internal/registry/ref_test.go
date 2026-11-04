package registry

import "testing"

func TestParseRef(t *testing.T) {
	t.Parallel()
	tests := []struct {
		in   string
		want Ref
	}{
		{"nginx", Ref{"docker.io", "library/nginx", "latest", ""}},
		{"nginx:1.27", Ref{"docker.io", "library/nginx", "1.27", ""}},
		{"example/app:2", Ref{"docker.io", "example/app", "2", ""}},
		{"docker.io/nginx:1", Ref{"docker.io", "library/nginx", "1", ""}},
		{"index.docker.io/library/nginx", Ref{"docker.io", "library/nginx", "latest", ""}},
		{"ghcr.io/example/app:1.2.3-alpine", Ref{"ghcr.io", "example/app", "1.2.3-alpine", ""}},
		{"registry.example.com:5000/team/app", Ref{"registry.example.com:5000", "team/app", "latest", ""}},
		{"localhost/app:dev", Ref{"localhost", "app", "dev", ""}},
		{"ghcr.io/example/app:1@sha256:abc", Ref{"ghcr.io", "example/app", "1", "sha256:abc"}},
		{"ghcr.io/example/app@sha256:abc", Ref{"ghcr.io", "example/app", "", "sha256:abc"}},
	}
	for _, tt := range tests {
		got, err := ParseRef(tt.in)
		if err != nil {
			t.Errorf("ParseRef(%q): %v", tt.in, err)
			continue
		}
		if got != tt.want {
			t.Errorf("ParseRef(%q) = %+v, want %+v", tt.in, got, tt.want)
		}
	}
}

func TestParseRefErrors(t *testing.T) {
	t.Parallel()
	for _, in := range []string{"", "Example/App", "ghcr.io/app:bad tag", "app@md5:abc", "ghcr.io/"} {
		if r, err := ParseRef(in); err == nil {
			t.Errorf("ParseRef(%q) = %+v, want an error", in, r)
		}
	}
}

func TestRefStrings(t *testing.T) {
	t.Parallel()
	r := Ref{"docker.io", "library/nginx", "1.27", "sha256:abc"}
	if r.String() != "docker.io/library/nginx:1.27@sha256:abc" || r.Reference() != "1.27" {
		t.Errorf("String %q, Reference %q", r.String(), r.Reference())
	}
	if p := (Ref{"ghcr.io", "a/b", "", "sha256:abc"}).Reference(); p != "sha256:abc" {
		t.Errorf("pinned Reference %q", p)
	}
	if apiHost("docker.io") != "registry-1.docker.io" || apiHost("ghcr.io") != "ghcr.io" {
		t.Error("apiHost")
	}
}

func TestPlatformFor(t *testing.T) {
	t.Parallel()
	p, err := PlatformFor("aarch64")
	if err != nil || p.String() != "linux/arm64/v8" {
		t.Errorf("aarch64: %v, %v", p, err)
	}
	if _, err := PlatformFor("mips"); err == nil {
		t.Error("unknown architecture accepted")
	}
}
