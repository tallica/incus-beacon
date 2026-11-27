package incus

import (
	"slices"
	"strings"
	"testing"

	"github.com/lxc/incus/v7/shared/api"
)

const (
	fpPostgres = "1111111111111111111111111111111111111111111111111111111111111111"
	fpMissing  = "2222222222222222222222222222222222222222222222222222222222222222"
)

func instance(projectName, name string, local, expanded map[string]string) api.Instance {
	inst := api.Instance{Name: name, Project: projectName}
	inst.Config = local
	inst.ExpandedConfig = map[string]string{}
	for k, v := range local {
		inst.ExpandedConfig[k] = v
	}
	for k, v := range expanded {
		inst.ExpandedConfig[k] = v
	}
	return inst
}

func withArch(inst api.Instance, arch string) api.Instance {
	inst.Architecture = arch
	return inst
}

func oci(extra map[string]string) map[string]string {
	m := map[string]string{
		"volatile.container.oci": "true",
		"image.architecture":     "x86_64",
		"image.description":      "docker.io/library/example (OCI)",
	}
	for k, v := range extra {
		m[k] = v
	}
	return m
}

func fixture() *fakeIncus {
	return &fakeIncus{
		instances: []api.Instance{
			instance("default", "web", oci(map[string]string{
				"user.image_alias":  "docker.io/library/nginx:1.27",
				"image.description": "docker.io/library/nginx (OCI)",
				"image.id":          "library/nginx:1.28",

				"user.label.incus-compose.project": "shop",
				"user.label.incus-compose.service": "web",
			}), nil),
			instance("default", "db", oci(map[string]string{"volatile.base_image": fpPostgres}), nil),
			instance("default", "db2", oci(map[string]string{"volatile.base_image": fpPostgres}), nil),
			instance("default", "sys", map[string]string{"image.os": "Debian"}, nil),
			instance("apps", "api", oci(map[string]string{
				"user.incus-beacon.status":  "update-available",
				"user.incus-beacon.current": "ghcr.io/example/api:1.2.0",
				"user.incus-beacon.latest":  "1.3.0",
				"user.incus-beacon.since":   "2026-11-01T10:00:00Z",
			}), map[string]string{"user.label.incus-beacon.image": "ghcr.io/example/api:1.2.0"}),
			instance("apps", "off", oci(map[string]string{"user.incus-beacon.enable": "false"}), nil),
			instance("apps", "bad", oci(map[string]string{"user.incus-beacon.track": "weird"}), nil),
			instance("apps", "orphan", oci(map[string]string{"volatile.base_image": fpMissing}), nil),
			withArch(instance("apps", "built", map[string]string{
				"volatile.container.oci": "true",
				"volatile.base_image":    fpMissing,
				"image.description":      "ghcr.io/example/app:1.0 (built by incus-compose)",
				"user.image_alias":       "ghcr.io/example/app:1.0",
			}, nil), "aarch64"),
			instance("apps", "pinned", oci(map[string]string{
				"volatile.base_image":             fpMissing,
				"image.description":               "docker.io/library/busybox (OCI)",
				"image.id":                        "library/busybox@sha256:f97b",
				"user.incus-beacon.current":       "docker.io/library/busybox:1.37-glibc",
				"user.incus-beacon.current-image": fpMissing,
			}), nil),
			instance("apps", "repinned", oci(map[string]string{
				"volatile.base_image":             fpMissing,
				"image.description":               "docker.io/library/busybox (OCI)",
				"image.id":                        "library/busybox@sha256:f97b",
				"user.incus-beacon.current":       "docker.io/library/busybox:1.37-glibc",
				"user.incus-beacon.current-image": "an-older-fingerprint",
			}), nil),
			instance("apps", "rolled", oci(map[string]string{
				"volatile.base_image":             fpMissing,
				"image.description":               "docker.io/library/busybox (OCI)",
				"image.id":                        "library/busybox@sha256:f97b",
				"user.incus-beacon.applied":       "docker.io/library/busybox:1.36-glibc",
				"user.incus-beacon.applied-image": fpMissing,
			}), nil),
			instance("apps", "reused", oci(map[string]string{
				"volatile.base_image":             fpMissing,
				"image.description":               "docker.io/library/busybox (OCI)",
				"image.id":                        "library/busybox:1.36.1-glibc",
				"user.incus-beacon.applied":       "docker.io/library/busybox:1.36-glibc",
				"user.incus-beacon.applied-image": fpMissing,
			}), nil),
			instance("apps", "retagged", oci(map[string]string{
				"volatile.base_image":             fpMissing,
				"image.description":               "docker.io/library/busybox (OCI)",
				"image.id":                        "library/busybox:1.37-glibc",
				"user.incus-beacon.applied":       "docker.io/library/busybox:1.36-glibc",
				"user.incus-beacon.applied-image": "an-older-fingerprint",
			}), nil),
			instance("apps", "moved", oci(map[string]string{
				"volatile.base_image":             fpMissing,
				"image.description":               "docker.io/library/busybox (OCI)",
				"image.id":                        "library/busybox@sha256:f97b",
				"user.incus-beacon.applied":       "docker.io/library/busybox:1.36-glibc",
				"user.incus-beacon.applied-image": "an-older-fingerprint",
			}), nil),
			instance("apps", "pruned", oci(map[string]string{
				"volatile.base_image": fpMissing,
				"image.description":   "ghcr.io/example/worker (OCI)",
				"image.id":            "example/worker:3.1",
			}), nil),
		},
		images: map[string]api.Image{
			"default/" + fpPostgres: {UpdateSource: &api.ImageSource{
				Protocol: "oci",
				Server:   "https://docker.io",
				Alias:    "library/postgres:17@sha256:abc",
			}},
		},
	}
}

func names(ts []Target) []string {
	out := make([]string, 0, len(ts))
	for i := range ts {
		out = append(out, ts[i].Project+"/"+ts[i].Instance)
	}
	return out
}

func TestTargetsAllProjects(t *testing.T) {
	t.Parallel()
	f := fixture()
	targets, err := Targets(t.Context(), f.connect(t), ListOptions{WatchAll: true})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"apps/api",
		"apps/bad",
		"apps/built",
		"apps/moved",
		"apps/orphan",
		"apps/pinned",
		"apps/pruned",
		"apps/repinned",
		"apps/retagged",
		"apps/reused",
		"apps/rolled",
		"default/db",
		"default/db2",
		"default/web",
	}
	if got := names(targets); !slices.Equal(got, want) {
		t.Fatalf("targets %q, want %q (non-OCI and enable=false left out, sorted)", got, want)
	}

	byName := map[string]Target{}
	for _, tg := range targets {
		byName[tg.Project+"/"+tg.Instance] = tg
	}
	refs := []struct {
		name, ref, from string
	}{
		{"apps/api", "ghcr.io/example/api:1.2.0", RefFromBeacon},
		{"default/web", "docker.io/library/nginx:1.27", RefFromCompose},
		{"default/db", "docker.io/library/postgres:17", RefFromUpdateSource},
		{"apps/orphan", "", ""},
		{"apps/pruned", "ghcr.io/example/worker:3.1", RefFromImageKeys},
		// Rolled back from a fingerprint: image.id is pinned by digest, beacon's record names the tag.
		{"apps/rolled", "docker.io/library/busybox:1.36-glibc", RefFromApplied},
		{"apps/moved", "", ""},
		// Never updated, image pruned, image.id pinned: the last check's ref still names it.
		{"apps/pinned", "docker.io/library/busybox:1.37-glibc", RefFromCurrent},
		// The last check was of another image, so its ref says nothing about this one.
		{"apps/repinned", "", ""},
		// The image record's own tag can be another of the same build; beacon's record is exact.
		{"apps/reused", "docker.io/library/busybox:1.36-glibc", RefFromApplied},
		// Rebuilt by hand since: the fingerprint moved, so the image keys decide.
		{"apps/retagged", "docker.io/library/busybox:1.37-glibc", RefFromImageKeys},
	}
	for _, r := range refs {
		tg := byName[r.name]
		if tg.Ref != r.ref || tg.RefSource != r.from {
			t.Errorf("%s: ref %q from %q, want %q from %q", r.name, tg.Ref, tg.RefSource, r.ref, r.from)
		}
	}
	if w := byName["default/web"]; w.ImageRef != "docker.io/library/nginx:1.28" || w.ComposeProject != "shop" ||
		w.ComposeService != "web" {
		t.Errorf(
			"default/web: image ref %q, compose %q/%q; want the image keys' tag and the compose labels",
			w.ImageRef,
			w.ComposeProject,
			w.ComposeService,
		)
	}
	if a := byName["apps/api"]; a.ComposeProject != "" || a.ComposeService != "" {
		t.Errorf("apps/api: compose %q/%q, want none", a.ComposeProject, a.ComposeService)
	}
	if b := byName["apps/built"]; !b.Local || b.Arch != "aarch64" {
		t.Errorf("apps/built: local %t, arch %q; want local, arch from the instance", b.Local, b.Arch)
	}
	if byName["apps/api"].Local {
		t.Error("apps/api: pulled image marked local")
	}
	if byName["apps/bad"].PolicyErr == nil {
		t.Error("apps/bad: want a policy error for track=weird")
	}
	if a := byName["apps/api"]; a.Beacon.Status != "update-available" || a.Beacon.Latest != "1.3.0" ||
		a.Arch != "x86_64" {
		t.Errorf("apps/api: %+v", a)
	}
	if n := f.count("/1.0/images/" + fpPostgres); n != 1 {
		t.Errorf("image looked up %d times for two instances, want 1", n)
	}
}

func TestTargetsFilters(t *testing.T) {
	t.Parallel()
	srv := fixture().connect(t)

	targets, err := Targets(t.Context(), srv, ListOptions{Projects: []string{"apps"}, WatchAll: true})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := names(
		targets,
	), []string{
		"apps/api",
		"apps/bad",
		"apps/built",
		"apps/moved",
		"apps/orphan",
		"apps/pinned",
		"apps/pruned",
		"apps/repinned",
		"apps/retagged",
		"apps/reused",
		"apps/rolled",
	}; !slices.Equal(
		got,
		want,
	) {
		t.Errorf("projects=apps: %q, want %q", got, want)
	}

	_, err = Targets(t.Context(), srv, ListOptions{Projects: []string{"apps", "typo"}, WatchAll: true})
	if err == nil || !strings.Contains(err.Error(), `project typo: not found`) {
		t.Errorf("unknown project: err = %v, want it named as not found", err)
	}

	targets, err = Targets(t.Context(), srv, ListOptions{Instance: "web", WatchAll: true})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := names(targets), []string{"default/web"}; !slices.Equal(got, want) {
		t.Errorf("instance=web: %q, want %q", got, want)
	}

	targets, err = Targets(t.Context(), srv, ListOptions{WatchAll: false})
	if err != nil {
		t.Fatal(err)
	}
	if len(targets) != 0 {
		t.Errorf("opt-in with no enable keys: %q, want none", names(targets))
	}
}

func TestSourceRef(t *testing.T) {
	t.Parallel()
	tests := []struct {
		src  *api.ImageSource
		want string
	}{
		{nil, ""},
		{&api.ImageSource{Protocol: "simplestreams", Server: "https://images.example.com", Alias: "debian/12"}, ""},
		{&api.ImageSource{Protocol: "oci", Server: "https://ghcr.io", Alias: "example/app:2"}, "ghcr.io/example/app:2"},
		{
			&api.ImageSource{Protocol: "oci", Server: "https://registry.example.com/mirror/", Alias: "app:2"},
			"registry.example.com/mirror/app:2",
		},
		{&api.ImageSource{Protocol: "oci", Server: "not a url", Alias: "app:2"}, ""},
		{
			&api.ImageSource{Protocol: "oci", Server: "https://docker.io", Alias: "library/postgres:17@sha256:abc"},
			"docker.io/library/postgres:17",
		},
		{&api.ImageSource{Protocol: "oci", Server: "https://docker.io", Alias: "library/postgres@sha256:abc"}, ""},
		{
			&api.ImageSource{Protocol: "oci", Server: "https://docker.io", Alias: "library/postgres"},
			"docker.io/library/postgres",
		},
	}
	for _, tt := range tests {
		if got := sourceRef(tt.src); got != tt.want {
			t.Errorf("sourceRef(%+v) = %q, want %q", tt.src, got, tt.want)
		}
	}
}

func TestImageKeysRef(t *testing.T) {
	t.Parallel()
	tests := []struct {
		desc, id, want string
	}{
		{"docker.io/example/app (OCI)", "example/app:2", "docker.io/example/app:2"},
		{"registry.example.com:5000/app (OCI)", "app:1.0", "registry.example.com:5000/app:1.0"},
		{"ghcr.io/example/app (OCI)", "example/app@sha256:abc", ""},
		{"ghcr.io/example/app (OCI)", "example/app", ""},
		{"Debian bookworm", "debian/12:x", ""},
		{"", "example/app:2", ""},
	}
	for _, tt := range tests {
		got := imageKeysRef(map[string]string{"image.description": tt.desc, "image.id": tt.id})
		if got != tt.want {
			t.Errorf("imageKeysRef(%q, %q) = %q, want %q", tt.desc, tt.id, got, tt.want)
		}
	}
}

func TestNormalizeFingerprint(t *testing.T) {
	t.Parallel()
	if got := normalizeFingerprint(" AB:cd:EF "); got != "abcdef" {
		t.Errorf("normalizeFingerprint = %q", got)
	}
}

func TestTrusted(t *testing.T) {
	t.Parallel()
	if err := trusted(fixture().connect(t)); err != nil {
		t.Errorf("trusted server: %v", err)
	}
	f := fixture()
	f.untrusted = true
	if err := trusted(
		f.connect(t),
	); err == nil ||
		!strings.Contains(err.Error(), "doesn't trust this client certificate") {
		t.Errorf("untrusted server: err %v", err)
	}
}

func TestAppliedRef(t *testing.T) {
	t.Parallel()
	const file, newer = "docker.io/library/nginx:1.27", "docker.io/library/nginx:1.28"
	moved := Target{Update: Update{Requested: file, Applied: newer}}
	tests := []struct {
		name    string
		t       Target
		asked   string
		ref     string
		applied bool
	}{
		{"never updated", Target{}, file, file, false},
		{"updated, file unchanged", moved, file, newer, true},
		{"updated, the same ref spelled short", moved, "nginx:1.27", newer, true},
		{"file moved on", moved, "docker.io/library/nginx:1.29", "docker.io/library/nginx:1.29", false},
	}
	for _, tt := range tests {
		if ref, ok := applied(tt.t, tt.asked); ref != tt.ref || ok != tt.applied {
			t.Errorf("%s: %q, %t; want %q, %t", tt.name, ref, ok, tt.ref, tt.applied)
		}
	}
}

func TestComposeOverrideAfterARecreate(t *testing.T) {
	t.Parallel()
	const file, override = "docker.io/library/nginx:1.27", "docker.io/library/nginx:1.28"
	resolve := func(cfg map[string]string) Target {
		cfg["image.type"], cfg["user.image_alias"], cfg["user.incus-compose.managed"] = "oci", file, "true"
		inst := &api.Instance{Name: "web-1", InstancePut: api.InstancePut{Config: cfg}, ExpandedConfig: cfg}
		tg, ok := target(nil, inst, true, map[string]*api.ImageSource{})
		if !ok {
			t.Fatal("not a target")
		}
		return tg
	}
	// Recreated by incus-compose on the override: beacon's own keys are gone.
	tg := resolve(map[string]string{ComposeOverrideKey: override, ComposeOverrideOfKey: file})
	if tg.Ref != override || !tg.Applied || tg.Asked != file || tg.RefSource != RefFromCompose {
		t.Errorf("in force: %+v", tg)
	}
	tg = resolve(map[string]string{ComposeOverrideKey: override, ComposeOverrideOfKey: "docker.io/library/nginx:1.26"})
	if tg.Ref != file || tg.Applied {
		t.Errorf("set against an older file ref: %+v; want the file's ref", tg)
	}
	// An image key from a compose label survives the recreate; the override still decides.
	tg = resolve(
		map[string]string{
			"user.label.incus-beacon.image": file,
			ComposeOverrideKey:              override,
			ComposeOverrideOfKey:            file,
		},
	)
	if tg.Ref != override || !tg.Applied || tg.RefSource != RefFromBeacon {
		t.Errorf("with an image label: %+v", tg)
	}
	// incus-compose's override is what it will run, so it wins over beacon's own record.
	tg = resolve(map[string]string{
		"user.incus-beacon.requested": file, "user.incus-beacon.applied": "docker.io/library/nginx:1.29",
		ComposeOverrideKey: override, ComposeOverrideOfKey: file,
	})
	if tg.Ref != override {
		t.Errorf("override and applied disagree: ref %q, want the override", tg.Ref)
	}
}

func TestReadBeaconCurrentImageUnderLegacyNames(t *testing.T) {
	t.Parallel()
	b, legacy := readBeacon(map[string]string{
		"user.beacon.status":              "current",
		"user.beacon.current":             "docker.io/library/nginx:1.28",
		"user.incus-beacon.current-image": "fp-1", // a key with no old name
	})
	if !legacy || b.Current != "docker.io/library/nginx:1.28" || b.CurrentImage != "fp-1" {
		t.Errorf("%+v, legacy %t", b, legacy)
	}
}
