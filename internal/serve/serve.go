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

	"github.com/tallica/incus-beacon/internal/incus"
)

type Summary struct {
	Started  time.Time `json:"started"`
	Finished time.Time `json:"finished"`
	Targets  int       `json:"targets"`
	Updates  int       `json:"updates"`
	Errors   int       `json:"errors"`
	Written  int       `json:"written"`
	Failed   int       `json:"failed_writes"`
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

	mu      sync.Mutex
	cycles  int
	last    *Summary
	lastErr string
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
	mux := http.NewServeMux()
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
