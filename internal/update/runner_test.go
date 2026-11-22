package update

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/tallica/incus-beacon/internal/check"
	"github.com/tallica/incus-beacon/internal/incus"
	"github.com/tallica/incus-beacon/internal/metrics"
	"github.com/tallica/incus-beacon/internal/policy"
)

func autoResult(name string) check.Result {
	t := target()
	t.Instance = name
	t.Policy.Update = policy.UpdateAuto
	return check.Result{Target: t, Status: check.UpdateAvailable, Current: oldRef, Latest: "1.28-alpine"}
}

func runner(enabled bool) *Runner {
	return &Runner{
		Enabled: enabled, Registry: fakeRegistry{}, Options: options(), Log: zerolog.Nop(), Metrics: &metrics.Set{},
		Now: func() time.Time { return time.Date(2026, 11, 21, 3, 30, 0, 0, time.UTC) },
	}
}

func window(t *testing.T, s string) policy.Window {
	t.Helper()
	w, err := policy.ParseWindow(s)
	if err != nil {
		t.Fatal(err)
	}
	return w
}

func TestRunnerUpdates(t *testing.T) {
	t.Parallel()
	f := newFake()
	r := runner(true)
	done := r.Run(context.Background(), f, []check.Result{autoResult("web")})
	if len(done) != 1 || done[0].Outcome.Status != Updated || !done[0].Notable || !done[0].Changed() {
		t.Fatalf("done %+v", done)
	}
	rec := httptest.NewRecorder()
	r.Metrics.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if !strings.Contains(rec.Body.String(), `incus_beacon_updates_total{result="updated"} 1`) {
		t.Errorf("metrics:\n%s", rec.Body.String())
	}
}

func TestRunnerPinsTheCheckedDigest(t *testing.T) {
	t.Parallel()
	const pin = "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	f := newFake()
	res := autoResult("web")
	res.LatestDigest = pin
	done := runner(true).Run(context.Background(), f, []check.Result{res})
	if len(done) != 1 || done[0].Outcome.Status != Updated {
		t.Fatalf("done %+v", done)
	}
	if len(f.rebuilds) != 1 || f.rebuilds[0].Alias != "library/nginx:1.28-alpine@"+pin {
		t.Errorf("rebuilds %+v, want the alias pinned to %s", f.rebuilds, pin)
	}
}

func TestRunnerSkips(t *testing.T) {
	t.Parallel()
	notifyOnly := autoResult("web")
	notifyOnly.Target.Policy.Update = policy.UpdateNotify
	current := autoResult("web")
	current.Status = check.Current
	failedBefore := autoResult("web")
	failedBefore.Target.Update.Failed = newRef
	ownWindow := autoResult("web")
	w := window(t, "22:00-23:00")
	ownWindow.Target.Policy.Window = &w
	tests := []struct {
		name   string
		r      *Runner
		result check.Result
	}{
		{"serve without --update", runner(false), autoResult("web")},
		{"update=notify", runner(true), notifyOnly},
		{"no update found", runner(true), current},
		{"the ref failed before", runner(true), failedBefore},
		{"outside the daemon's window", func() *Runner { r := runner(true); r.Window = window(t, "04:00-05:00"); return r }(), autoResult("web")},
		{"outside the instance's window", runner(true), ownWindow},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := newFake()
			if done := tt.r.Run(context.Background(), f, []check.Result{tt.result}); len(done) != 0 || len(f.states) != 0 {
				t.Errorf("done %+v, states %v; want nothing", done, f.states)
			}
		})
	}
}

func TestRunnerInstanceWindowBeatsTheDaemons(t *testing.T) {
	t.Parallel()
	r := runner(true)
	r.Window = window(t, "04:00-05:00")
	res := autoResult("web")
	w := window(t, "03:00-04:00")
	res.Target.Policy.Window = &w
	if done := r.Run(context.Background(), newFake(), []check.Result{res}); len(done) != 1 || done[0].Outcome.Status != Updated {
		t.Errorf("done %+v", done)
	}
}

func TestRunnerStopsAReleaseAfterOneFailedReplica(t *testing.T) {
	t.Parallel()
	f := newFake()
	f.crash = func(n int) bool { return n == 1 }
	done := runner(true).Run(context.Background(), f, []check.Result{autoResult("web-1"), autoResult("web-2")})
	if len(done) != 1 || done[0].Outcome.Status != RolledBack || done[0].Target.Instance != "web-1" {
		t.Errorf("done %+v; want web-1 rolled back and web-2 left alone", done)
	}
}

func TestRunnerRecoversAbandonedClaims(t *testing.T) {
	t.Parallel()
	stale := claimJSON(t, claim{Owner: "other/7", Expires: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), Step: -1, To: newRef, From: "fp-old"})
	res := autoResult("web")
	res.Status = check.Current
	res.Target.Claim = stale

	f := newFake()
	f.inst.Config[incus.ClaimKey] = stale
	if done := runner(false).Run(context.Background(), f, []check.Result{res}); len(done) != 0 || f.inst.Config[incus.ClaimKey] == "" {
		t.Errorf("without --update: done %+v; want the claim left", done)
	}
	done := runner(true).Run(context.Background(), f, []check.Result{res})
	if len(done) != 1 || f.inst.Config[incus.ClaimKey] != "" {
		t.Errorf("with --update: done %+v, claim %q; want it cleared", done, f.inst.Config[incus.ClaimKey])
	}
}

func TestRunnerStartsNothingOnceCancelled(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	f := newFake()
	if done := runner(true).Run(ctx, f, []check.Result{autoResult("web")}); len(done) != 0 || len(f.states) != 0 {
		t.Errorf("done %+v, states %v", done, f.states)
	}
}

func TestRunnerRepeatedRefusalIsNotNews(t *testing.T) {
	t.Parallel()
	f := newFake()
	f.snapshots = []string{"snap0"}
	r := runner(true)
	first := r.Run(context.Background(), f, []check.Result{autoResult("web")})
	res := autoResult("web")
	res.Target.Update = incus.Update{Status: Refused, Reason: first[0].Outcome.Reason}
	again := r.Run(context.Background(), f, []check.Result{res})
	if !first[0].Notable || again[0].Notable || again[0].Changed() {
		t.Errorf("first %+v, again %+v; want only the first notable", first, again)
	}
}
