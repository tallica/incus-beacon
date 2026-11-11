package install

import (
	"context"
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
	return op{}, nil
}

func (f *fake) ExecInstance(_ string, req api.InstanceExecPost, _ *incusclient.InstanceExecArgs) (incusclient.Operation, error) {
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
		StatusCode: api.Running,
		InstancePut: api.InstancePut{
			Config: map[string]string{
				"environment.BEACON_TOKEN_FILE": tokenFile,
				"image.id":                      oldImage,
				"volatile.base_image":           "fp-old",
			},
			Devices: map[string]map[string]string{stateDev: {"type": "disk", "source": "incus-beacon-state", "path": stateDir}},
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
	if err := Upgrade(context.Background(), f, upgradeOpts("ghcr.io/tallica/incus-beacon:0.5")); err != nil {
		t.Fatal(err)
	}
	want := []api.InstanceSource{{Type: "image", Protocol: "oci", Server: "https://ghcr.io", Alias: "tallica/incus-beacon:0.5"}}
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
	if err == nil || !strings.Contains(err.Error(), "not ready within") || !strings.Contains(err.Error(), "rolled back to "+oldImage) {
		t.Fatalf("err %v, want not ready and rolled back", err)
	}
	if len(f.rebuilds) != 2 || !reflect.DeepEqual(f.rebuilds[1], api.InstanceSource{Type: "image", Fingerprint: "fp-old"}) {
		t.Errorf("rebuilds %+v, want the new image, then the old fingerprint", f.rebuilds)
	}
	if !slices.Equal(f.states, []string{"stop", "start", "stop", "start"}) {
		t.Errorf("states %v", f.states)
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
		{"snapshots", func(f *fake) { f.snapshots = []string{"snap0"} }, "ghcr.io/tallica/incus-beacon:0.5", "has snapshots (snap0)"},
		{"not installed", func(f *fake) { delete(f.instances["incus-beacon"].Devices, stateDev) }, "ghcr.io/tallica/incus-beacon:0.5", "not created by install"},
		{"missing", func(f *fake) { delete(f.instances, "incus-beacon") }, "ghcr.io/tallica/incus-beacon:0.5", "install it first"},
		{"no base image", func(f *fake) { delete(f.instances["incus-beacon"].Config, "volatile.base_image") }, "ghcr.io/tallica/incus-beacon:0.5", "no base image"},
		{"bad image", func(*fake) {}, "incus-beacon:0.5", "want registry/repository"},
	}
	for _, tt := range tests {
		f := installedFake()
		tt.setup(f)
		err := Upgrade(context.Background(), f, upgradeOpts(tt.image))
		if err == nil || !strings.Contains(err.Error(), tt.want) {
			t.Errorf("%s: err %v, want %q", tt.name, err, tt.want)
		}
		if len(f.states) != 0 || len(f.rebuilds) != 0 {
			t.Errorf("%s: states %v, rebuilds %v, want the instance untouched", tt.name, f.states, f.rebuilds)
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
