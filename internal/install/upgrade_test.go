package install

import (
	"bytes"
	"context"
	"errors"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	incusclient "github.com/lxc/incus/v7/client"
	"github.com/lxc/incus/v7/shared/api"
	"github.com/rs/zerolog"
)

func (f *fake) GetInstanceSnapshotNames(string) ([]string, error) { return f.snapshots, nil }

func (f *fake) RebuildInstance(name string, req api.InstanceRebuildPost) (incusclient.Operation, error) {
	f.rebuilds = append(f.rebuilds, req.Source)
	if err := f.failRebuild; err != nil && len(f.rebuilds) == 1 {
		return nil, err
	}
	return op{}, nil
}

func (f *fake) ExecInstance(
	_ string,
	req api.InstanceExecPost,
	_ *incusclient.InstanceExecArgs,
) (incusclient.Operation, error) {
	f.calls = append(f.calls, "exec "+strings.Join(req.Command, " "))
	code := 1.0
	if f.ready != nil && f.ready(len(f.rebuilds)) {
		code = 0
	}
	return op{meta: api.Operation{Metadata: map[string]any{"return": code}}}, nil
}

const oldImage = "tallica/incus-beacon:0.4"

func installedFake() *fake {
	f := newFake()
	f.instances["incus-beacon"] = &api.Instance{
		Name:       "incus-beacon",
		Project:    "default",
		StatusCode: api.Running,
		InstancePut: api.InstancePut{
			Config: map[string]string{
				"environment.BEACON_TOKEN_FILE": tokenFile,
				"image.id":                      oldImage,
				"volatile.base_image":           "fp-old",
			},
			Devices: map[string]map[string]string{
				stateDev: {"type": "disk", "pool": "default", "source": "incus-beacon-state", "path": stateDir},
			},
		},
	}
	f.ready = func(int) bool { return true }
	return f
}

func upgradeOpts(image string) UpgradeOptions {
	return UpgradeOptions{Name: "incus-beacon", Image: image, Wait: 50 * time.Millisecond, Log: zerolog.Nop()}
}

func TestUpgrade(t *testing.T) {
	t.Parallel()
	f := installedFake()
	o := upgradeOpts("ghcr.io/tallica/incus-beacon:0.5")
	var buf bytes.Buffer
	o.Audit = zerolog.New(&buf)
	if err := Upgrade(context.Background(), f, o); err != nil {
		t.Fatal(err)
	}
	for _, r := range checkAudit(t, &buf, "upgrade begin", "upgrade done") {
		if r["project"] != "default" || r["from"] != oldImage || r["from_image"] != "fp-old" || r["image"] != o.Image ||
			r["log_volume"] != "incus-beacon-log" {
			t.Errorf("record %v: want project, from, from_image, image and log_volume", r)
		}
	}
	checkLogVolume(t, f)
	if i, j := slices.Index(
		f.calls,
		"update instance incus-beacon",
	), slices.Index(
		f.calls,
		"create volume incus-beacon-log",
	); j < 0 ||
		i < j {
		t.Errorf("calls %v, want the volume created, then the instance updated", f.calls)
	}
	want := []api.InstanceSource{
		{Type: "image", Protocol: "oci", Server: "https://ghcr.io", Alias: "tallica/incus-beacon:0.5"},
	}
	if !reflect.DeepEqual(f.rebuilds, want) {
		t.Errorf("rebuilds %+v, want %+v", f.rebuilds, want)
	}
	if !slices.Equal(f.states, []string{"stop", "start"}) {
		t.Errorf("states %v, want stop, start", f.states)
	}
	if !slices.Contains(f.calls, "exec "+strings.Join(readyProbe, " ")) {
		t.Errorf("calls %v, want the /ready probe", f.calls)
	}
}

func TestUpgradeRollsBack(t *testing.T) {
	t.Parallel()
	f := installedFake()
	f.ready = func(rebuilds int) bool { return rebuilds != 1 }
	err := Upgrade(context.Background(), f, upgradeOpts("ghcr.io/tallica/incus-beacon:0.5"))
	if err == nil || !strings.Contains(err.Error(), "not ready within") ||
		!strings.Contains(err.Error(), "rolled back to "+oldImage) {
		t.Fatalf("err %v, want not ready and rolled back", err)
	}
	if len(f.rebuilds) != 2 ||
		!reflect.DeepEqual(f.rebuilds[1], api.InstanceSource{Type: "image", Fingerprint: "fp-old"}) {
		t.Errorf("rebuilds %+v, want the new image, then the old fingerprint", f.rebuilds)
	}
	if !slices.Equal(f.states, []string{"stop", "start", "stop", "start"}) {
		t.Errorf("states %v", f.states)
	}
	checkLogVolume(t, f)
}

func checkLogVolume(t *testing.T, f *fake) {
	t.Helper()
	inst := f.instances["incus-beacon"]
	if _, ok := f.volumes["default/incus-beacon-log"]; !ok {
		t.Error("no log volume")
	}
	if d := inst.Devices[logDev]; d["pool"] != "default" || d["source"] != "incus-beacon-log" || d["path"] != logDir {
		t.Errorf("log device %v", d)
	}
	if inst.Devices[stateDev]["source"] != "incus-beacon-state" ||
		inst.Config["environment.BEACON_AUDIT_LOG"] != auditFile ||
		inst.Config["volatile.base_image"] != "fp-old" {
		t.Errorf("instance %+v, want the state device and config kept and the audit path added", inst.InstancePut)
	}
}

func TestUpgradeLeavesTheKeyOfAnInstanceWithALogDevice(t *testing.T) {
	t.Parallel()
	f := installedFake()
	inst := f.instances["incus-beacon"]
	inst.Devices[logDev] = disk("default", "incus-beacon-log", logDir)
	var buf bytes.Buffer
	o := upgradeOpts("ghcr.io/tallica/incus-beacon:0.5")
	o.Audit = zerolog.New(&buf)
	if err := Upgrade(context.Background(), f, o); err != nil {
		t.Fatal(err)
	}
	if slices.ContainsFunc(f.calls, func(c string) bool {
		return strings.HasPrefix(c, "create volume") || strings.HasPrefix(c, "update instance")
	}) {
		t.Errorf("calls %v, want the log volume added only once", f.calls)
	}
	if strings.Contains(buf.String(), "log_volume") {
		t.Errorf("audited %s, want no log_volume when nothing was added", buf.String())
	}
	if v, ok := inst.Config["environment.BEACON_AUDIT_LOG"]; ok {
		t.Errorf("BEACON_AUDIT_LOG = %q, want an unset key left unset", v)
	}
}

func TestUpgradeMovesAHandSetAuditLogOntoTheVolume(t *testing.T) {
	t.Parallel()
	f := installedFake()
	f.instances["incus-beacon"].Config["environment.BEACON_AUDIT_LOG"] = "/tmp/audit.log"
	if err := Upgrade(context.Background(), f, upgradeOpts("ghcr.io/tallica/incus-beacon:0.5")); err != nil {
		t.Fatal(err)
	}
	checkLogVolume(t, f)
}

func TestUpgradeRemovesALogVolumeItCouldNotAttach(t *testing.T) {
	t.Parallel()
	f := installedFake()
	f.failUpdate = errors.New("etag mismatch")
	err := Upgrade(context.Background(), f, upgradeOpts("ghcr.io/tallica/incus-beacon:0.5"))
	if err == nil || !strings.Contains(err.Error(), "add the log device") {
		t.Fatalf("err %v, want the failed attach", err)
	}
	if _, ok := f.volumes["default/incus-beacon-log"]; ok {
		t.Error("log volume left behind with nothing attached to it")
	}
	if len(f.states) != 0 || len(f.rebuilds) != 0 {
		t.Errorf("states %v, rebuilds %v, want the instance untouched", f.states, f.rebuilds)
	}

	f = installedFake()
	f.volumes["default/incus-beacon-log"] = api.StorageVolumesPost{Name: "incus-beacon-log"}
	f.failUpdate = errors.New("etag mismatch")
	_ = Upgrade(context.Background(), f, upgradeOpts("ghcr.io/tallica/incus-beacon:0.5"))
	if _, ok := f.volumes["default/incus-beacon-log"]; !ok {
		t.Error("a log volume the upgrade didn't create was deleted")
	}
}

func TestUpgradeRollsBackAFailedRebuild(t *testing.T) {
	t.Parallel()
	f := installedFake()
	f.failRebuild = errors.New("Failed getting remote image info")
	o := upgradeOpts("ghcr.io/tallica/incus-beacon:0.5")
	var buf bytes.Buffer
	o.Audit = zerolog.New(&buf)
	err := Upgrade(context.Background(), f, o)
	if err == nil || !strings.Contains(err.Error(), "rolled back to "+oldImage) {
		t.Fatalf("err %v, want rolled back", err)
	}
	checkAudit(t, &buf, "upgrade begin", "upgrade.rollback begin", "upgrade.rollback done", "upgrade failed")
	if !slices.Equal(f.states, []string{"stop", "start"}) || f.instances["incus-beacon"].StatusCode != api.Running {
		t.Errorf("states %v, want the instance started again without a second stop", f.states)
	}
	if len(f.rebuilds) != 2 ||
		!reflect.DeepEqual(f.rebuilds[1], api.InstanceSource{Type: "image", Fingerprint: "fp-old"}) {
		t.Errorf("rebuilds %+v, want the failed one, then the old fingerprint", f.rebuilds)
	}
}

func TestStopIfRunningStopsAnUnreadableInstance(t *testing.T) {
	t.Parallel()
	f := newFake()
	if err := stopIfRunning(
		context.Background(),
		f,
		"unreadable",
	); err != nil ||
		!slices.Equal(f.states, []string{"stop"}) {
		t.Errorf("err %v, states %v, want the stop tried", err, f.states)
	}
}

func TestUpgradeRefuses(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		setup func(*fake)
		image string
		want  string
	}{
		{"downgrade", func(*fake) {}, "ghcr.io/tallica/incus-beacon:0.3", "give --force to downgrade"},
		{
			"downgrade by digest",
			func(*fake) {},
			"ghcr.io/tallica/incus-beacon:0.3@sha256:" + strings.Repeat("a", 64),
			"give --force to downgrade",
		},
		{
			"snapshots",
			func(f *fake) { f.snapshots = []string{"snap0"} },
			"ghcr.io/tallica/incus-beacon:0.5",
			"has snapshots (snap0)",
		},
		{
			"not installed",
			func(f *fake) { delete(f.instances["incus-beacon"].Devices, stateDev) },
			"ghcr.io/tallica/incus-beacon:0.5",
			"not created by install",
		},
		{
			"missing",
			func(f *fake) { delete(f.instances, "incus-beacon") },
			"ghcr.io/tallica/incus-beacon:0.5",
			"install it first",
		},
		{
			"no base image",
			func(f *fake) { delete(f.instances["incus-beacon"].Config, "volatile.base_image") },
			"ghcr.io/tallica/incus-beacon:0.5",
			"no base image",
		},
		{"bad image", func(*fake) {}, "incus-beacon:0.5", "want registry/repository"},
		{"mistyped image", func(*fake) {}, "image=ghcr.io/tallica/incus-beacon:0.5", "not a valid reference"},
	}
	for _, tt := range tests {
		f := installedFake()
		tt.setup(f)
		o := upgradeOpts(tt.image)
		var buf bytes.Buffer
		o.Audit = zerolog.New(&buf)
		err := Upgrade(context.Background(), f, o)
		if err == nil || !strings.Contains(err.Error(), tt.want) {
			t.Errorf("%s: err %v, want %q", tt.name, err, tt.want)
		}
		if len(f.states) != 0 || len(f.rebuilds) != 0 {
			t.Errorf("%s: states %v, rebuilds %v, want the instance untouched", tt.name, f.states, f.rebuilds)
		}
		if buf.Len() > 0 {
			t.Errorf("%s: audited %s, want a refusal unrecorded", tt.name, buf.String())
		}
	}
}

func TestUpgradeForceAndOtherTags(t *testing.T) {
	t.Parallel()
	for _, image := range []string{"ghcr.io/tallica/incus-beacon:latest", "ghcr.io/tallica/incus-beacon:0.4.1", "ghcr.io/tallica/incus-beacon:0.4"} {
		if err := Upgrade(context.Background(), installedFake(), upgradeOpts(image)); err != nil {
			t.Errorf("%s: %v, want tags of another shape, or the same tag, allowed", image, err)
		}
	}
	o := upgradeOpts("ghcr.io/tallica/incus-beacon:0.3")
	o.Force = true
	if err := Upgrade(context.Background(), installedFake(), o); err != nil {
		t.Errorf("--force downgrade: %v", err)
	}
}
