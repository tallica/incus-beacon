package update

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"net/http"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	incusclient "github.com/lxc/incus/v7/client"
	"github.com/lxc/incus/v7/shared/api"
	"github.com/rs/zerolog"

	"github.com/tallica/incus-beacon/internal/check"
	"github.com/tallica/incus-beacon/internal/incus"
	"github.com/tallica/incus-beacon/internal/registry"
)

const (
	oldRef = "docker.io/library/nginx:1.27-alpine"
	newRef = "docker.io/library/nginx:1.28-alpine"
)

type fakeOp struct{ incusclient.Operation }

// hungOp never finishes on its own, like an image pull that stalls.
type hungOp struct{ incusclient.Operation }

func (hungOp) WaitContext(ctx context.Context) error { <-ctx.Done(); return ctx.Err() }

// lockedOp fails as Incus does when another write held the instance's update lock.
type lockedOp struct{ incusclient.Operation }

func (lockedOp) WaitContext(context.Context) error {
	return errors.New("Failed to create instance update operation: A matching non-reusable operation has now succeeded")
}

func (fakeOp) Wait() error                       { return nil }
func (fakeOp) WaitContext(context.Context) error { return nil }

// fake is one instance "web" on a server; starts and health follow scripts.
type fake struct {
	incusclient.InstanceServer
	version   string
	inst      api.Instance
	snapshots []string
	volumes   map[string]*api.StorageVolume // pool/name
	volSnaps  map[string][]string
	restored  []string
	rebuilds  []api.InstanceSource
	states    []string
	starts    int
	// crash reports whether the n-th start (from 1) leaves the instance stopped.
	crash func(n int) bool
	// health is what ic-healthd reports after the n-th start; nil means no health check.
	health func(n int) string
	etag   int
	// meddle, when set, runs before the n-th config write (from 1), as another writer would.
	meddle            func(n int, cfg map[string]string)
	writes            int
	profile           map[string]string // keys from profiles: in the expanded config only
	overrideAtRebuild string            // incus-compose's override when the new image was rebuilt
	// claimAtRebuild is the claim's step when the new image was rebuilt.
	claimAtRebuild int
	// stuck makes a graceful stop fail, as Incus does when the timeout runs out.
	stuck    bool
	lockedAt int // the write (from 1) refused once as held up by another update
	// lockedRebuild is the rebuild (from 1) whose oci.* refresh another write's lock skips;
	// lockedEarly fails it before the rootfs changes instead.
	lockedRebuild int
	lockedEarly   bool
	ociNew        string // oci.entrypoint the new image's rebuild refreshes to
	snapFail      string // pool/name whose snapshot fails
	hang          bool   // the rebuild onto the new image never finishes
	hangDel       bool   // snapshot deletes never finish
}

func newFake() *fake {
	return &fake{
		version: "7.6",
		inst: api.Instance{
			Name:       "web",
			Status:     "Running",
			StatusCode: api.Running,
			InstancePut: api.InstancePut{
				Config: map[string]string{
					"volatile.base_image":         "fp-old",
					"environment.NGINX_VERSION":   "1.27.5",
					"environment.FOO":             "bar",
					"user.incus-beacon.status":    "update-available",
					"user.incus-beacon.unrelated": "kept",
				},
				Devices: map[string]map[string]string{
					"data": {"type": "disk", "pool": "default", "source": "web-data", "path": "/data"},
				},
			},
			ExpandedDevices: map[string]map[string]string{
				"root": {"type": "disk", "pool": "default", "path": "/"},
				"data": {"type": "disk", "pool": "default", "source": "web-data", "path": "/data"},
				"host": {"type": "disk", "source": "/srv/web", "path": "/srv"},
			},
		},
		volumes: map[string]*api.StorageVolume{
			"default/web-data": {Name: "web-data", UsedBy: []string{"/1.0/instances/web"}},
		},
		volSnaps: map[string][]string{
			"default/web-data": {"incus-beacon-20260101T000000", "incus-beacon-20260102T000000", "manual", "incus-beacon-20260103T000000"},
		},
		crash: func(int) bool { return false },
	}
}

func (f *fake) UseProject(string) incusclient.InstanceServer { return f }

func (f *fake) GetServer() (*api.Server, string, error) {
	return &api.Server{Environment: api.ServerEnvironment{ServerVersion: f.version}}, "", nil
}

func (f *fake) GetInstance(string) (*api.Instance, string, error) {
	inst := f.inst
	inst.Config = maps.Clone(f.inst.Config)
	inst.ExpandedConfig = maps.Clone(f.profile)
	if inst.ExpandedConfig == nil {
		inst.ExpandedConfig = map[string]string{}
	}
	maps.Copy(inst.ExpandedConfig, f.inst.Config)
	return &inst, strconv.Itoa(f.etag), nil
}

func (f *fake) UpdateInstance(_ string, put api.InstancePut, etag string) (incusclient.Operation, error) {
	f.writes++
	if f.writes == f.lockedAt {
		return nil, errors.New("Failed to create instance update operation: A matching non-reusable operation has now succeeded")
	}
	if f.meddle != nil {
		before := maps.Clone(f.inst.Config)
		if f.meddle(f.writes, f.inst.Config); !maps.Equal(before, f.inst.Config) {
			f.etag++
		}
	}
	if etag != strconv.Itoa(f.etag) {
		return nil, api.StatusErrorf(http.StatusPreconditionFailed, "ETag doesn't match")
	}
	f.inst.Config = put.Config
	f.etag++
	return fakeOp{}, nil
}

func (f *fake) GetInstanceSnapshotNames(string) ([]string, error) { return f.snapshots, nil }

func (f *fake) UpdateInstanceState(_ string, st api.InstanceStatePut, _ string) (incusclient.Operation, error) {
	f.states = append(f.states, st.Action)
	switch st.Action {
	case "stop":
		if f.stuck && !st.Force {
			return nil, errors.New(`Failed shutting down instance, status is "Running"`)
		}
		f.inst.Status, f.inst.StatusCode = "Stopped", api.Stopped
		if f.health != nil {
			f.inst.Config[healthStatus] = "stopped"
		}
	case "start":
		f.starts++
		f.inst.Status, f.inst.StatusCode = "Running", api.Running
		if f.crash(f.starts) {
			f.inst.Status, f.inst.StatusCode = "Stopped", api.Stopped
		}
		if f.health != nil {
			f.inst.Config[healthStatus] = f.health(f.starts)
		}
	}
	return fakeOp{}, nil
}

func (f *fake) RebuildInstance(_ string, req api.InstanceRebuildPost) (incusclient.Operation, error) {
	f.rebuilds = append(f.rebuilds, req.Source)
	var c claim
	if json.Unmarshal([]byte(f.inst.Config[incus.ClaimKey]), &c) == nil && req.Source.Fingerprint == "" {
		f.claimAtRebuild = c.Step
	}
	if req.Source.Fingerprint == "" {
		f.overrideAtRebuild = f.inst.Config[incus.ComposeOverrideKey]
	}
	if f.hang && req.Source.Fingerprint == "" {
		return hungOp{}, nil
	}
	locked := len(f.rebuilds) == f.lockedRebuild
	if locked && f.lockedEarly {
		return lockedOp{}, nil
	}
	if req.Source.Fingerprint != "" {
		f.inst.Config["volatile.base_image"] = req.Source.Fingerprint
	} else {
		f.inst.Config["volatile.base_image"] = "fp-new"
	}
	if locked {
		return lockedOp{}, nil
	}
	if f.ociNew != "" && req.Source.Fingerprint == "" {
		f.inst.Config["oci.entrypoint"] = f.ociNew
	}
	return fakeOp{}, nil
}

func (f *fake) GetStoragePoolVolume(pool, _, name string) (*api.StorageVolume, string, error) {
	v, ok := f.volumes[pool+"/"+name]
	if !ok {
		return nil, "", errors.New("not found")
	}
	return v, "", nil
}

func (f *fake) UpdateStoragePoolVolume(pool, _, name string, put api.StorageVolumePut, _ string) error {
	f.restored = append(f.restored, pool+"/"+name+"/"+put.Restore)
	return nil
}

func (f *fake) CreateStoragePoolVolumeSnapshot(pool, _, name string, req api.StorageVolumeSnapshotsPost) (incusclient.Operation, error) {
	if pool+"/"+name == f.snapFail {
		return nil, errors.New("no space left")
	}
	f.volSnaps[pool+"/"+name] = append(f.volSnaps[pool+"/"+name], req.Name)
	return fakeOp{}, nil
}

func (f *fake) GetStoragePoolVolumeSnapshotNames(pool, _, name string) ([]string, error) {
	return f.volSnaps[pool+"/"+name], nil
}

func (f *fake) DeleteStoragePoolVolumeSnapshot(pool, _, name, snap string) (incusclient.Operation, error) {
	k := pool + "/" + name
	if f.hangDel {
		return hungOp{}, nil
	}
	f.volSnaps[k] = slices.DeleteFunc(f.volSnaps[k], func(s string) bool { return s == snap })
	return fakeOp{}, nil
}

type fakeRegistry struct{}

func (fakeRegistry) Resolve(_ context.Context, ref registry.Ref, _ registry.Platform) (registry.Image, error) {
	fp := map[string]string{"1.27-alpine": "fp-old", "1.28-alpine": "fp-new"}[ref.Tag]
	return registry.Image{Fingerprint: fp}, nil
}

func (fakeRegistry) Env(_ context.Context, ref registry.Ref, _ registry.Image) ([]string, error) {
	return map[string][]string{
		"1.27-alpine": {"PATH=/usr/bin", "NGINX_VERSION=1.27.5"},
		"1.28-alpine": {"PATH=/usr/bin", "NGINX_VERSION=1.28.3"},
	}[ref.Tag], nil
}

func target() incus.Target {
	return incus.Target{Project: "default", Instance: "web", Arch: "x86_64", BaseImage: "fp-old", Ref: oldRef, ImageRef: oldRef}
}

func options() Options {
	at := time.Date(2026, 11, 21, 3, 0, 0, 0, time.UTC)
	return Options{
		Owner: "test/1", Keep: 3, Settle: time.Millisecond, Wait: 200 * time.Millisecond, StopTimeout: 50 * time.Millisecond, Every: time.Millisecond,
		Now: func() time.Time { at = at.Add(time.Second); return at }, Log: zerolog.Nop(),
	}
}

func apply(t *testing.T, f *fake, tgt incus.Target) Outcome {
	t.Helper()
	o, err := Apply(context.Background(), f, fakeRegistry{}, tgt, Next{Ref: newRef}, options())
	if err != nil {
		t.Fatal(err)
	}
	return o
}

func TestApplyPinsDigest(t *testing.T) {
	t.Parallel()
	const pin = "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	f := newFake()
	o, err := Apply(context.Background(), f, fakeRegistry{}, target(), Next{Ref: newRef, Digest: pin}, options())
	if err != nil || o.Status != Updated {
		t.Fatalf("outcome %+v, %v", o, err)
	}
	want := []api.InstanceSource{{Type: "image", Protocol: "oci", Server: "https://docker.io", Alias: "library/nginx:1.28-alpine@" + pin}}
	if !reflect.DeepEqual(f.rebuilds, want) {
		t.Errorf("rebuilds %+v, want %+v", f.rebuilds, want)
	}
	if o.To != newRef || f.inst.Config["user.incus-beacon.applied"] != newRef {
		t.Errorf("recorded %q and %q, want the tag-only ref %q", o.To, f.inst.Config["user.incus-beacon.applied"], newRef)
	}
}

func TestApplyUpdates(t *testing.T) {
	t.Parallel()
	f := newFake()
	o := apply(t, f, target())
	if o.Status != Updated {
		t.Fatalf("outcome %+v", o)
	}
	want := []api.InstanceSource{{Type: "image", Protocol: "oci", Server: "https://docker.io", Alias: "library/nginx:1.28-alpine"}}
	if !reflect.DeepEqual(f.rebuilds, want) {
		t.Errorf("rebuilds %+v, want %+v", f.rebuilds, want)
	}
	if got := strings.Join(f.states, " "); got != "stop start" {
		t.Errorf("states %q", got)
	}
	cfg := f.inst.Config
	if _, ok := cfg["environment.NGINX_VERSION"]; ok || cfg["environment.FOO"] != "bar" {
		t.Errorf("environment: the old image's value should go and the override stay: %v", cfg)
	}
	if cfg["user.incus-beacon.applied"] != newRef || cfg["user.incus-beacon.requested"] != oldRef || cfg["user.incus-beacon.update-status"] != Updated ||
		cfg["user.incus-beacon.applied-image"] != "fp-new" {
		t.Errorf("update keys %v", cfg)
	}
	snaps := f.volSnaps["default/web-data"]
	if len(o.Snapshots) != 1 || !slices.Contains(snaps, "manual") || len(snaps) != 4 || slices.Contains(snaps, "incus-beacon-20260101T000000") {
		t.Errorf("snapshots %v after %v, want the oldest of four of beacon's pruned and manual kept", snaps, o.Snapshots)
	}
	if len(f.restored) != 0 {
		t.Errorf("restored %v on success", f.restored)
	}
	if _, ok := cfg[incus.ClaimKey]; ok || stepName(f.claimAtRebuild) != "rebuild" {
		t.Errorf("claim at rebuild: step %q, want rebuild; after: %q, want none", stepName(f.claimAtRebuild), cfg[incus.ClaimKey])
	}
}

func stepName(i int) string {
	steps := (&updater{}).steps()
	if i < 0 || i >= len(steps) {
		return ""
	}
	return steps[i].name
}

func stepIndex(t *testing.T, name string) int {
	t.Helper()
	for i, s := range (&updater{}).steps() {
		if s.name == name {
			return i
		}
	}
	t.Fatalf("no step %q", name)
	return 0
}

func claimJSON(t *testing.T, c claim) string {
	t.Helper()
	b, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestApplyRefusesWhileClaimed(t *testing.T) {
	t.Parallel()
	f := newFake()
	f.inst.Config[incus.ClaimKey] = claimJSON(t, claim{Owner: "other/7", Expires: time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC), To: newRef})
	o := apply(t, f, target())
	if o.Status != Refused || !strings.Contains(o.Reason, "being updated by other/7") || len(f.states) != 0 {
		t.Errorf("outcome %+v, states %v", o, f.states)
	}
	if f.inst.Config[incus.ClaimKey] == "" || f.inst.Config["user.incus-beacon.update-status"] != "" {
		t.Errorf("config %v; want the other process's claim kept and nothing written", f.inst.Config)
	}
}

func TestPreviewAnAbandonedClaim(t *testing.T) {
	t.Parallel()
	expired := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for _, tt := range []struct {
		step   int
		status string
		reason string
	}{
		{stepIndex(t, "rebuild"), Recover, "by other/7 was interrupted at rebuild"},
		{-1, Refused, "by other/7 was interrupted before its first step"},
	} {
		f := newFake()
		raw := claimJSON(t, claim{Owner: "other/7", Expires: expired, Step: tt.step, To: newRef, FromRef: oldRef, From: "fp-old"})
		f.inst.Config[incus.ClaimKey] = raw
		o := Preview(context.Background(), f, fakeRegistry{}, target(), Next{}, options())
		if o.Status != tt.status || !strings.Contains(o.Reason, tt.reason) || o.From != oldRef || o.To != newRef {
			t.Errorf("step %d: outcome %+v", tt.step, o)
		}
		if f.inst.Config[incus.ClaimKey] != raw || len(f.states) != 0 || len(f.rebuilds) != 0 {
			t.Errorf("step %d: preview changed the instance: config %v, states %v", tt.step, f.inst.Config, f.states)
		}
	}
}

func TestApplyRecoversAnAbandonedClaim(t *testing.T) {
	t.Parallel()
	f := newFake()
	cfg := f.inst.Config
	cfg["volatile.base_image"] = "fp-new"
	delete(cfg, "environment.NGINX_VERSION")
	cfg["oci.entrypoint"] = "set by the new image"
	cfg[incus.ClaimKey] = claimJSON(t, claim{
		Owner: "other/7", Expires: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), Step: stepIndex(t, "start"), To: newRef,
		FromRef: oldRef, From: "fp-old",
		Snapshot: "incus-beacon-20261120T000000", Volumes: []string{"default/web-data"},
		Dropped: map[string]string{"environment.NGINX_VERSION": "1.27.5"},
	})
	o := apply(t, f, target())
	if o.Status != RolledBack || !strings.Contains(o.Reason, "by other/7 was interrupted at start") || o.From != oldRef {
		t.Fatalf("outcome %+v", o)
	}
	if !reflect.DeepEqual(f.rebuilds, []api.InstanceSource{{Type: "image", Fingerprint: "fp-old"}}) {
		t.Errorf("rebuilds %+v, want only the old fingerprint", f.rebuilds)
	}
	if !reflect.DeepEqual(f.restored, []string{"default/web-data/incus-beacon-20261120T000000"}) {
		t.Errorf("restored %v", f.restored)
	}
	cfg = f.inst.Config
	if cfg["environment.NGINX_VERSION"] != "1.27.5" || cfg[incus.ClaimKey] != "" || cfg["user.incus-beacon.update-failed"] != newRef {
		t.Errorf("config after recovery: %v", cfg)
	}
	if f.inst.StatusCode != api.Running {
		t.Errorf("instance %s after recovery", f.inst.Status)
	}
	if cfg["oci.entrypoint"] != "set by the new image" {
		t.Errorf("oci.entrypoint %q: a claim from an older process records no oci.* keys to put back", cfg["oci.entrypoint"])
	}
}

func TestApplyRollbackFinishesARebuildAnotherWriteHeldUp(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		crash         bool
		lockedRebuild int
		early         bool
		want          string
	}{
		"undo rebuild held up":              {crash: true, lockedRebuild: 2, want: RolledBack},
		"forward rebuild held up":           {lockedRebuild: 1, want: RolledBack},
		"undo rebuild held up, not rebuilt": {crash: true, lockedRebuild: 2, early: true, want: Failed},
	} {
		f := newFake()
		f.inst.Config["oci.entrypoint"] = "nginx-old"
		f.inst.Config["oci.cwd"] = "/srv"
		f.ociNew = "nginx-new"
		f.crash = func(n int) bool { return tc.crash && n == 1 }
		f.lockedRebuild, f.lockedEarly = tc.lockedRebuild, tc.early
		o := apply(t, f, target())
		if o.Status != tc.want {
			t.Errorf("%s: outcome %+v, want %s", name, o, tc.want)
			continue
		}
		if tc.want != RolledBack {
			continue
		}
		if cfg := f.inst.Config; cfg["oci.entrypoint"] != "nginx-old" || cfg["oci.cwd"] != "/srv" || cfg["volatile.base_image"] != "fp-old" {
			t.Errorf("%s: config %v, want the old image with its oci.* keys", name, cfg)
		}
		if f.inst.StatusCode != api.Running {
			t.Errorf("%s: instance %s, want running", name, f.inst.Status)
		}
	}
}

func TestApplyLosesARaceForTheClaim(t *testing.T) {
	t.Parallel()
	f := newFake()
	f.meddle = func(n int, cfg map[string]string) {
		if n == 1 {
			cfg[incus.ClaimKey] = `{"owner":"other/7","expires":"2027-01-01T00:00:00Z"}`
		}
	}
	o := apply(t, f, target())
	if o.Status != Refused || !strings.Contains(o.Reason, "claimed it first") || len(f.states) != 0 {
		t.Errorf("outcome %+v, states %v", o, f.states)
	}
}

func TestApplyStopsWhenItsClaimIsTaken(t *testing.T) {
	t.Parallel()
	f := newFake()
	f.meddle = func(n int, cfg map[string]string) {
		if n == 2 {
			cfg[incus.ClaimKey] = `{"owner":"other/7","expires":"2027-01-01T00:00:00Z"}`
		}
	}
	o := apply(t, f, target())
	if o.Status != Failed || !strings.Contains(o.Reason, "taken over by other/7") || len(f.states) != 0 || len(f.rebuilds) != 0 {
		t.Errorf("outcome %+v, states %v, rebuilds %v; want hands off", o, f.states, f.rebuilds)
	}
}

func TestApplyRollsBackWhenTheInstanceStops(t *testing.T) {
	t.Parallel()
	f := newFake()
	f.crash = func(n int) bool { return n == 1 }
	o := apply(t, f, target())
	if o.Status != RolledBack || !strings.Contains(o.Reason, "stopped after the start") {
		t.Fatalf("outcome %+v", o)
	}
	if len(f.rebuilds) != 2 || !reflect.DeepEqual(f.rebuilds[1], api.InstanceSource{Type: "image", Fingerprint: "fp-old"}) {
		t.Errorf("rebuilds %+v, want the new image, then the old fingerprint", f.rebuilds)
	}
	if len(f.restored) != 1 || !strings.HasPrefix(f.restored[0], "default/web-data/incus-beacon-") {
		t.Errorf("restored %v, want the volume from the new snapshot", f.restored)
	}
	cfg := f.inst.Config
	if cfg["environment.NGINX_VERSION"] != "1.27.5" {
		t.Errorf("dropped environment key not put back: %v", cfg)
	}
	if cfg["user.incus-beacon.update-status"] != RolledBack || cfg["user.incus-beacon.update-failed"] != newRef ||
		cfg["user.incus-beacon.applied"] != oldRef || cfg["user.incus-beacon.applied-image"] != "fp-old" {
		t.Errorf("update keys %v", cfg)
	}
	if snaps := f.volSnaps["default/web-data"]; len(snaps) != 4 || slices.Contains(snaps, "incus-beacon-20260101T000000") {
		t.Errorf("snapshots %v; want a rollback to prune the oldest of beacon's too", snaps)
	}
}

func withHealth(f *fake, report func(n int) string) *fake {
	f.inst.Config[healthEnabled] = "true"
	f.inst.Config[healthStatus] = "healthy"
	f.health = report
	return f
}

func TestApplyRollsBackWhenUnhealthy(t *testing.T) {
	t.Parallel()
	f := withHealth(newFake(), func(n int) string {
		if n == 1 {
			return "unhealthy"
		}
		return "healthy"
	})
	if o := apply(t, f, target()); o.Status != RolledBack || !strings.Contains(o.Reason, "unhealthy") {
		t.Errorf("outcome %+v", o)
	}
}

func TestApplyWaitsForHealthy(t *testing.T) {
	t.Parallel()
	f := withHealth(newFake(), func(int) string { return "healthy" })
	if o := apply(t, f, target()); o.Status != Updated {
		t.Errorf("outcome %+v", o)
	}
	f = withHealth(newFake(), func(int) string { return "starting" })
	if o := apply(t, f, target()); o.Status != Failed || !strings.Contains(o.Reason, "not healthy within") {
		t.Errorf("never healthy: outcome %+v, want failed, since the rollback can't get healthy either", o)
	}
}

func TestApplyNeedsTheHealthCheckToSeeTheStop(t *testing.T) {
	t.Parallel()
	f := withHealth(newFake(), nil) // ic-healthd is down: the old "healthy" stays
	o := apply(t, f, target())
	if o.Status != RolledBack || !strings.Contains(o.Reason, "did not report the stop") || len(f.rebuilds) != 0 {
		t.Errorf("outcome %+v, rebuilds %v; want a restart on the old image, never trusting the old status", o, f.rebuilds)
	}
}

func TestApplyFailsWhenTheRollbackFails(t *testing.T) {
	t.Parallel()
	f := newFake()
	f.crash = func(int) bool { return true }
	tgt := target()
	tgt.Update = incus.Update{Applied: "docker.io/library/nginx:1.26-alpine", AppliedImage: "fp-older", Status: Updated}
	o := apply(t, f, tgt)
	if o.Status != Failed || !strings.Contains(o.Reason, "roll back stop: the instance is stopped") {
		t.Errorf("outcome %+v", o)
	}
	if cfg := f.inst.Config; cfg["user.incus-beacon.applied"] != "docker.io/library/nginx:1.26-alpine" || cfg["user.incus-beacon.applied-image"] != "fp-older" {
		t.Errorf("a failed rollback leaves the instance unknown, so the record stays: %v", cfg)
	}
	if f.inst.Config["user.incus-beacon.update-status"] != Failed {
		t.Errorf("update keys %v", f.inst.Config)
	}
	if snaps := f.volSnaps["default/web-data"]; len(snaps) != 5 {
		t.Errorf("snapshots %v; want every one kept after a failed rollback", snaps)
	}
}

func TestApplyLeavesSharedVolumes(t *testing.T) {
	t.Parallel()
	f := newFake()
	f.volumes["default/web-data"].UsedBy = []string{"/1.0/instances/web", "/1.0/instances/worker"}
	f.crash = func(n int) bool { return n == 1 }
	o := apply(t, f, target())
	if o.Status != RolledBack || len(f.restored) != 0 || !reflect.DeepEqual(o.Unrestored, []string{"default/web-data"}) {
		t.Errorf("outcome %+v, restored %v; want the shared volume left alone", o, f.restored)
	}
}

func TestApplyRefuses(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		change func(*fake, *incus.Target)
		reason string
	}{
		{"compose without an image alias", func(f *fake, _ *incus.Target) { f.inst.Config["user.incus-compose.managed"] = "true" }, "no user.image_alias"},
		{"beacon", func(f *fake, _ *incus.Target) { f.inst.Config["environment.BEACON_TOKEN_FILE"] = "/run/secrets/token" }, "incus-beacon upgrade"},
		{"failed before", func(_ *fake, t *incus.Target) { t.Update.Failed = newRef }, "failed before"},
		{"stopped", func(f *fake, _ *incus.Target) { f.inst.Status, f.inst.StatusCode = "Stopped", api.Stopped }, "is stopped"},
		{"snapshots", func(f *fake, _ *incus.Target) { f.snapshots = []string{"snap0"} }, "snap0"},
		{"old server", func(f *fake, _ *incus.Target) { f.version = "7.4" }, "7.6 or later"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := newFake()
			tgt := target()
			tt.change(f, &tgt)
			o := apply(t, f, tgt)
			if o.Status != Refused || !strings.Contains(o.Reason, tt.reason) {
				t.Errorf("outcome %+v, want refused with %q", o, tt.reason)
			}
			if len(f.states) != 0 || len(f.rebuilds) != 0 {
				t.Errorf("refused, yet states %v and rebuilds %v", f.states, f.rebuilds)
			}
			if f.inst.Config["user.incus-beacon.update-status"] != Refused {
				t.Errorf("update keys %v", f.inst.Config)
			}
		})
	}
}

func TestOutcomeSpellsBothRefsOut(t *testing.T) {
	t.Parallel()
	tgt := target()
	tgt.Ref = "docker.io/nginx:1.27-alpine" // as an image record's update_source spells it
	o := Preview(context.Background(), newFake(), fakeRegistry{}, tgt, Next{Ref: newRef}, options())
	if o.From != oldRef || o.To != newRef {
		t.Errorf("from %q to %q; want both as Docker spells them out", o.From, o.To)
	}
}

func TestApplyRecordsRefsSpelledOut(t *testing.T) {
	t.Parallel()
	raw := target()
	raw.Ref = "docker.io/nginx:1.27-alpine"
	f := newFake()
	if o := apply(t, f, raw); o.Status != Updated || f.inst.Config["user.incus-beacon.requested"] != oldRef {
		t.Errorf("update: %+v, requested %q; want it spelled out", o, f.inst.Config["user.incus-beacon.requested"])
	}
	f = newFake()
	f.crash = func(n int) bool { return n == 1 }
	if o := apply(t, f, raw); o.Status != RolledBack || f.inst.Config["user.incus-beacon.applied"] != oldRef {
		t.Errorf("rollback: %+v, applied %q; want it spelled out", o, f.inst.Config["user.incus-beacon.applied"])
	}
}

func TestNextOf(t *testing.T) {
	t.Parallel()
	const pin = "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	for latest, want := range map[string]string{
		"1.28-alpine":                     newRef,
		"1.27-alpine@sha256:0123456789ab": oldRef,
	} {
		got, err := NextOf(check.Result{Current: oldRef, Latest: latest, LatestDigest: pin})
		if err != nil || got != (Next{Ref: want, Digest: pin}) {
			t.Errorf("NextOf(%q) = %+v, %v; want %q at %s", latest, got, err, want, pin)
		}
	}
	if _, err := NextOf(check.Result{Current: oldRef}); err == nil {
		t.Error("an empty latest was accepted")
	}
}

func TestAtLeast(t *testing.T) {
	t.Parallel()
	for v, want := range map[string]bool{"7.6": true, "7.6.1": true, "7.10": true, "8.0": true, "7.5": false, "6.20": false, "7.6-rc1": true, "": false, "x": false} {
		if got := atLeast(v, 7, 6); got != want {
			t.Errorf("atLeast(%q) = %t, want %t", v, got, want)
		}
	}
}

func TestApplyRecoveryBeforeAnyStepBlamesNoImage(t *testing.T) {
	t.Parallel()
	f := newFake()
	f.inst.Config[incus.ClaimKey] = claimJSON(t, claim{Owner: "other/7", Expires: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), Step: -1, To: newRef, From: "fp-old"})
	o := apply(t, f, target())
	cfg := f.inst.Config
	if o.Status != Refused || cfg[incus.ClaimKey] != "" || cfg["user.incus-beacon.update-failed"] != "" || len(f.states) != 0 {
		t.Errorf("outcome %+v, config %v, states %v; want the claim cleared and the ref not marked failed", o, cfg, f.states)
	}
}

func TestApplyRollsBackAFailedSnapshot(t *testing.T) {
	t.Parallel()
	f := newFake()
	f.inst.ExpandedDevices["logs"] = map[string]string{"type": "disk", "pool": "default", "source": "web-logs", "path": "/logs"}
	f.volumes["default/web-logs"] = &api.StorageVolume{Name: "web-logs", UsedBy: []string{"/1.0/instances/web"}}
	f.snapFail = "default/web-logs"
	o := apply(t, f, target())
	if o.Status != RolledBack || !strings.Contains(o.Reason, "snapshot: default/web-logs") {
		t.Fatalf("outcome %+v", o)
	}
	if len(f.rebuilds) != 0 || len(f.restored) != 0 || f.inst.StatusCode != api.Running {
		t.Errorf("rebuilds %v, restored %v, status %s; want the instance started again untouched", f.rebuilds, f.restored, f.inst.Status)
	}
	if !slices.Contains(f.volSnaps["default/web-data"], o.Snapshots[0][len("default/web-data/"):]) {
		t.Errorf("the first volume's snapshot %v should stay", o.Snapshots)
	}
}

func TestApplyRetriesAWriteHeldUpByAnotherUpdate(t *testing.T) {
	t.Parallel()
	for n := 1; n <= 4; n++ {
		f := newFake()
		f.lockedAt = n
		if o := apply(t, f, target()); o.Status != Updated {
			t.Errorf("write %d held up once: outcome %+v, want updated", n, o)
		}
	}
}

func TestStopForcesAStuckInstance(t *testing.T) {
	t.Parallel()
	f := newFake()
	f.stuck = true
	if err := Stop(context.Background(), f, "web", time.Second); err != nil || f.inst.StatusCode != api.Stopped {
		t.Errorf("err %v, status %s; want a forced stop", err, f.inst.Status)
	}
}

func TestPreview(t *testing.T) {
	t.Parallel()
	f := newFake()
	f.volumes["default/web-data"].UsedBy = []string{"/1.0/instances/web", "/1.0/instances/worker"}
	o := Preview(context.Background(), f, fakeRegistry{}, target(), Next{Ref: newRef}, options())
	if o.Status != Ready || !reflect.DeepEqual(o.Snapshots, []string{"default/web-data"}) ||
		!reflect.DeepEqual(o.Dropped, []string{"environment.NGINX_VERSION"}) || !reflect.DeepEqual(o.Kept, []string{"environment.FOO"}) ||
		!reflect.DeepEqual(o.Unrestored, []string{"default/web-data"}) || o.OldEnvUnread {
		t.Errorf("preview %+v", o)
	}
	moved := target()
	moved.BaseImage = "fp-older" // the old tag now names another build
	if o := Preview(context.Background(), f, fakeRegistry{}, moved, Next{Ref: newRef}, options()); !o.OldEnvUnread {
		t.Errorf("preview with a moved old tag: %+v, want the old environment marked unread", o)
	}
	if len(f.states) != 0 || f.writes != 0 {
		t.Errorf("preview changed the instance: states %v, %d writes", f.states, f.writes)
	}
	f.snapshots = []string{"snap0"}
	if o := Preview(context.Background(), f, fakeRegistry{}, target(), Next{Ref: newRef}, options()); o.Status != Refused || !strings.Contains(o.Reason, "snap0") {
		t.Errorf("preview with instance snapshots: %+v", o)
	}
}

func TestApplyBoundsAHungStepByTheLease(t *testing.T) {
	t.Parallel()
	f := newFake()
	f.hang = true
	opts := options()
	opts.Lease = 100 * time.Millisecond
	o, err := Apply(context.Background(), f, fakeRegistry{}, target(), Next{Ref: newRef}, opts)
	if err != nil || o.Status != RolledBack || !strings.Contains(o.Reason, "rebuild: context deadline exceeded") {
		t.Errorf("outcome %+v, %v; want the hung rebuild cut at the lease and rolled back", o, err)
	}
}

func TestApplyPrunesAfterTheRelease(t *testing.T) {
	t.Parallel()
	f := newFake()
	f.hangDel = true
	opts := options()
	opts.Lease, opts.pruneWait = time.Minute, 100*time.Millisecond
	o, err := Apply(context.Background(), f, fakeRegistry{}, target(), Next{Ref: newRef}, opts)
	if err != nil || o.Status != Updated {
		t.Fatalf("outcome %+v, %v; want a hung prune to leave the update standing", o, err)
	}
	if _, ok := f.inst.Config[incus.ClaimKey]; ok || f.inst.Config["user.incus-beacon.update-status"] != Updated {
		t.Errorf("config %v; want the outcome recorded and the claim gone before the prune", f.inst.Config)
	}
}

func composeFake() *fake {
	f := newFake()
	f.inst.Config["user.incus-compose.managed"] = "true"
	f.inst.Config["user.image_alias"] = oldRef
	return f
}

func composeTarget() incus.Target {
	tgt := target()
	tgt.RefSource, tgt.Asked = incus.RefFromCompose, oldRef
	return tgt
}

func TestApplySetsIncusComposesImageOverride(t *testing.T) {
	t.Parallel()
	f := composeFake()
	tgt := composeTarget()
	if o := apply(t, f, tgt); o.Status != Updated {
		t.Fatalf("outcome %+v", o)
	}
	cfg := f.inst.Config
	if cfg[incus.ComposeOverrideKey] != newRef || cfg[incus.ComposeOverrideOfKey] != oldRef {
		t.Errorf("override %q of %q; want the new ref over the compose file's", cfg[incus.ComposeOverrideKey], cfg[incus.ComposeOverrideOfKey])
	}
	if f.overrideAtRebuild != newRef {
		t.Errorf("override at rebuild %q; want it set before the rebuild, so an up meanwhile keeps the update", f.overrideAtRebuild)
	}
	if cfg["user.incus-beacon.requested"] != oldRef || cfg["user.incus-beacon.applied"] != newRef {
		t.Errorf("config %v; want the compose file's ref as requested and the new one as applied", cfg)
	}
	// A second update, while the compose file is unchanged, keeps its ref as requested.
	tgt.Ref, tgt.Applied, tgt.Update = newRef, true, incus.Update{Requested: oldRef, Applied: newRef, Status: Updated}
	f.inst.Config["volatile.base_image"], tgt.BaseImage = "fp-new", "fp-new"
	o, err := Apply(context.Background(), f, fakeRegistry{}, tgt, Next{Ref: "docker.io/library/nginx:1.29-alpine"}, options())
	if err != nil || o.Status != Updated || f.inst.Config["user.incus-beacon.requested"] != oldRef ||
		f.inst.Config[incus.ComposeOverrideOfKey] != oldRef {
		t.Errorf("second update: %+v, %v, config %v", o, err, f.inst.Config)
	}
}

func TestApplyRollbackRestoresTheImageOverride(t *testing.T) {
	t.Parallel()
	f := composeFake()
	f.crash = func(n int) bool { return n == 1 }
	if o := apply(t, f, composeTarget()); o.Status != RolledBack {
		t.Fatalf("outcome %+v", o)
	}
	if _, ok := f.inst.Config[incus.ComposeOverrideKey]; ok {
		t.Errorf("config %v; want no override after rolling back an instance that had none", f.inst.Config)
	}

	f = composeFake()
	f.crash = func(n int) bool { return n == 1 }
	f.inst.Config[incus.ComposeOverrideKey] = "docker.io/library/nginx:1.27.1-alpine"
	f.inst.Config[incus.ComposeOverrideOfKey] = oldRef
	if o := apply(t, f, composeTarget()); o.Status != RolledBack || f.inst.Config[incus.ComposeOverrideKey] != "docker.io/library/nginx:1.27.1-alpine" {
		t.Errorf("outcome %+v, override %q; want the earlier override back", o, f.inst.Config[incus.ComposeOverrideKey])
	}
}

func TestApplyResetsRequestedOnceTheAskedRefMoved(t *testing.T) {
	t.Parallel()
	f := newFake()
	tgt := target()
	tgt.RefSource = incus.RefFromBeacon
	tgt.Update = incus.Update{Requested: "docker.io/library/nginx:1.25-alpine", Applied: "docker.io/library/nginx:1.26-alpine", Status: Updated}
	if o := apply(t, f, tgt); o.Status != Updated || f.inst.Config["user.incus-beacon.requested"] != oldRef {
		t.Errorf("outcome %+v, requested %q; want the ref asked for now", o, f.inst.Config["user.incus-beacon.requested"])
	}
}

func TestApplyRecoveryRestoresTheImageOverride(t *testing.T) {
	t.Parallel()
	f := composeFake()
	cfg := f.inst.Config
	cfg["volatile.base_image"] = "fp-new"
	cfg[incus.ComposeOverrideKey], cfg[incus.ComposeOverrideOfKey] = newRef, oldRef
	cfg[incus.ClaimKey] = claimJSON(t, claim{
		Owner: "other/7", Expires: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), Step: stepIndex(t, "rebuild"), To: newRef, From: "fp-old",
		Override: map[string]string{incus.ComposeOverrideKey: "", incus.ComposeOverrideOfKey: ""},
	})
	o := apply(t, f, composeTarget())
	if _, ok := f.inst.Config[incus.ComposeOverrideKey]; o.Status != RolledBack || ok {
		t.Errorf("outcome %+v, config %v; want the override removed as it was before", o, f.inst.Config)
	}
}
