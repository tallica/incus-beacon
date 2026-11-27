// Command incus-beacon reports newer registry images for Incus OCI instances.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/signal"
	"syscall"
	_ "time/tzdata" // update windows read TZ; the image has no zoneinfo

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

	common := cfg.CommonOf()
	logger, err := log.New(stderr, common.LogLevel, common.LogFormat, common.LogColor)
	if err != nil {
		fmt.Fprintln(stderr, "incus-beacon:", err)
		return exitUsage
	}
	a := cfg.AuditOf()
	audit, closer := log.NewAudit(a.AuditLog, a.AuditLogMaxSize, a.AuditLogKeep, logger)
	defer closer.Close()
	audit = audit.With().Str("version", version.Version).Logger()
	return dispatch(ctx, cfg, logger, audit, stdout)
}

func dispatch(ctx context.Context, cfg *config.Config, logger, audit zerolog.Logger, stdout io.Writer) int {
	logger = logger.With().Str("command", cfg.Command).Logger()
	var err error
	switch cfg.Command {
	case config.CmdCheck:
		err = runCheck(ctx, cfg.Check, logger, stdout)
	case config.CmdServe:
		err = runServe(ctx, cfg.Serve, logger, audit)
	case config.CmdStatus:
		err = runStatus(ctx, cfg.Status, stdout)
	case config.CmdUpdate:
		err = runUpdate(ctx, cfg.Update, logger, audit, stdout)
	case config.CmdInstall:
		err = runInstall(ctx, cfg.Install, logger, audit)
	case config.CmdConfigure:
		err = runConfigure(ctx, cfg.Configure, logger, audit)
	case config.CmdUpgrade:
		err = runUpgrade(ctx, cfg.Upgrade, logger, audit)
	case config.CmdUninstall:
		err = runUninstall(ctx, cfg.Uninstall, logger, audit)
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
	targets, err := incus.Targets(ctx, srv, listOptions(c.Common, c.Instance))
	if err != nil {
		return err
	}
	reg, err := newRegistry(c.Registry, nil)
	if err != nil {
		return err
	}
	warnLegacy(logger, targets, nil)
	results := check.New(reg, check.Options{Concurrency: c.Concurrency, Cooldown: c.Cooldown}, logger).Run(ctx, targets)
	failed := 0
	if c.Write {
		var written int
		written, failed = serve.WriteResults(ctx, srv, results, logger, nil)
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

func runUpdate(ctx context.Context, c *config.Update, logger, audit zerolog.Logger, stdout io.Writer) error {
	srv, err := incus.Connect(ctx, connectOptions(c.Common))
	if err != nil {
		return err
	}
	targets, err := incus.Targets(ctx, srv, listOptions(c.Common, c.Instance))
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
	reg, err := newRegistry(c.Registry, nil)
	if err != nil {
		return err
	}
	opts := updateOptions(c.UpdateFlags, logger, audit)
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

func runServe(ctx context.Context, c *config.Serve, logger, audit zerolog.Logger) error {
	logger.Info().Str("version", version.String()).Msg("incus-beacon")
	set := &metrics.Set{}
	set.Gauge("incus_beacon_build_info", "Always 1; the version label is the running build.", "version").
		Set(1, version.Version)
	reg, err := newRegistry(c.Registry, set)
	if err != nil {
		return err
	}
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
	runner := &update.Runner{
		Enabled: c.Update, Window: c.Window, Registry: reg, Metrics: set, Log: logger,
		Options: updateOptions(c.UpdateFlags, logger, audit),
	}
	runner.Init()
	checker := &serve.Checker{
		Engine: check.New(
			reg,
			check.Options{Concurrency: c.Concurrency, Cooldown: c.Cooldown, Pin: c.Update},
			logger,
		),
		Runner:       runner,
		Sender:       sender,
		NotifyErrors: c.NotifyErrors,
		WatchAll:     watchAll,
		Log:          logger,
	}
	warned := map[incus.Key]bool{} // only the cycle goroutine touches it
	cycle := func(ctx context.Context) (serve.Summary, error) {
		srv, err := conn.Get(ctx)
		if err != nil {
			return serve.Summary{}, err
		}
		targets, err := incus.Targets(ctx, srv, listOptions(c.Common, ""))
		if err != nil {
			conn.Drop()
			return serve.Summary{}, err
		}
		warnLegacy(logger, targets, warned)
		return checker.Run(ctx, srv, targets), nil
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
			if ok && checker.Changed(t) {
				targets = append(targets, t)
			}
		}
		return checker.Run(ctx, srv, targets), nil
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

func runStatus(ctx context.Context, c *config.Status, stdout io.Writer) error {
	srv, err := incus.Connect(ctx, connectOptions(c.Common))
	if err != nil {
		return err
	}
	targets, err := incus.Targets(ctx, srv, listOptions(c.Common, ""))
	if err != nil {
		return err
	}
	return report.WriteStatus(stdout, c.Format, report.StatusRows(targets, c.OnlyUpdates))
}

func runInstall(ctx context.Context, c *config.Install, logger, audit zerolog.Logger) error {
	if _, err := notify.New(notify.Options{URLs: c.NotifyURLs}); err != nil {
		return err
	}
	image := imageFor(c.Image, logger)
	srv, err := incus.Connect(ctx, remoteOptions(c.Common))
	if err != nil {
		return err
	}
	return install.Install(ctx, srv, install.Options{
		Name:        c.Name,
		Image:       image,
		StoragePool: c.StoragePool,
		Projects:    c.Projects,
		Watch:       c.Watch,
		Settings:    c.Settings,
		Wait:        c.Wait,
		Log:         logger,
		Audit:       audit,
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

func runConfigure(ctx context.Context, c *config.Configure, logger, audit zerolog.Logger) error {
	if _, err := notify.New(notify.Options{URLs: c.NotifyURLs}); err != nil {
		return err
	}
	// Built before connecting, so a misspelt name panics in any test that runs configure.
	opts := install.ConfigureOptions{
		APIURL:       given(c, "api-url", &c.APIURL),
		Projects:     given(c, "project", &c.Projects),
		Watch:        given(c, "watch", &c.Watch),
		Events:       given(c, "events", &c.Events),
		Track:        given(c, "track", &c.Track),
		NotifyURLs:   given(c, "notify-url", &c.NotifyURLs),
		NotifyErrs:   given(c, "notify-errors", &c.NotifyErrs),
		Interval:     given(c, "interval", &c.Interval),
		Cooldown:     given(c, "cooldown", &c.Cooldown),
		Update:       given(c, "update", &c.Update),
		UpdateWindow: given(c, "update-window", &c.UpdateWindow),
		Wait:         c.Wait,
		Log:          logger,
		Audit:        audit,
	}
	srv, err := incus.Connect(ctx, remoteOptions(c.Common))
	if err != nil {
		return err
	}
	opts.Name = install.ResolveName(srv, c.Name, logger)
	return install.Configure(ctx, srv, opts)
}

func given[T any](c *config.Configure, flagName string, v *T) *T {
	if c.Given(flagName) {
		return v
	}
	return nil
}

func runUpgrade(ctx context.Context, c *config.Upgrade, logger, audit zerolog.Logger) error {
	image := imageFor(c.Image, logger)
	srv, err := incus.Connect(ctx, remoteOptions(c.Common))
	if err != nil {
		return err
	}
	return install.Upgrade(ctx, srv, install.UpgradeOptions{
		Name:  install.ResolveName(srv, c.Name, logger),
		Image: image,
		Force: c.Force,
		Wait:  c.Wait,
		Log:   logger,
		Audit: audit,
	})
}

func runUninstall(ctx context.Context, c *config.Uninstall, logger, audit zerolog.Logger) error {
	srv, err := incus.Connect(ctx, remoteOptions(c.Common))
	if err != nil {
		return err
	}
	return install.Uninstall(ctx, srv, install.UninstallOptions{
		Name:      install.ResolveName(srv, c.Name, logger),
		DeleteLog: c.DeleteLog,
		Log:       logger,
		Audit:     audit,
	})
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

// remoteOptions connects install, configure, upgrade and uninstall, which take only a CLI remote.
func remoteOptions(c config.Common) incus.ConnectOptions {
	return incus.ConnectOptions{Remote: c.Remote, RemoteProject: c.RemoteProject, UserAgent: userAgent()}
}

func listOptions(c config.Common, instance string) incus.ListOptions {
	return incus.ListOptions{Projects: c.Projects, Instance: instance, WatchAll: c.Watch == config.WatchAll}
}

func newRegistry(r config.Registry, m *metrics.Set) (*registry.Client, error) {
	return registry.New(
		registry.Options{AuthFile: r.RegistryAuthFile, Timeout: r.Timeout, UserAgent: userAgent(), Metrics: m},
	)
}

func updateOptions(u config.UpdateFlags, logger, audit zerolog.Logger) update.Options {
	return update.Options{
		StopTimeout: u.StopTimeout,
		Wait:        u.Wait,
		Settle:      u.Settle,
		Keep:        u.Keep,
		Log:         logger,
		Audit:       audit,
	}
}

func userAgent() string { return "incus-beacon/" + version.Version }
