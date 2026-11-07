package serve

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/tallica/incus-beacon/internal/check"
	"github.com/tallica/incus-beacon/internal/incus"
)

func ready(t *testing.T, s *Server) (int, readiness) {
	t.Helper()
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/ready", nil))
	var r readiness
	if err := json.Unmarshal(rec.Body.Bytes(), &r); err != nil {
		t.Fatal(err)
	}
	return rec.Code, r
}

func TestReadyAfterFirstCycle(t *testing.T) {
	t.Parallel()
	s := &Server{Interval: time.Hour, Log: zerolog.Nop(), Cycle: func(context.Context) (Summary, error) {
		return Summary{Targets: 3, Updates: 1}, nil
	}}
	if code, r := ready(t, s); code != http.StatusServiceUnavailable || r.Ready {
		t.Errorf("before any cycle: %d %+v, want 503 not ready", code, r)
	}

	s.runCycle(t.Context())
	code, r := ready(t, s)
	if code != http.StatusOK || !r.Ready || r.Cycles != 1 || r.LastCycle.Targets != 3 || r.LastCycle.Updates != 1 {
		t.Errorf("after a cycle: %d %+v", code, r)
	}
	if r.LastCycle.Finished.Before(r.LastCycle.Started) {
		t.Errorf("cycle times %v .. %v", r.LastCycle.Started, r.LastCycle.Finished)
	}

	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/health", nil))
	if rec.Code != http.StatusOK {
		t.Errorf("/health %d", rec.Code)
	}
}

func TestFailedCycleNotReady(t *testing.T) {
	t.Parallel()
	fail := true
	s := &Server{Interval: time.Hour, Log: zerolog.Nop(), Cycle: func(context.Context) (Summary, error) {
		if fail {
			return Summary{}, errors.New("incus unreachable")
		}
		return Summary{}, nil
	}}
	s.runCycle(t.Context())
	if code, r := ready(t, s); code != http.StatusServiceUnavailable || r.LastError != "incus unreachable" {
		t.Errorf("failed cycle: %d %+v", code, r)
	}
	fail = false
	s.runCycle(t.Context())
	if code, r := ready(t, s); code != http.StatusOK || r.LastError != "" || r.Cycles != 2 {
		t.Errorf("recovered: %d %+v", code, r)
	}
}

func TestJitterBounds(t *testing.T) {
	t.Parallel()
	d := time.Hour
	for _, r := range []float64{0, 0.25, 0.5, 0.999} {
		got := jittered(d, r)
		if got < 54*time.Minute || got > 66*time.Minute {
			t.Errorf("jittered(1h, %v) = %s, want within ±10%%", r, got)
		}
	}
	if jittered(d, 0.5) != d {
		t.Error("r=0.5 should be the interval itself")
	}
}

func TestRunCyclesAndShutsDown(t *testing.T) {
	t.Parallel()
	var cycles atomic.Int32
	s := &Server{Interval: 20 * time.Millisecond, Log: zerolog.Nop(), Cycle: func(context.Context) (Summary, error) {
		cycles.Add(1)
		return Summary{}, nil
	}}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- s.Run(ctx, ln) }()

	deadline := time.Now().Add(5 * time.Second)
	for cycles.Load() < 3 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if n := cycles.Load(); n < 3 {
		t.Fatalf("%d cycles in 5s at a 20ms interval", n)
	}
	resp, err := http.Get("http://" + ln.Addr().String() + "/ready")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("/ready over the listener: %d", resp.StatusCode)
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Run returned %v on cancel, want nil", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after cancel")
	}
}

func TestCycleGetsADeadline(t *testing.T) {
	t.Parallel()
	s := &Server{Interval: 50 * time.Millisecond, Log: zerolog.Nop(), Cycle: func(ctx context.Context) (Summary, error) {
		<-ctx.Done()
		return Summary{}, ctx.Err()
	}}
	start := time.Now()
	s.runCycle(t.Context())
	if took := time.Since(start); took > 2*time.Second {
		t.Errorf("hung cycle ran %s, want it cut at the interval", took)
	}
	if _, r := ready(t, s); r.LastError == "" {
		t.Error("a timed-out cycle should leave its error on /ready")
	}
}

func TestCycleLogsDurationAsString(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	s := &Server{Interval: time.Hour, Log: zerolog.New(&buf), Cycle: func(context.Context) (Summary, error) {
		return Summary{}, nil
	}}
	s.runCycle(t.Context())
	var line struct{ Took any }
	if err := json.Unmarshal(buf.Bytes(), &line); err != nil {
		t.Fatal(err)
	}
	took, ok := line.Took.(string)
	if !ok {
		t.Fatalf("took = %#v, want a duration string", line.Took)
	}
	if _, err := time.ParseDuration(took); err != nil {
		t.Error(err)
	}
}

func TestBackoffCapsAtInterval(t *testing.T) {
	t.Parallel()
	s := &Server{Interval: 6 * time.Hour}
	want := []time.Duration{6 * time.Hour, time.Minute, 2 * time.Minute, 4 * time.Minute, 8 * time.Minute}
	for failures, w := range want {
		if got := s.wait(failures); got != w {
			t.Errorf("wait(%d) = %s, want %s", failures, got, w)
		}
	}
	if got := s.wait(20); got != 6*time.Hour {
		t.Errorf("wait(20) = %s, want the interval", got)
	}
	if got := (&Server{Interval: time.Minute}).wait(1); got != time.Minute {
		t.Errorf("1m interval: wait(1) = %s, want 1m", got)
	}
}

func TestRetrySoonerAfterError(t *testing.T) {
	t.Parallel()
	var cycles atomic.Int32
	s := &Server{Interval: time.Hour, Retry: 20 * time.Millisecond, Log: zerolog.Nop(), Cycle: func(context.Context) (Summary, error) {
		if cycles.Add(1) == 1 {
			return Summary{}, errors.New("incus unreachable")
		}
		return Summary{}, nil
	}}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go func() { _ = s.Run(ctx, ln) }()

	deadline := time.Now().Add(5 * time.Second)
	code, r := ready(t, s)
	for r.Cycles < 2 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
		code, r = ready(t, s)
	}
	if r.Cycles < 2 {
		t.Fatalf("%d cycles in 5s after a failure, want a retry long before the 1h interval", r.Cycles)
	}
	if code != http.StatusOK {
		t.Errorf("after the retry: %d %+v", code, r)
	}
	time.Sleep(100 * time.Millisecond)
	if n := cycles.Load(); n != 2 {
		t.Errorf("%d cycles; after a success the next should wait the full interval", n)
	}
}

func TestEventsRunOneTargetedCheck(t *testing.T) {
	t.Parallel()
	events := make(chan incus.Key, 4)
	calls := make(chan []incus.Key, 4)
	s := &Server{
		Interval: time.Hour, Log: zerolog.Nop(), Events: events, Settle: 20 * time.Millisecond,
		Cycle: func(context.Context) (Summary, error) { return Summary{}, nil },
		Targeted: func(_ context.Context, keys []incus.Key) (Summary, error) {
			calls <- keys
			return Summary{Targets: len(keys)}, nil
		},
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go func() { _ = s.Run(ctx, ln) }()

	web, api := incus.Key{Project: "apps", Instance: "web"}, incus.Key{Project: "apps", Instance: "api"}
	events <- web
	events <- api
	events <- web
	select {
	case keys := <-calls:
		slices.SortFunc(keys, func(a, b incus.Key) int { return strings.Compare(a.Instance, b.Instance) })
		if !slices.Equal(keys, []incus.Key{api, web}) {
			t.Errorf("targeted check got %v, want api and web once each", keys)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no targeted check within 5s")
	}
	select {
	case keys := <-calls:
		t.Errorf("a second targeted check for %v; one burst should give one", keys)
	case <-time.After(100 * time.Millisecond):
	}
	if _, r := ready(t, s); r.Cycles != 1 {
		t.Errorf("/ready counts %d cycles, want 1: targeted checks are not cycles", r.Cycles)
	}
}

func scrape(t *testing.T, s *Server) string {
	t.Helper()
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("/metrics %d", rec.Code)
	}
	return rec.Body.String()
}

func wantLines(t *testing.T, body string, lines ...string) {
	t.Helper()
	for _, l := range lines {
		if !strings.Contains(body, l+"\n") {
			t.Errorf("missing %q in\n%s", l, body)
		}
	}
}

func TestMetrics(t *testing.T) {
	t.Parallel()
	web, api, db := incus.Key{Project: "apps", Instance: "web"}, incus.Key{Project: "apps", Instance: "api"}, incus.Key{Project: "apps", Instance: "db"}
	fail := false
	s := &Server{
		Interval: time.Hour, Log: zerolog.Nop(),
		Cycle: func(context.Context) (Summary, error) {
			if fail {
				return Summary{}, errors.New("incus unreachable")
			}
			return Summary{Written: 2, Statuses: map[incus.Key]check.Status{
				web: check.Current, api: check.UpdateAvailable,
			}}, nil
		},
		Targeted: func(context.Context, []incus.Key) (Summary, error) {
			return Summary{Failed: 1, Statuses: map[incus.Key]check.Status{
				web: check.Error, db: check.Unknown,
			}}, nil
		},
	}
	wantLines(t, scrape(t, s),
		`incus_beacon_cycles_total{result="success"} 0`,
		`incus_beacon_instances{status="current"} 0`,
		`incus_beacon_last_success_timestamp_seconds 0`,
	)

	s.runCycle(t.Context())
	s.runTargeted(t.Context(), []incus.Key{web, db})
	wantLines(t, scrape(t, s),
		`incus_beacon_cycles_total{result="success"} 1`,
		`incus_beacon_instances{status="current"} 0`,
		`incus_beacon_instances{status="update-available"} 1`,
		`incus_beacon_instances{status="error"} 1`,
		`incus_beacon_instances{status="unknown"} 1`,
		`incus_beacon_status_writes_total{result="written"} 2`,
		`incus_beacon_status_writes_total{result="failed"} 1`,
	)

	fail = true
	s.runCycle(t.Context())
	body := scrape(t, s)
	wantLines(t, body,
		`incus_beacon_cycles_total{result="failure"} 1`,
		`incus_beacon_instances{status="update-available"} 1`,
	)
	if strings.Contains(body, "incus_beacon_last_success_timestamp_seconds 0\n") {
		t.Error("a failed cycle reset the last success time")
	}

	fail = false
	s.runCycle(t.Context())
	wantLines(t, scrape(t, s),
		`incus_beacon_instances{status="current"} 1`,
		`incus_beacon_instances{status="error"} 0`,
		`incus_beacon_instances{status="unknown"} 0`,
	)
}
