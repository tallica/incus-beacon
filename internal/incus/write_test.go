package incus

import (
	"context"
	"maps"
	"testing"
	"time"

	"github.com/lxc/incus/v7/shared/api"
)

func writeFixture() (*fakeIncus, Target) {
	f := &fakeIncus{instances: []api.Instance{
		instance("default", "web", oci(map[string]string{
			"user.incus-beacon.status": "current",
			"user.incus-beacon.error":  "old reason",
			"user.other":               "kept",
		}), map[string]string{"user.from-profile": "not copied"}),
	}}
	t := Target{Project: "default", Instance: "web", Beacon: Beacon{Status: "current", Error: "old reason"}}
	return f, t
}

func TestWriteOnlyOnChange(t *testing.T) {
	t.Parallel()
	f, tg := writeFixture()
	changed, err := WriteStatus(t.Context(), f.connect(t), tg, tg.Beacon)
	if err != nil || changed {
		t.Fatalf("unchanged beacon: changed %v, err %v", changed, err)
	}
	if f.count("/1.0/instances/") != 0 {
		t.Error("unchanged beacon still fetched the instance")
	}
}

func TestWriteTouchesOnlyBeaconKeys(t *testing.T) {
	t.Parallel()
	f, tg := writeFixture()
	before := maps.Clone(f.instances[0].Config)
	b := Beacon{Status: "update-available", Current: "ghcr.io/example/web:1", Latest: "2", Since: "2026-11-04T12:00:00Z"}

	changed, err := WriteStatus(t.Context(), f.connect(t), tg, b)
	if err != nil || !changed {
		t.Fatalf("changed %v, err %v", changed, err)
	}
	got := f.instances[0].Config
	want := maps.Clone(before)
	delete(want, "user.incus-beacon.error")
	want["user.incus-beacon.status"] = "update-available"
	want["user.incus-beacon.current"] = "ghcr.io/example/web:1"
	want["user.incus-beacon.latest"] = "2"
	want["user.incus-beacon.since"] = "2026-11-04T12:00:00Z"
	if !maps.Equal(got, want) {
		t.Errorf("config after write:\n got %v\nwant %v", got, want)
	}
	if _, ok := got["user.from-profile"]; ok {
		t.Error("an expanded (profile) key was written onto the instance")
	}
}

func TestWriteRetriesOnceOnETagConflict(t *testing.T) {
	t.Parallel()
	f, tg := writeFixture()
	f.conflicts = 1
	if _, err := WriteStatus(t.Context(), f.connect(t), tg, Beacon{Status: "error", Error: "x"}); err != nil {
		t.Fatalf("one conflict: %v", err)
	}
	if f.puts != 1 {
		t.Errorf("%d accepted PUTs, want 1", f.puts)
	}

	f, tg = writeFixture()
	f.conflicts = 2
	if _, err := WriteStatus(t.Context(), f.connect(t), tg, Beacon{Status: "error", Error: "x"}); err == nil {
		t.Error("two conflicts: want an error after one retry")
	}
}

func TestWriteWaitsOutABusyInstance(t *testing.T) {
	t.Parallel()
	f, tg := writeFixture()
	f.busy = 1
	if _, err := WriteStatus(t.Context(), f.connect(t), tg, Beacon{Status: "current"}); err != nil {
		t.Fatalf("busy once, as right after a launch: %v", err)
	}
	if f.puts != 1 {
		t.Errorf("%d accepted PUTs, want 1", f.puts)
	}

	f, tg = writeFixture()
	f.busy = 100
	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	if _, err := WriteStatus(ctx, f.connect(t), tg, Beacon{Status: "current"}); err == nil {
		t.Error("busy until the deadline: want an error")
	}
}

func TestReadAndMigrateLegacyStatus(t *testing.T) {
	t.Parallel()
	old := map[string]string{
		"user.beacon.status": "update-available",
		"user.beacon.latest": "2",
		"user.beacon.since":  "2026-11-04T12:00:00Z",
	}
	b, legacy := readBeacon(old)
	if want := (Beacon{Status: "update-available", Latest: "2", Since: "2026-11-04T12:00:00Z"}); b != want || !legacy {
		t.Fatalf("read %+v, legacy %v; want the old keys read", b, legacy)
	}
	mixed := maps.Clone(old)
	mixed["user.incus-beacon.status"] = "current"
	if b, legacy := readBeacon(mixed); b.Status != "current" || b.Latest != "" || !legacy {
		t.Errorf("mixed: %+v, legacy %v; want only the new keys read, still marked legacy", b, legacy)
	}

	f := &fakeIncus{instances: []api.Instance{instance("default", "web", oci(maps.Clone(old)), nil)}}
	tg := Target{Project: "default", Instance: "web", Beacon: b, LegacyStatus: true}
	changed, err := WriteStatus(t.Context(), f.connect(t), tg, b)
	if err != nil || !changed {
		t.Fatalf("unchanged beacon under old names: changed %v, err %v; want a migrating write", changed, err)
	}
	got := f.instances[0].Config
	for k := range old {
		if _, ok := got[k]; ok {
			t.Errorf("%s left after the write", k)
		}
	}
	if got["user.incus-beacon.status"] != "update-available" || got["user.incus-beacon.since"] != "2026-11-04T12:00:00Z" {
		t.Errorf("config %v, want the values under the new names, since kept", got)
	}
}

func TestTargetOfLegacyKeys(t *testing.T) {
	t.Parallel()
	f := &fakeIncus{instances: []api.Instance{instance("default", "web", oci(map[string]string{
		"user.beacon.status": "current",
		"user.beacon.track":  "semver",
	}), nil)}}
	tg, ok, err := TargetOf(t.Context(), f.connect(t), Key{Project: "default", Instance: "web"}, true)
	if err != nil || !ok {
		t.Fatalf("ok %v, err %v", ok, err)
	}
	if !tg.LegacyStatus || tg.Beacon.Status != "current" || tg.Policy.Track != "semver" || len(tg.Legacy) != 1 || tg.Legacy[0] != "user.beacon.track" {
		t.Errorf("target %+v, want the old keys read and both kinds marked legacy", tg)
	}
}
