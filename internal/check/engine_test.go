package check

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
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
	created map[string]time.Time // config digest -> created
	headErr error
	tagsErr error
	calls   map[string]int // "head", "resolve", "tags"
}

func newFake() *fakeRegistry {
	return &fakeRegistry{images: map[string]image{}, tags: map[string][]string{}, created: map[string]time.Time{}, calls: map[string]int{}}
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

func (f *fakeRegistry) Created(_ context.Context, _ registry.Ref, img registry.Image) (time.Time, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls["created"]++
	return f.created[img.Config.Digest.String()], nil
}

// set gives each image a config digest equal to its head digest.
func (f *fakeRegistry) set(ref, head, fingerprint string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.images[ref] = image{head: head, img: registry.Image{
		Digest: head, ManifestDigest: head, Fingerprint: fingerprint,
		Config: ocispec.Descriptor{Digest: digest.Digest(head)},
	}}
}

// built sets the created time of the image the head digest names.
func (f *fakeRegistry) built(head string, at time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.created[head] = at
}

const (
	fpOld = "0000000000000000000000000000000000000000000000000000000000000001"
	fpNew = "0000000000000000000000000000000000000000000000000000000000000002"
	head1 = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	head2 = "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	head3 = "sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"
	head4 = "sha256:dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd"
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
	return New(f, Options{Concurrency: 4}, zerolog.Nop()).Run(t.Context(), targets)
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
		t.Errorf("local image with user.incus-beacon.image: %+v, want it checked", r)
	}
}

func TestSemver(t *testing.T) {
	t.Parallel()
	f := newFake()
	f.set("docker.io/library/nginx:1.27.3-alpine", head1, fpOld)
	f.set("docker.io/library/nginx:latest", head1, fpOld)
	f.tags["docker.io/library/nginx"] = []string{"1.27.3-alpine", "1.27.4-alpine", "1.28.0", "1.28.0-alpine", "latest"}

	constrained := target("pinned", "nginx:1.27.3-alpine", fpOld, policy.TrackSemver)
	c, err := policyWith(map[string]string{"user.incus-beacon.track": "semver", "user.incus-beacon.constraint": "~1.27"})
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
	e := New(f, Options{Concurrency: 2}, zerolog.Nop())
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

func TestComposeDrift(t *testing.T) {
	t.Parallel()
	f := newFake()
	f.set("docker.io/library/nginx:1.28", head1, fpOld)
	compose := func(name, imageRef, track string) incus.Target {
		tg := target(name, "docker.io/library/nginx:1.28", fpOld, track)
		tg.RefSource, tg.ImageRef = incus.RefFromCompose, imageRef
		return tg
	}
	updateSource := target("source", "docker.io/library/nginx:1.28", fpOld, "")
	updateSource.RefSource, updateSource.ImageRef = incus.RefFromUpdateSource, "docker.io/library/nginx:1.29"

	res := run(t, f,
		compose("same", "docker.io/library/nginx:1.28", ""),
		compose("mirror", "mirror.example.com/library/nginx:1.28", ""),
		compose("nokeys", "", ""),
		compose("retagged", "docker.io/library/nginx:1.29", policy.TrackSemver),
		compose("moved", "docker.io/example/nginx:1.28", ""),
		updateSource,
	)
	want := []struct {
		status Status
		reason string
	}{
		{Current, ""},
		{Current, ""},
		{Current, ""},
		{Error, "compose drift: runs 1.29, compose file has 1.28"},
		{Error, "compose drift: runs example/nginx:1.28, compose file has library/nginx:1.28"},
		{Current, ""},
	}
	for i, w := range want {
		if res[i].Status != w.status || res[i].Reason != w.reason || res[i].Latest != "" {
			t.Errorf("%s: %s %q latest %q, want %s %q", res[i].Target.Instance, res[i].Status, res[i].Reason, res[i].Latest, w.status, w.reason)
		}
	}
	if n := f.count("tags"); n != 0 {
		t.Errorf("%d tag lists fetched for a drifted semver instance, want 0", n)
	}
}

func TestSettledWithoutRegistry(t *testing.T) {
	t.Parallel()
	f := newFake()
	bad := target("bad", "nginx:1.28", fpOld, "")
	bad.PolicyErr = errors.New(`track "x": must be digest or semver`)
	arch := target("arch", "ghcr.io/example/app:1", fpOld, "")
	arch.Arch = "mips"
	local := target("local", "ghcr.io/example/app:1", fpOld, "")
	local.Local, local.RefSource = true, incus.RefFromCompose

	res := run(t, f,
		bad,
		target("noref", "", fpOld, ""),
		target("nobase", "nginx:1.28", "", ""),
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
	for _, i := range []int{0, 2} {
		if got := res[i].Current; got != "docker.io/library/nginx:1.28" {
			t.Errorf("%s: current %q, want the normalised ref", res[i].Target.Instance, got)
		}
	}
	if got := res[4].Current; got != "Not A Ref" {
		t.Errorf("badref: current %q, want it as given", got)
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

var now = time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)

func coolEngine(f *fakeRegistry, cooldown time.Duration) *Engine {
	e := New(f, Options{Concurrency: 4, Cooldown: cooldown}, zerolog.Nop())
	e.now = func() time.Time { return now }
	return e
}

func runCooldown(t *testing.T, f *fakeRegistry, cooldown time.Duration, targets ...incus.Target) []Result {
	t.Helper()
	return coolEngine(f, cooldown).Run(t.Context(), targets)
}

func TestCooldownSemver(t *testing.T) {
	t.Parallel()
	f := newFake()
	f.set("docker.io/library/nginx:1.27", head1, fpOld)
	f.set("docker.io/library/nginx:1.28", head2, fpNew)
	f.set("docker.io/library/nginx:1.29", head3, fpNew)
	f.set("docker.io/library/nginx:1.30", head4, fpNew)
	f.tags["docker.io/library/nginx"] = []string{"1.27", "1.28", "1.29", "1.30"}
	f.built(head2, now.Add(-30*24*time.Hour))
	f.built(head3, now.Add(-36*time.Hour))
	f.built(head4, now.Add(-2*time.Hour))
	nginx := target("web", "nginx:1.27", fpOld, policy.TrackSemver)

	e := coolEngine(f, 48*time.Hour)
	for range 2 {
		res := e.Run(t.Context(), []incus.Target{nginx})
		if r := res[0]; r.Status != UpdateAvailable || r.Latest != "1.28" ||
			r.Reason != "1.30 held by cooldown until 2026-11-19T10:00:00Z" {
			t.Errorf("48h: %+v, want 1.28 with 1.30 noted as held", r)
		}
	}
	if n := f.count("created"); n != 3 {
		t.Errorf("%d config fetches over two runs, want one per image", n)
	}

	res := runCooldown(t, f, 24*time.Hour, nginx)
	if r := res[0]; r.Status != UpdateAvailable || r.Latest != "1.29" {
		t.Errorf("24h: %+v, want 1.29", r)
	}

	res = runCooldown(t, f, 0, nginx)
	if r := res[0]; r.Status != UpdateAvailable || r.Latest != "1.30" || r.Reason != "" {
		t.Errorf("off: %+v, want 1.30", r)
	}

	res = runCooldown(t, f, 60*24*time.Hour, nginx)
	if r := res[0]; r.Status != Current || r.Latest != "" || !strings.HasPrefix(r.Reason, "1.30 held") {
		t.Errorf("none old enough: %+v, want current with a note", r)
	}

	key := nginx
	key.Policy.Cooldown, key.Policy.HasCooldown = 0, true
	res = runCooldown(t, f, 48*time.Hour, key)
	if r := res[0]; r.Latest != "1.30" {
		t.Errorf("instance key 0 under a 48h default: %+v, want the cooldown off", r)
	}
}

func TestCooldownDigest(t *testing.T) {
	t.Parallel()
	f := newFake()
	f.set("ghcr.io/example/app:1", head2, fpNew)
	f.set("ghcr.io/example/undated:1", head3, fpNew)
	f.built(head2, now.Add(-time.Hour))

	res := runCooldown(t, f, 48*time.Hour,
		target("app", "ghcr.io/example/app:1", fpOld, ""),
		target("undated", "ghcr.io/example/undated:1", fpOld, ""),
	)
	if r := res[0]; r.Status != Current || r.Latest != "" ||
		r.Reason != "1@sha256:bbbbbbbbbbbb held by cooldown until 2026-11-19T11:00:00Z" {
		t.Errorf("rebuilt an hour ago: %+v, want it held", r)
	}
	if r := res[1]; r.Status != UpdateAvailable {
		t.Errorf("no created time: %+v, want the age unknown and nothing held", r)
	}
	if b := Beacon(res[0], now); b.Status != "current" || b.Error != "" {
		t.Errorf("held update written as %+v, want a plain current", b)
	}
}

func policyWith(cfg map[string]string) (policy.Policy, error) {
	return policy.Parse(cfg, true)
}
