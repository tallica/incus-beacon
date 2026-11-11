// Package serve runs check cycles on an interval and reports on them.
package serve

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"maps"
	"math/rand/v2"
	"net"
	"net/http"
	"slices"
	"sync"
	"time"

	"github.com/rs/zerolog"

	"github.com/tallica/incus-beacon/internal/check"
	"github.com/tallica/incus-beacon/internal/incus"
	"github.com/tallica/incus-beacon/internal/metrics"
)

type Summary struct {
	Started  time.Time `json:"started"`
	Finished time.Time `json:"finished"`
	Targets  int       `json:"targets"`
	Updates  int       `json:"updates"`
	Errors   int       `json:"errors"`
	Written  int       `json:"written"`
	Failed   int       `json:"failed_writes"`

	Statuses map[incus.Key]check.Status `json:"-"`
}

type Cycle func(ctx context.Context) (Summary, error)

// Targeted checks only the given instances, between cycles.
type Targeted func(ctx context.Context, keys []incus.Key) (Summary, error)

type Server struct {
	Interval time.Duration
	Retry    time.Duration // first wait after a failed cycle; defaultRetry when zero
	Cycle    Cycle
	Log      zerolog.Logger

	Events   <-chan incus.Key // nil turns targeted checks off
	Targeted Targeted
	Settle   time.Duration // quiet time before a targeted check; defaultSettle when zero

	Metrics *metrics.Set // served on /metrics; a set of its own when nil

	once     sync.Once
	m        serverMetrics
	mu       sync.Mutex
	cycles   int
	last     *Summary
	lastErr  string
	statuses map[incus.Key]check.Status
}

type serverMetrics struct {
	cycles      *metrics.Counter
	duration    *metrics.Gauge
	lastSuccess *metrics.Gauge
	instances   *metrics.Gauge
	writes      *metrics.Counter
}

var statuses = []check.Status{check.Current, check.UpdateAvailable, check.Error, check.Unknown}

func (s *Server) instruments() *serverMetrics {
	s.once.Do(func() {
		if s.Metrics == nil {
			s.Metrics = &metrics.Set{}
		}
		set := s.Metrics
		s.m = serverMetrics{
			cycles: set.Counter("incus_beacon_cycles_total",
				"Full check cycles by result: success, or failure when listing instances failed.", "result"),
			duration: set.Gauge("incus_beacon_cycle_duration_seconds",
				"How long the last full check cycle took."),
			lastSuccess: set.Gauge("incus_beacon_last_success_timestamp_seconds",
				"Unix time the last successful full check cycle finished; 0 before the first."),
			instances: set.Gauge("incus_beacon_instances",
				"Watched instances by status, as of each one's latest check.", "status"),
			writes: set.Counter("incus_beacon_status_writes_total",
				"user.incus-beacon.* writes by result: written, or failed.", "result"),
		}
		for _, r := range []string{"success", "failure"} {
			s.m.cycles.Add(0, r)
		}
		for _, r := range []string{"written", "failed"} {
			s.m.writes.Add(0, r)
		}
		for _, st := range statuses {
			s.m.instances.Set(0, string(st))
		}
	})
	return &s.m
}

const (
	shutdownTimeout = 5 * time.Second
	defaultRetry    = time.Minute
	defaultSettle   = 2 * time.Second
)

// Run follows docs/ARCHITECTURE.md#serving until ctx is done.
func (s *Server) Run(ctx context.Context, ln net.Listener) error {
	srv := &http.Server{Handler: s.Handler(), ReadHeaderTimeout: 10 * time.Second}
	served := make(chan error, 1)
	go func() { served <- srv.Serve(ln) }()

	timer := time.NewTimer(0)
	defer timer.Stop()
	failures := 0
	pending := map[incus.Key]struct{}{}
	var settled <-chan time.Time
	for {
		select {
		case <-ctx.Done():
			shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), shutdownTimeout)
			defer cancel()
			if err := srv.Shutdown(shutdownCtx); err != nil {
				return err
			}
			if err := <-served; !errors.Is(err, http.ErrServerClosed) {
				return err
			}
			return nil
		case err := <-served:
			return err
		case <-timer.C:
			if s.runCycle(ctx) {
				failures++
			} else {
				failures = 0
			}
			next := jittered(s.wait(failures), rand.Float64())
			s.Log.Debug().Stringer("next_in", next.Round(time.Second)).Msg("cycle scheduled")
			timer.Reset(next)
		case k := <-s.Events:
			if len(pending) == 0 {
				settled = time.After(cmp.Or(s.Settle, defaultSettle))
			}
			pending[k] = struct{}{}
		case <-settled:
			settled = nil
			s.runTargeted(ctx, slices.Collect(maps.Keys(pending)))
			clear(pending)
		}
	}
}

func (s *Server) wait(failures int) time.Duration {
	if failures == 0 {
		return s.Interval
	}
	d := cmp.Or(s.Retry, defaultRetry)
	for i := 1; i < failures && d < s.Interval; i++ {
		d *= 2
	}
	return min(d, s.Interval)
}

func (s *Server) runCycle(ctx context.Context) (failed bool) {
	cycleCtx, cancel := context.WithTimeout(ctx, s.Interval)
	defer cancel()
	started := time.Now()
	sum, err := s.Cycle(cycleCtx)
	if ctx.Err() != nil {
		return false
	}
	sum.Started, sum.Finished = started, time.Now()

	s.mu.Lock()
	s.cycles++
	s.last = &sum
	s.lastErr = ""
	if err != nil {
		s.lastErr = err.Error()
	}
	s.mu.Unlock()

	m := s.instruments()
	m.duration.Set(sum.Finished.Sub(sum.Started).Seconds())
	if err != nil {
		m.cycles.Add(1, "failure")
	} else {
		m.cycles.Add(1, "success")
		m.lastSuccess.Set(float64(sum.Finished.UnixMilli()) / 1000)
		s.recordStatuses(sum.Statuses, true)
	}
	s.recordWrites(sum)

	ev := s.Log.Info()
	if err != nil {
		ev = s.Log.Error().Err(err)
	}
	ev.Int("targets", sum.Targets).Int("updates", sum.Updates).Int("errors", sum.Errors).
		Int("written", sum.Written).Int("failed_writes", sum.Failed).
		Stringer("took", sum.Finished.Sub(sum.Started).Round(time.Millisecond)).Msg("cycle done")
	return err != nil
}

// runTargeted leaves /ready alone: it reflects full cycles only.
func (s *Server) runTargeted(ctx context.Context, keys []incus.Key) {
	checkCtx, cancel := context.WithTimeout(ctx, s.Interval)
	defer cancel()
	started := time.Now()
	sum, err := s.Targeted(checkCtx, keys)
	if ctx.Err() != nil {
		return
	}
	if err == nil {
		s.recordStatuses(sum.Statuses, false)
	}
	s.recordWrites(sum)
	ev := s.Log.Info()
	switch {
	case err != nil:
		ev = s.Log.Error().Err(err)
	case sum.Targets == 0:
		ev = s.Log.Debug() // beacon's own writes come back as events
	}
	ev.Int("instances", len(keys)).Int("targets", sum.Targets).Int("updates", sum.Updates).
		Int("errors", sum.Errors).Int("written", sum.Written).Int("failed_writes", sum.Failed).
		Stringer("took", time.Since(started).Round(time.Millisecond)).Msg("targeted check done")
}

// recordStatuses replaces the known statuses after a full cycle and merges a targeted check in.
func (s *Server) recordStatuses(got map[incus.Key]check.Status, full bool) {
	s.mu.Lock()
	if full || s.statuses == nil {
		s.statuses = map[incus.Key]check.Status{}
	}
	maps.Copy(s.statuses, got)
	counts := map[check.Status]int{}
	for _, st := range s.statuses {
		counts[st]++
	}
	s.mu.Unlock()
	m := s.instruments()
	for _, st := range statuses {
		m.instances.Set(float64(counts[st]), string(st))
	}
}

func (s *Server) recordWrites(sum Summary) {
	m := s.instruments()
	m.writes.Add(float64(sum.Written), "written")
	m.writes.Add(float64(sum.Failed), "failed")
}

// jittered spreads d by ±10% for r in [0, 1).
func jittered(d time.Duration, r float64) time.Duration {
	return d + time.Duration(float64(d)*0.1*(2*r-1))
}

type readiness struct {
	Ready     bool     `json:"ready"`
	Cycles    int      `json:"cycles"`
	LastCycle *Summary `json:"last_cycle,omitempty"`
	LastError string   `json:"last_error,omitempty"`
}

func (s *Server) Handler() http.Handler {
	s.instruments()
	mux := http.NewServeMux()
	mux.Handle("GET /metrics", s.Metrics.Handler())
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	mux.HandleFunc("GET /ready", func(w http.ResponseWriter, _ *http.Request) {
		s.mu.Lock()
		r := readiness{Cycles: s.cycles, LastCycle: s.last, LastError: s.lastErr}
		s.mu.Unlock()
		r.Ready = r.Cycles > 0 && r.LastError == ""
		code := http.StatusOK
		if !r.Ready {
			code = http.StatusServiceUnavailable
		}
		writeJSON(w, code, r)
	})
	return mux
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}
