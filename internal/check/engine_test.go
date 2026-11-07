package check

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/tallica/incus-beacon/internal/incus"
	"github.com/tallica/incus-beacon/internal/policy"
	"github.com/tallica/incus-beacon/internal/registry"
)

type image struct {
	head string
	img  registry.Image
}

type fakeRegistry struct {
	mu      sync.Mutex
	images  map[string]image // ref string -> image
	tags    map[string][]string
	headErr error
	tagsErr error
	calls   map[string]int // "head", "resolve", "tags"
}

func newFake() *fakeRegistry {
	return &fakeRegistry{images: map[string]image{}, tags: map[string][]string{}, calls: map[string]int{}}
}

func (f *fakeRegistry) count(name string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls[name]
}

func (f *fakeRegistry) Head(_ context.Context, ref registry.Ref) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls["head"]++
	if f.headErr != nil {
		return "", f.headErr
	}
	img, ok := f.images[ref.String()]
	if !ok {
		return "", registry.ErrNotFound
	}
	return img.head, nil
}

func (f *fakeRegistry) Resolve(_ context.Context, ref registry.Ref, _ registry.Platform) (registry.Image, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls["resolve"]++
	return f.images[ref.String()].img, nil
}

func (f *fakeRegistry) Tags(_ context.Context, ref registry.Ref) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls["tags"]++
	return f.tags[ref.Name()], f.tagsErr
}

func (f *fakeRegistry) set(ref, head, fingerprint string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.images[ref] = image{head: head, img: registry.Image{Digest: head, ManifestDigest: head, Fingerprint: fingerprint}}
}

const (
	fpOld = "0000000000000000000000000000000000000000000000000000000000000001"
	fpNew = "0000000000000000000000000000000000000000000000000000000000000002"
	head1 = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	head2 = "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
)

func target(name, ref, base string, track string) incus.Target {
	if track == "" {
		track = policy.TrackDigest
	}
	return incus.Target{
		Project: "default", Instance: name, Arch: "x86_64",
		BaseImage: base, Ref: ref,
		Policy: policy.Policy{Watched: true, Track: track},
	}
}

func run(t *testing.T, f *fakeRegistry, targets ...incus.Target) []Result {
	t.Helper()
	return New(f, 4, zerolog.Nop()).Run(t.Context(), targets)
}

func TestDigest(t *testing.T) {
	t.Parallel()
	f := newFake()
	f.set("ghcr.io/example/app:1.2", head1, fpOld)
	f.set("ghcr.io/example/new:1.2", head2, fpNew)

	local := target("local", "ghcr.io/example/app:1.2", fpOld, "")
	local.Local, local.RefSource = true, incus.RefFromBeacon

	res := run(t, f,
		target("same", "ghcr.io/example/app:1.2", fpOld, ""),
		target("rebuilt", "ghcr.io/example/new:1.2", fpOld, ""),
		local,
	)
	if r := res[0]; r.Status != Current || r.Latest != "" || r.Current != "ghcr.io/example/app:1.2" {
		t.Errorf("same layers: %+v", r)
	}
	if r := res[1]; r.Status != UpdateAvailable || r.Latest != "1.2@sha256:bbbbbbbbbbbb" {
		t.Errorf("rebuilt tag: %+v", r)
	}
	if r := res[2]; r.Status != Current {
		t.Errorf("local image with user.beacon.image: %+v, want it checked", r)
	}
}

func TestSemver(t *testing.T) {
	t.Parallel()
	f := newFake()
	f.set("docker.io/library/nginx:1.27.3-alpine", head1, fpOld)
	f.set("docker.io/library/nginx:latest", head1, fpOld)
	f.tags["docker.io/library/nginx"] = []string{"1.27.3-alpine", "1.27.4-alpine", "1.28.0", "1.28.0-alpine", "latest"}

	constrained := target("pinned", "nginx:1.27.3-alpine", fpOld, policy.TrackSemver)
	c, err := policyWith(map[string]string{"user.beacon.track": "semver", "user.beacon.constraint": "~1.27"})
	if err != nil {
		t.Fatal(err)
	}
	constrained.Policy = c

	res := run(t, f,
		target("newer", "nginx:1.27.3-alpine", fpOld, policy.TrackSemver),
		constrained,
		target("floating", "nginx:latest", fpOld, policy.TrackSemver),
	)
	if r := res[0]; r.Status != UpdateAvailable || r.Latest != "1.28.0-alpine" {
		t.Errorf("newer tag, suffix kept: %+v", r)
	}
	if r := res[1]; r.Status != UpdateAvailable || r.Latest != "1.27.4-alpine" {
		t.Errorf("constraint ~1.27: %+v", r)
	}
	if r := res[2]; r.Status != Error || !strings.Contains(r.Reason, "needs a version tag") {
		t.Errorf("semver on latest: %+v", r)
	}
	if n := f.count("tags"); n != 1 {
		t.Errorf("tags listed %d times for one repository, want 1", n)
	}
}

func TestDedupeAndCache(t *testing.T) {
	t.Parallel()
	f := newFake()
	f.set("ghcr.io/example/app:1", head1, fpOld)
	e := New(f, 2, zerolog.Nop())
	replicas := []incus.Target{
		target("app-1", "ghcr.io/example/app:1", fpOld, ""),
		target("app-2", "ghcr.io/example/app:1", fpOld, ""),
		target("app-3", "ghcr.io/example/app:1", fpOld, ""),
	}

	e.Run(t.Context(), replicas)
	if h, r := f.count("head"), f.count("resolve"); h != 1 || r != 1 {
		t.Errorf("three replicas: %d HEADs, %d GETs; want 1 each", h, r)
	}

	e.Run(t.Context(), replicas)
	if h, r := f.count("head"), f.count("resolve"); h != 2 || r != 1 {
		t.Errorf("second run, digest unchanged: %d HEADs, %d GETs; want 2 and still 1", h, r)
	}

	f.set("ghcr.io/example/app:1", head2, fpNew)
	res := e.Run(t.Context(), replicas)
	if r := f.count("resolve"); r != 2 {
		t.Errorf("digest moved: %d GETs, want a new one", r)
	}
	if res[0].Status != UpdateAvailable {
		t.Errorf("after the tag moved: %+v", res[0])
	}
}

func TestSettledWithoutRegistry(t *testing.T) {
	t.Parallel()
	f := newFake()
	bad := target("bad", "ghcr.io/example/app:1", fpOld, "")
	bad.PolicyErr = errors.New(`track "x": must be digest or semver`)
	arch := target("arch", "ghcr.io/example/app:1", fpOld, "")
	arch.Arch = "mips"
	local := target("local", "ghcr.io/example/app:1", fpOld, "")
	local.Local, local.RefSource = true, incus.RefFromCompose

	res := run(t, f,
		bad,
		target("noref", "", fpOld, ""),
		target("nobase", "ghcr.io/example/app:1", "", ""),
		target("pinned", "ghcr.io/example/app@sha256:abc", fpOld, ""),
		target("badref", "Not A Ref", fpOld, ""),
		arch,
		local,
	)
	want := []struct {
		status Status
		reason string
	}{
		{Error, "policy:"},
		{Unknown, "no registry ref"},
		{Unknown, "no base image"},
		{Unknown, "pinned by digest"},
		{Error, "invalid repository"},
		{Error, "no OCI platform"},
		{Unknown, "not pulled from a registry"},
	}
	for i, w := range want {
		if res[i].Status != w.status || !strings.Contains(res[i].Reason, w.reason) {
			t.Errorf("%s: %s %q, want %s %q", res[i].Target.Instance, res[i].Status, res[i].Reason, w.status, w.reason)
		}
	}
	if n := f.count("head") + f.count("tags"); n != 0 {
		t.Errorf("%d registry calls for targets settled locally", n)
	}
}

func TestRegistryErrors(t *testing.T) {
	t.Parallel()
	until := time.Date(2026, 11, 4, 18, 0, 0, 0, time.UTC)
	tests := []struct {
		err  error
		want string
	}{
		{&registry.RateLimitError{Host: "registry-1.docker.io", Until: until}, "rate limited until 2026-11-04T18:00:00Z"},
		{registry.ErrAuth, "registry auth failed"},
		{registry.ErrNoAccess, "not found, or private: no registry credentials"},
		{registry.ErrNotFound, "tag not found"},
		{context.DeadlineExceeded, "registry timeout"},
	}
	for _, tt := range tests {
		f := newFake()
		f.headErr = tt.err
		res := run(t, f, target("app", "ghcr.io/example/app:1", fpOld, ""))
		if res[0].Status != Error || res[0].Reason != tt.want {
			t.Errorf("%v: %s %q, want error %q", tt.err, res[0].Status, res[0].Reason, tt.want)
		}
	}

	f := newFake()
	f.set("ghcr.io/example/app:1", head1, fpOld)
	f.tagsErr = registry.ErrAuth
	res := run(t, f, target("app", "ghcr.io/example/app:1", fpOld, policy.TrackSemver))
	if res[0].Status != Error || res[0].Reason != "registry auth failed" {
		t.Errorf("tag list error: %+v", res[0])
	}
}

func policyWith(cfg map[string]string) (policy.Policy, error) {
	return policy.Parse(cfg, true)
}
