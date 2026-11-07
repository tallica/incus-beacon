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
			"user.beacon.status": "current",
			"user.beacon.error":  "old reason",
			"user.other":         "kept",
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
	delete(want, "user.beacon.error")
	want["user.beacon.status"] = "update-available"
	want["user.beacon.current"] = "ghcr.io/example/web:1"
	want["user.beacon.latest"] = "2"
	want["user.beacon.since"] = "2026-11-04T12:00:00Z"
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
