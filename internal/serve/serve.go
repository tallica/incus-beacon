// Package serve runs check cycles on an interval and reports on them.
package serve

import (
	"context"
	"encoding/json"
	"errors"
	"math/rand/v2"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/rs/zerolog"
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

type Server struct {
	Interval time.Duration
	Cycle    Cycle
	Log      zerolog.Logger

	mu      sync.Mutex
	cycles  int
	last    *Summary
	lastErr string
}

const shutdownTimeout = 5 * time.Second

// Run follows docs/ARCHITECTURE.md#serving until ctx is done.
func (s *Server) Run(ctx context.Context, ln net.Listener) error {
	srv := &http.Server{Handler: s.Handler(), ReadHeaderTimeout: 10 * time.Second}
	served := make(chan error, 1)
	go func() { served <- srv.Serve(ln) }()

	timer := time.NewTimer(0)
	defer timer.Stop()
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
			s.runCycle(ctx)
			next := jittered(s.Interval, rand.Float64())
			s.Log.Debug().Stringer("next_in", next.Round(time.Second)).Msg("cycle scheduled")
			timer.Reset(next)
		}
	}
}

func (s *Server) runCycle(ctx context.Context) {
	cycleCtx, cancel := context.WithTimeout(ctx, s.Interval)
	defer cancel()
	started := time.Now()
	sum, err := s.Cycle(cycleCtx)
	if ctx.Err() != nil {
		return
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
