package update

import (
	"context"
	"sync"
	"time"

	incusclient "github.com/lxc/incus/v7/client"
	"github.com/rs/zerolog"

	"github.com/tallica/incus-beacon/internal/check"
	"github.com/tallica/incus-beacon/internal/incus"
	"github.com/tallica/incus-beacon/internal/metrics"
	"github.com/tallica/incus-beacon/internal/policy"
)

// Runner is serve's updater: it applies the updates a check found, one at a time.
type Runner struct {
	Enabled  bool          // serve --update
	Window   policy.Window // for instances that set none; the zero Window is any time
	Options  Options
	Registry Registry
	Metrics  *metrics.Set // nil records nothing
	Log      zerolog.Logger
	Now      func() time.Time

	once    sync.Once
	updates *metrics.Counter
	// warned is unguarded: only serve's single loop goroutine calls Run, for cycles and events.
	warned map[incus.Key]bool
}

// Done is one update Run tried, and whether its outcome is news.
type Done struct {
	Target  incus.Target
	Outcome Outcome
	Notable bool
}

// Init registers the metric, so it is scraped from the start; Run calls it too.
func (r *Runner) Init() {
	r.once.Do(func() {
		r.warned = map[incus.Key]bool{}
		if r.Now == nil {
			r.Now = time.Now
		}
		if r.Metrics != nil {
			r.updates = r.Metrics.Counter("incus_beacon_updates_total",
				"Updates tried by serve, by outcome: updated, rolled-back, failed or refused, recorded on the instance or not.", "result")
			for _, s := range []string{Updated, RolledBack, Failed, Refused} {
				r.updates.Add(0, s)
			}
		}
	})
}

// Run follows docs/ARCHITECTURE.md#updating-from-serve; it starts no update once ctx is done.
func (r *Runner) Run(ctx context.Context, srv incusclient.InstanceServer, results []check.Result) []Done {
	r.Init()
	failed := map[string]bool{} // refs that failed this run, so the next replica waits
	var done []Done
	for _, res := range results {
		if ctx.Err() != nil {
			break
		}
		t := res.Target
		// Read per instance: an earlier update may have run past the window.
		now := r.Now()
		log := r.Log.With().Str("project", t.Project).Str("instance", t.Instance).Logger()
		if t.Claim != "" {
			switch {
			case !Abandoned(t.Claim, now):
				log.Debug().Msg("another process is updating it")
			case !r.Enabled:
				r.warnOnce(t, log, "an abandoned update claim; serve rolls it back only with --update")
			default:
				done = append(done, r.apply(ctx, srv, t, Next{}))
			}
			continue
		}
		if res.Status != check.UpdateAvailable || t.Policy.Update != policy.UpdateAuto {
			continue
		}
		if !r.Enabled {
			r.warnOnce(t, log, "update=auto, but serve runs without --update, so it only reports")
			continue
		}
		window := r.Window
		if t.Policy.Window != nil {
			window = *t.Policy.Window
		}
		if !window.Contains(now) {
			log.Debug().Stringer("window", window).Msg("update waits for its window")
			continue
		}
		next, err := NextOf(res)
		if err != nil {
			log.Warn().Err(err).Msg("no ref to update to")
			continue
		}
		if next.Ref == t.Update.Failed || failed[next.Ref] {
			continue
		}
		d := r.apply(ctx, srv, t, next)
		if d.Outcome.Status == RolledBack || d.Outcome.Status == Failed {
			failed[next.Ref] = true
		}
		done = append(done, d)
	}
	return done
}

func (r *Runner) apply(ctx context.Context, srv incusclient.InstanceServer, t incus.Target, next Next) Done {
	// Once begun, an update runs to its end; a deadline cut would turn it into a rollback.
	o, err := Apply(context.WithoutCancel(ctx), srv, r.Registry, t, next, r.Options)
	if err != nil {
		r.Log.Warn().Err(err).Str("project", t.Project).Str("instance", t.Instance).Msg("update outcome not recorded")
	}
	if r.updates != nil {
		r.updates.Add(1, o.Status)
	}
	// A refusal repeats every cycle; it is news only when the reason is.
	notable := o.Status != Refused || t.Update.Status != Refused || t.Update.Reason != o.Reason
	return Done{Target: t, Outcome: o, Notable: notable}
}

func (r *Runner) warnOnce(t incus.Target, log zerolog.Logger, msg string) {
	if !r.warned[t.Key()] {
		r.warned[t.Key()] = true
		log.Warn().Msg(msg)
	}
}

// Changed reports an update that touched the instance, whose status keys now describe the old image.
func (d Done) Changed() bool { return d.Outcome.Status != Refused }
