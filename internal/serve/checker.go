package serve

import (
	"context"
	"maps"
	"time"

	incusclient "github.com/lxc/incus/v7/client"
	"github.com/rs/zerolog"

	"github.com/tallica/incus-beacon/internal/check"
	"github.com/tallica/incus-beacon/internal/incus"
	"github.com/tallica/incus-beacon/internal/notify"
	"github.com/tallica/incus-beacon/internal/update"
)

// Sender is what notify.Sender does for a Checker: send one batch to every target.
type Sender interface {
	Notify(ctx context.Context, b notify.Batch)
}

// Checker is what a cycle or targeted check runs: check, write, update, re-check and notify.
type Checker struct {
	Engine       *check.Engine
	Runner       *update.Runner
	Sender       Sender
	NotifyErrors bool
	WatchAll     bool
	Log          zerolog.Logger

	seen incus.Seen // only serve's loop goroutine calls Run and Changed
}

// Changed reports a target whose inputs or status keys moved since its last check.
func (c *Checker) Changed(t incus.Target) bool { return c.seen.Changed(t) }

// Run checks targets, applies the updates found, re-checks what they changed and sends one message.
func (c *Checker) Run(ctx context.Context, srv incusclient.InstanceServer, targets []incus.Target) Summary {
	results, sum, changes := c.checkWrite(ctx, srv, targets)
	var rechecks []incus.Target
	for _, d := range c.Runner.Run(ctx, srv, results) {
		if d.Notable {
			o := d.Outcome
			changes = notify.WithApplied(changes, notify.NewApplied(d.Target, o.Status, o.From, o.To, o.Reason))
		}
		if !d.Changed() {
			continue
		}
		if t, ok, err := incus.TargetOf(ctx, srv, d.Target.Key(), c.WatchAll); err == nil && ok {
			rechecks = append(rechecks, t)
		}
	}
	// The status keys of an updated instance still describe its old image.
	if len(rechecks) > 0 {
		_, re, more := c.checkWrite(ctx, srv, rechecks)
		sum.Written, sum.Failed = sum.Written+re.Written, sum.Failed+re.Failed
		maps.Copy(sum.Statuses, re.Statuses)
		changes = append(changes, more...)
	}
	sum.count()
	if len(changes) > 0 {
		c.Sender.Notify(ctx, notify.Batch{Server: serverName(srv, c.Log), Transitions: changes})
	}
	return sum
}

func (c *Checker) checkWrite(
	ctx context.Context,
	srv incusclient.InstanceServer,
	targets []incus.Target,
) ([]check.Result, Summary, []notify.Transition) {
	if c.seen == nil {
		c.seen = incus.Seen{}
	}
	for _, t := range targets {
		c.seen.Record(t)
	}
	results := c.Engine.Run(ctx, targets)
	sum := Summary{Targets: len(results), Statuses: map[incus.Key]check.Status{}}
	var changes []notify.Transition
	sum.Written, sum.Failed = WriteResults(ctx, srv, results, c.Log, func(t incus.Target, next incus.Beacon) {
		c.seen.Wrote(t.Key(), next)
		if notify.Notable(t.Beacon, next, c.NotifyErrors) {
			changes = append(changes, notify.NewTransition(t, next))
		}
	})
	for i := range results {
		sum.Statuses[results[i].Target.Key()] = results[i].Status
	}
	return results, sum, changes
}

// count sets Updates and Errors from Statuses.
func (s *Summary) count() {
	s.Updates, s.Errors = 0, 0
	for _, st := range s.Statuses {
		switch st {
		case check.UpdateAvailable:
			s.Updates++
		case check.Error:
			s.Errors++
		case check.Current, check.Unknown:
		}
	}
}

// WriteResults goes on after a failed write so the report still covers all.
func WriteResults(ctx context.Context, srv incusclient.InstanceServer, results []check.Result, logger zerolog.Logger,
	onWrite func(t incus.Target, next incus.Beacon),
) (written, failed int) {
	now := time.Now()
	for i := range results {
		r := &results[i]
		if claimedElsewhere(r.Target, now) {
			continue
		}
		next := check.Beacon(*r, now)
		changed, err := incus.WriteStatus(ctx, srv, r.Target, next)
		if err != nil {
			logger.Warn().
				Err(err).
				Str("project", r.Target.Project).
				Str("instance", r.Target.Instance).
				Msg("status not recorded")
			failed++
			continue
		}
		if changed {
			written++
			if onWrite != nil {
				onWrite(r.Target, next)
			}
		}
	}
	return written, failed
}

// claimedElsewhere leaves an instance's status to the process updating it: docs/ARCHITECTURE.md#updating says why.
func claimedElsewhere(t incus.Target, now time.Time) bool {
	return t.Claim != "" && !update.Abandoned(t.Claim, now)
}

// serverName names the Incus server in notifications; a cluster gives the member.
func serverName(srv incusclient.InstanceServer, logger zerolog.Logger) string {
	s, _, err := srv.GetServer()
	if err != nil {
		logger.Debug().Err(err).Msg("server name not read")
		return ""
	}
	return s.Environment.ServerName
}
