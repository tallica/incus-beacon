package serve

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rs/zerolog"
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
