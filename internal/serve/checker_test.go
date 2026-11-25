package serve

import (
	"context"
	"maps"
	"testing"
	"time"

	incusclient "github.com/lxc/incus/v7/client"
	"github.com/lxc/incus/v7/shared/api"
	"github.com/rs/zerolog"

	"github.com/tallica/incus-beacon/internal/check"
	"github.com/tallica/incus-beacon/internal/incus"
	"github.com/tallica/incus-beacon/internal/notify"
	"github.com/tallica/incus-beacon/internal/policy"
	"github.com/tallica/incus-beacon/internal/registry"
	"github.com/tallica/incus-beacon/internal/update"
)

func TestClaimedElsewhere(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 23, 12, 0, 0, 0, time.UTC)
	for name, tc := range map[string]struct {
		claim string
		want  bool
	}{
		"no claim":   {"", false},
		"live":       {`{"owner":"other/7","expires":"2026-11-23T12:30:00Z"}`, true},
		"abandoned":  {`{"owner":"other/7","expires":"2026-11-23T11:30:00Z"}`, false},
		"unreadable": {"not json", true},
	} {
		if got := claimedElsewhere(incus.Target{Claim: tc.claim}, now); got != tc.want {
			t.Errorf("%s: claimedElsewhere %v, want %v", name, got, tc.want)
		}
	}
}

func TestSummaryCount(t *testing.T) {
	t.Parallel()
	s := Summary{Updates: 9, Statuses: map[incus.Key]check.Status{
		{Instance: "a"}: check.UpdateAvailable,
		{Instance: "b"}: check.UpdateAvailable,
		{Instance: "c"}: check.Error,
		{Instance: "d"}: check.Current,
		{Instance: "e"}: check.Unknown,
	}}
	s.count()
	if s.Updates != 2 || s.Errors != 1 {
		t.Errorf("updates %d, errors %d; want 2 and 1, recounted", s.Updates, s.Errors)
	}
}

type fakeOp struct{ incusclient.Operation }

func (fakeOp) WaitContext(context.Context) error { return nil }

// fakeIncus holds one instance, web, and counts config writes.
type fakeIncus struct {
	incusclient.InstanceServer
	config map[string]string
	writes int
}

func (f *fakeIncus) UseProject(string) incusclient.InstanceServer { return f }

func (f *fakeIncus) GetInstance(name string) (*api.Instance, string, error) {
	return &api.Instance{Name: name, InstancePut: api.InstancePut{Config: maps.Clone(f.config)}}, "etag", nil
}

func (f *fakeIncus) UpdateInstance(_ string, put api.InstancePut, _ string) (incusclient.Operation, error) {
	f.writes++
	f.config = maps.Clone(put.Config)
	return fakeOp{}, nil
}

func (f *fakeIncus) GetServer() (*api.Server, string, error) {
	return &api.Server{Environment: api.ServerEnvironment{ServerName: "node1"}}, "", nil
}

// fakeRegistry serves every ref one image, on layers other than the instance's.
type fakeRegistry struct{}

const newDigest = "sha256:" + "ab12ab12ab12ab12ab12ab12ab12ab12ab12ab12ab12ab12ab12ab12ab12ab12"

func (fakeRegistry) Head(context.Context, registry.Ref) (string, error) { return newDigest, nil }

func (fakeRegistry) Resolve(context.Context, registry.Ref, registry.Platform) (registry.Image, error) {
	return registry.Image{Digest: newDigest, ManifestDigest: newDigest, Fingerprint: "fp-new"}, nil
}

func (fakeRegistry) Tags(context.Context, registry.Ref) ([]string, error) { return nil, nil }

func (fakeRegistry) Created(context.Context, registry.Ref, registry.Image) (time.Time, error) {
	return time.Time{}, nil
}

type batches []notify.Batch

func (b *batches) Notify(_ context.Context, batch notify.Batch) { *b = append(*b, batch) }

func TestCheckerRun(t *testing.T) {
	t.Parallel()
	srv := &fakeIncus{config: map[string]string{"volatile.base_image": "fp-old"}}
	var sent batches
	c := &Checker{
		Engine: check.New(fakeRegistry{}, check.Options{Concurrency: 1}, zerolog.Nop()),
		Runner: &update.Runner{},
		Sender: &sent,
		Log:    zerolog.Nop(),
	}
	tg := incus.Target{
		Project: "default", Instance: "web", Arch: "x86_64", BaseImage: "fp-old",
		Ref: "ghcr.io/example/web:1", RefSource: incus.RefFromBeacon,
		Policy: policy.Policy{Watched: true, Track: policy.TrackDigest, Update: policy.UpdateNotify},
	}
	sum := c.Run(t.Context(), srv, []incus.Target{tg})
	if sum.Targets != 1 || sum.Updates != 1 || sum.Written != 1 || sum.Failed != 0 {
		t.Errorf("summary %+v, want one update written", sum)
	}
	if srv.config["user.incus-beacon.status"] != string(check.UpdateAvailable) {
		t.Errorf("config %v, want the update recorded", srv.config)
	}
	if len(sent) != 1 || sent[0].Server != "node1" || len(sent[0].Transitions) != 1 {
		t.Fatalf("sent %+v, want one batch from node1 with one transition", sent)
	}

	// The next pass reads back what this one wrote: nothing changed, so nothing is written or sent.
	cfg := srv.config
	tg.Beacon = incus.Beacon{
		Status: cfg["user.incus-beacon.status"], Current: cfg["user.incus-beacon.current"],
		CurrentImage: cfg["user.incus-beacon.current-image"], Latest: cfg["user.incus-beacon.latest"],
		Error: cfg["user.incus-beacon.error"], Since: cfg["user.incus-beacon.since"],
	}
	if c.Changed(tg) {
		t.Error("the target as written reads as changed")
	}
	sum = c.Run(t.Context(), srv, []incus.Target{tg})
	if sum.Written != 0 || srv.writes != 1 || len(sent) != 1 {
		t.Errorf("second pass: summary %+v, %d writes, %d batches; want nothing new", sum, srv.writes, len(sent))
	}
}
