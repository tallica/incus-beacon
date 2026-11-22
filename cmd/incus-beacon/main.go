// Command incus-beacon reports newer registry images for Incus OCI instances.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"net"
	"os"
	"os/signal"
	"syscall"
	"time"
	_ "time/tzdata" // update windows read TZ; the image has no zoneinfo

	incusclient "github.com/lxc/incus/v7/client"
	"github.com/rs/zerolog"

	"github.com/tallica/incus-beacon/internal/check"
	"github.com/tallica/incus-beacon/internal/config"
	"github.com/tallica/incus-beacon/internal/incus"
	"github.com/tallica/incus-beacon/internal/install"
	"github.com/tallica/incus-beacon/internal/log"
	"github.com/tallica/incus-beacon/internal/metrics"
	"github.com/tallica/incus-beacon/internal/notify"
	"github.com/tallica/incus-beacon/internal/registry"
	"github.com/tallica/incus-beacon/internal/report"
	"github.com/tallica/incus-beacon/internal/serve"
	"github.com/tallica/incus-beacon/internal/update"
	"github.com/tallica/incus-beacon/internal/version"
)

// Exit codes: 2 is a usage error, as with the flag package.
const (
	exitOK    = 0
	exitError = 1
	exitUsage = 2
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := run(ctx, config.ForProgram(os.Args[0], os.Args[1:]), os.Getenv, os.Stdout, os.Stderr)
	stop()
	os.Exit(code)
}

func run(ctx context.Context, args []string, getenv func(string) string, stdout, stderr io.Writer) int {
	cfg, err := config.Parse(args, getenv, stderr)
	switch {
	case errors.Is(err, config.ErrHelp):
		return exitOK
	case err != nil:
		fmt.Fprintln(stderr, "incus-beacon:", err)
		return exitUsage
	}

	if cfg.Command == config.CmdVersion {
		fmt.Fprintln(stdout, version.String())
		return exitOK
	}

	common := commonOf(cfg)
	logger, err := log.New(stderr, common.LogLevel, common.LogFormat, common.LogColor)
	if err != nil {
		fmt.Fprintln(stderr, "incus-beacon:", err)
		return exitUsage
	}
	return dispatch(ctx, cfg, logger, stdout)
}

func commonOf(cfg *config.Config) config.Common {
	switch {
	case cfg.Check != nil:
		return cfg.Check.Common
	case cfg.Status != nil:
		return cfg.Status.Common
	case cfg.Install != nil:
		return cfg.Install.Common
	case cfg.Upgrade != nil:
		return cfg.Upgrade.Common
	case cfg.Uninstall != nil:
		return cfg.Uninstall.Common
	case cfg.Update != nil:
		return cfg.Update.Common
	default:
		return cfg.Serve.Common
	}
}

func dispatch(ctx context.Context, cfg *config.Config, logger zerolog.Logger, stdout io.Writer) int {
	logger = logger.With().Str("command", cfg.Command).Logger()
	var err error
	switch cfg.Command {
	case config.CmdCheck:
		err = runCheck(ctx, cfg.Check, logger, stdout)
	case config.CmdServe:
		err = runServe(ctx, cfg.Serve, logger)
	case config.CmdStatus:
		err = runStatus(ctx, cfg.Status, stdout)
	case config.CmdUpdate:
		err = runUpdate(ctx, cfg.Update, logger, stdout)
	case config.CmdInstall:
		err = runInstall(ctx, cfg.Install, logger)
	case config.CmdUpgrade:
		err = runUpgrade(ctx, cfg.Upgrade, logger)
	case config.CmdUninstall:
		err = runUninstall(ctx, cfg.Uninstall, logger)
	default:
		logger.Error().Msg("unknown command")
		return exitUsage
	}
	if err != nil {
		logger.Error().Err(err).Send()
		return exitError
	}
	return exitOK
}

func runCheck(ctx context.Context, c *config.Check, logger zerolog.Logger, stdout io.Writer) error {
	srv, err := incus.Connect(ctx, connectOptions(c.Common))
	if err != nil {
		return err
	}
	targets, err := incus.Targets(ctx, srv, incus.ListOptions{
		Projects: c.Projects,
		Instance: c.Instance,
		WatchAll: c.Watch == config.WatchAll,
	})
	if err != nil {
		return err
	}
	reg, err := registry.New(registry.Options{
		AuthFile:  c.RegistryAuthFile,
		Timeout:   c.Timeout,
		UserAgent: userAgent(),
	})
	if err != nil {
		return err
	}
	warnLegacy(logger, targets, nil)
	results := check.New(reg, check.Options{Concurrency: c.Concurrency, Cooldown: c.Cooldown}, logger).Run(ctx, targets)
	failed := 0
	if c.Write {
		var written int
		written, failed = writeResults(ctx, srv, results, logger, nil)
		logger.Info().Int("written", written).Int("unchanged", len(results)-written-failed).
			Int("failed", failed).Msg("status recorded")
	}
	if err := report.WriteCheck(stdout, c.Format, report.CheckRows(results)); err != nil {
		return err
	}
	if failed > 0 {
		return fmt.Errorf("status not recorded on %d instances", failed)
	}
	return nil
}

func runUpdate(ctx context.Context, c *config.Update, logger zerolog.Logger, stdout io.Writer) error {
	srv, err := incus.Connect(ctx, connectOptions(c.Common))
	if err != nil {
		return err
	}
	targets, err := incus.Targets(ctx, srv, incus.ListOptions{Projects: c.Projects, Instance: c.Instance, WatchAll: c.Watch == config.WatchAll})
	if err != nil {
		return err
	}
	switch len(targets) {
	case 0:
		return fmt.Errorf("%s is not a watched OCI instance in the projects in scope", c.Instance)
	case 1:
	default:
		return fmt.Errorf("%s is in several projects; give --project", c.Instance)
	}
	reg, err := registry.New(registry.Options{AuthFile: c.RegistryAuthFile, Timeout: c.Timeout, UserAgent: userAgent()})
	if err != nil {
		return err
	}
	opts := update.Options{StopTimeout: c.StopTimeout, Wait: c.Wait, Settle: c.Settle, Keep: c.Keep, Log: logger}
	// A claim first: an interrupted update can leave it current on an ungated image.
	if t := targets[0]; t.Claim != "" {
		if c.DryRun {
			o := update.Preview(ctx, srv, reg, t, update.Next{}, opts)
			if o.Status == update.Recover {
				fmt.Fprintf(stdout, "%s/%s: would roll back to %s: %s\n", t.Project, t.Instance, o.From, o.Reason)
				return fmt.Errorf("would roll back: %s", o.Reason)
			}
			fmt.Fprintf(stdout, "%s/%s: would refuse: %s\n", t.Project, t.Instance, o.Reason)
			return fmt.Errorf("%s: %s", update.Refused, o.Reason)
		}
		o, err := update.Apply(ctx, srv, reg, t, update.Next{}, opts)
		return reportUpdate(stdout, t.Project+"/"+t.Instance, o, err)
	}
	r := check.New(reg, check.Options{Concurrency: c.Concurrency, Cooldown: c.Cooldown, Pin: true}, logger).Run(ctx, targets)[0]
	name := r.Target.Project + "/" + r.Target.Instance
	if r.Status != check.UpdateAvailable {
		reason := ""
		if r.Reason != "" {
			reason = ": " + r.Reason
		}
		_, err := fmt.Fprintf(stdout, "%s: %s, nothing to update%s\n", name, r.Status, reason)
		return err
	}
	next, err := update.NextOf(r)
	if err != nil {
		return err
	}
	if c.DryRun {
		o := update.Preview(ctx, srv, reg, r.Target, next, opts)
		if o.Status != update.Ready {
			fmt.Fprintf(stdout, "%s: would refuse %s\n", name, next.Ref)
			return fmt.Errorf("%s: %s", update.Refused, o.Reason)
		}
		fmt.Fprintf(stdout, "%s: would update from %s to %s\n", name, o.From, o.To)
		printOutcome(stdout, o, "would snapshot", "would drop")
		return nil
	}
	o, err := update.Apply(ctx, srv, reg, r.Target, next, opts)
	return reportUpdate(stdout, name, o, err)
}

func reportUpdate(stdout io.Writer, name string, o update.Outcome, err error) error {
	fmt.Fprintf(stdout, "%s: %s, %s to %s\n", name, o.Status, o.From, o.To)
	printOutcome(stdout, o, "snapshot", "dropped")
	if err != nil {
		return fmt.Errorf("record the outcome: %w", err)
	}
	if o.Status != update.Updated {
		return fmt.Errorf("%s: %s", o.Status, o.Reason)
	}
	return nil
}

func printOutcome(w io.Writer, o update.Outcome, snapshot, dropped string) {
	for _, v := range o.Snapshots {
		fmt.Fprintf(w, "  %s %s\n", snapshot, v)
	}
	for _, k := range o.Dropped {
		fmt.Fprintf(w, "  %s %s, which holds an image's own value\n", dropped, k)
	}
	why := "an override"
	if o.OldEnvUnread {
		why = "an override, or a copy from an old image that can't be read now"
	}
	for _, k := range o.Kept {
		fmt.Fprintf(w, "  kept %s: %s\n", k, why)
	}
	for _, v := range o.Unrestored {
		fmt.Fprintf(w, "  not restored on a rollback: %s, which another instance also uses\n", v)
	}
}

// writeResults goes on after a failed write so the report still covers all.
func writeResults(ctx context.Context, srv incusclient.InstanceServer, results []check.Result, logger zerolog.Logger,
	onWrite func(t incus.Target, next incus.Beacon),
) (written, failed int) {
	now := time.Now()
	for i := range results {
		r := &results[i]
		next := check.Beacon(*r, now)
		changed, err := incus.WriteStatus(ctx, srv, r.Target, next)
		if err != nil {
			logger.Warn().Err(err).Str("project", r.Target.Project).Str("instance", r.Target.Instance).Msg("status not recorded")
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

func runServe(ctx context.Context, c *config.Serve, logger zerolog.Logger) error {
	logger.Info().Str("version", version.String()).Msg("incus-beacon")
	set := &metrics.Set{}
	set.Gauge("incus_beacon_build_info", "Always 1; the version label is the running build.", "version").
		Set(1, version.Version)
	reg, err := registry.New(registry.Options{
		AuthFile:  c.RegistryAuthFile,
		Timeout:   c.Timeout,
		UserAgent: userAgent(),
		Metrics:   set,
	})
	if err != nil {
		return err
	}
	engine := check.New(reg, check.Options{Concurrency: c.Concurrency, Cooldown: c.Cooldown, Pin: c.Update}, logger)
	sender, err := notify.New(notify.Options{
		URLs:    c.NotifyURLs,
		URLFile: c.NotifyURLFile,
		Command: c.NotifyCommand,
		Metrics: set,
		Log:     logger,
	})
	if err != nil {
		return err
	}
	conn := &incus.Conn{Options: connectOptions(c.Common)}
	watchAll := c.Watch == config.WatchAll
	seen := incus.Seen{}
	runner := &update.Runner{
		Enabled: c.Update, Window: c.Window, Registry: reg, Metrics: set, Log: logger,
		Options: update.Options{StopTimeout: c.StopTimeout, Wait: c.Wait, Settle: c.Settle, Keep: c.Keep, Log: logger},
	}
	runner.Init()

	checkWrite := func(ctx context.Context, srv incusclient.InstanceServer, targets []incus.Target) ([]check.Result, serve.Summary, []notify.Transition) {
		for _, t := range targets {
			seen.Record(t)
		}
		results := engine.Run(ctx, targets)
		sum := serve.Summary{Targets: len(results), Statuses: map[incus.Key]check.Status{}}
		var changes []notify.Transition
		sum.Written, sum.Failed = writeResults(ctx, srv, results, logger, func(t incus.Target, next incus.Beacon) {
			seen.Wrote(t.Key(), next)
			if notify.Notable(t.Beacon, next, c.NotifyErrors) {
				changes = append(changes, notify.NewTransition(t, next))
			}
		})
		for i := range results {
			sum.Statuses[results[i].Target.Key()] = results[i].Status
		}
		return results, sum, changes
	}
	// checkAndWrite then applies updates, re-checks what they changed and sends one message.
	checkAndWrite := func(ctx context.Context, srv incusclient.InstanceServer, targets []incus.Target) serve.Summary {
		results, sum, changes := checkWrite(ctx, srv, targets)
		var rechecks []incus.Target
		for _, d := range runner.Run(ctx, srv, results) {
			if d.Notable {
				o := d.Outcome
				changes = notify.WithApplied(changes, notify.NewApplied(d.Target, o.Status, o.From, o.To, o.Reason))
			}
			if !d.Changed() {
				continue
			}
			if t, ok, err := incus.TargetOf(ctx, srv, d.Target.Key(), watchAll); err == nil && ok {
				rechecks = append(rechecks, t)
			}
		}
		// The status keys of an updated instance still describe its old image.
		if len(rechecks) > 0 {
			_, re, more := checkWrite(ctx, srv, rechecks)
			sum.Written, sum.Failed = sum.Written+re.Written, sum.Failed+re.Failed
			maps.Copy(sum.Statuses, re.Statuses)
			changes = append(changes, more...)
		}
		for _, st := range sum.Statuses {
			switch st {
			case check.UpdateAvailable:
				sum.Updates++
			case check.Error:
				sum.Errors++
			case check.Current, check.Unknown:
			}
		}
		if len(changes) > 0 {
			sender.Notify(ctx, notify.Batch{Server: serverName(srv, logger), Transitions: changes})
		}
		return sum
	}
	warned := map[incus.Key]bool{} // only the cycle goroutine touches it
	cycle := func(ctx context.Context) (serve.Summary, error) {
		srv, err := conn.Get(ctx)
		if err != nil {
			return serve.Summary{}, err
		}
		targets, err := incus.Targets(ctx, srv, incus.ListOptions{Projects: c.Projects, WatchAll: watchAll})
		if err != nil {
			conn.Drop()
			return serve.Summary{}, err
		}
		warnLegacy(logger, targets, warned)
		return checkAndWrite(ctx, srv, targets), nil
	}
	targeted := func(ctx context.Context, keys []incus.Key) (serve.Summary, error) {
		srv, err := conn.Get(ctx)
		if err != nil {
			return serve.Summary{}, err
		}
		var targets []incus.Target
		for _, k := range keys {
			t, ok, err := incus.TargetOf(ctx, srv, k, watchAll)
			if err != nil {
				logger.Debug().Err(err).Msg("instance not read") // deleted since the event, most often
				continue
			}
			if ok && seen.Changed(t) {
				targets = append(targets, t)
			}
		}
		return checkAndWrite(ctx, srv, targets), nil
	}

	s := &serve.Server{Interval: c.Interval, Cycle: cycle, Log: logger, Metrics: set}
	if c.Events {
		events := make(chan incus.Key, 256)
		s.Events, s.Targeted = events, targeted
		go incus.Follow(ctx, conn, c.Projects, events, logger)
	}

	ln, err := net.Listen("tcp", c.Listen)
	if err != nil {
		return fmt.Errorf("listen: %w", err)
	}
	logger.Info().Str("listen", ln.Addr().String()).Stringer("interval", c.Interval).Msg("serving")
	return s.Run(ctx, ln)
}

// warnLegacy names old-style keys; with warned, once per instance.
func warnLegacy(logger zerolog.Logger, targets []incus.Target, warned map[incus.Key]bool) {
	for _, t := range targets {
		if len(t.Legacy) == 0 || warned[t.Key()] {
			continue
		}
		if warned != nil {
			warned[t.Key()] = true
		}
		logger.Warn().Str("project", t.Project).Str("instance", t.Instance).Strs("keys", t.Legacy).
			Msg("old key names, read until 0.7: rename user.beacon.* to user.incus-beacon.*")
	}
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

func runStatus(ctx context.Context, c *config.Status, stdout io.Writer) error {
	srv, err := incus.Connect(ctx, connectOptions(c.Common))
	if err != nil {
		return err
	}
	targets, err := incus.Targets(ctx, srv, incus.ListOptions{
		Projects: c.Projects,
		WatchAll: c.Watch == config.WatchAll,
	})
	if err != nil {
		return err
	}
	return report.WriteStatus(stdout, c.Format, report.StatusRows(targets, c.OnlyUpdates))
}

func runInstall(ctx context.Context, c *config.Install, logger zerolog.Logger) error {
	if _, err := notify.New(notify.Options{URLs: c.NotifyURLs}); err != nil {
		return err
	}
	image := imageFor(c.Image, logger)
	srv, err := incus.Connect(ctx, incus.ConnectOptions{Remote: c.Remote, RemoteProject: c.RemoteProject, UserAgent: userAgent()})
	if err != nil {
		return err
	}
	return install.Install(ctx, srv, install.Options{
		Name:         c.Name,
		Image:        image,
		StoragePool:  c.StoragePool,
		APIURL:       c.APIURL,
		Projects:     c.Projects,
		Watch:        c.Watch,
		Events:       c.Events,
		Track:        c.Track,
		NotifyURLs:   c.NotifyURLs,
		NotifyErrs:   c.NotifyErrs,
		Interval:     c.Interval,
		Cooldown:     c.Cooldown,
		Update:       c.Update,
		UpdateWindow: c.UpdateWindow,
		Wait:         c.Wait,
		Log:          logger,
	})
}

// imageFor defaults to this build's own release line.
func imageFor(image string, logger zerolog.Logger) string {
	if image != "" {
		return image
	}
	image, released := install.DefaultImage(version.Version)
	if !released {
		logger.Warn().Str("version", version.Version).Str("image", image).
			Msg("unreleased build: the image may be older than this installer; give --image to choose")
	}
	return image
}

func runUpgrade(ctx context.Context, c *config.Upgrade, logger zerolog.Logger) error {
	image := imageFor(c.Image, logger)
	srv, err := incus.Connect(ctx, incus.ConnectOptions{Remote: c.Remote, RemoteProject: c.RemoteProject, UserAgent: userAgent()})
	if err != nil {
		return err
	}
	return install.Upgrade(ctx, srv, install.UpgradeOptions{
		Name:  install.ResolveName(srv, c.Name, logger),
		Image: image,
		Force: c.Force,
		Wait:  c.Wait,
		Log:   logger,
	})
}

func runUninstall(ctx context.Context, c *config.Uninstall, logger zerolog.Logger) error {
	srv, err := incus.Connect(ctx, incus.ConnectOptions{Remote: c.Remote, RemoteProject: c.RemoteProject, UserAgent: userAgent()})
	if err != nil {
		return err
	}
	return install.Uninstall(ctx, srv, install.ResolveName(srv, c.Name, logger), logger)
}

func connectOptions(c config.Common) incus.ConnectOptions {
	return incus.ConnectOptions{
		Remote:            c.Remote,
		URL:               c.Incus,
		ClientCert:        c.ClientCert,
		ClientKey:         c.ClientKey,
		ServerFingerprint: c.ServerFingerprint,
		Token:             c.Token,
		TokenFile:         c.TokenFile,
		StateDir:          c.StateDir,
		UserAgent:         userAgent(),
	}
}

func userAgent() string { return "incus-beacon/" + version.Version }
