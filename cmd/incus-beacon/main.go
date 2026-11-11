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
	"time"

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
	code := run(ctx, os.Args[1:], os.Getenv, os.Stdout, os.Stderr)
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
	results := check.New(reg, c.Concurrency, logger).Run(ctx, targets)
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
	engine := check.New(reg, c.Concurrency, logger)
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

	checkAndWrite := func(ctx context.Context, srv incusclient.InstanceServer, targets []incus.Target) serve.Summary {
		for _, t := range targets {
			seen.Record(t)
		}
		results := engine.Run(ctx, targets)
		sum := serve.Summary{Targets: len(results), Statuses: map[incus.Key]check.Status{}}
		for i := range results {
			sum.Statuses[results[i].Target.Key()] = results[i].Status
			switch results[i].Status {
			case check.UpdateAvailable:
				sum.Updates++
			case check.Error:
				sum.Errors++
			case check.Current, check.Unknown:
			}
		}
		var changes []notify.Transition
		sum.Written, sum.Failed = writeResults(ctx, srv, results, logger, func(t incus.Target, next incus.Beacon) {
			if notify.Notable(t.Beacon, next, c.NotifyErrors) {
				changes = append(changes, notify.NewTransition(t, next))
			}
		})
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
		Name:        c.Name,
		Image:       image,
		StoragePool: c.StoragePool,
		APIURL:      c.APIURL,
		Projects:    c.Projects,
		Watch:       c.Watch,
		Events:      c.Events,
		Track:       c.Track,
		NotifyURLs:  c.NotifyURLs,
		NotifyErrs:  c.NotifyErrs,
		Wait:        c.Wait,
		Log:         logger,
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
		Name:  c.Name,
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
	return install.Uninstall(ctx, srv, c.Name, logger)
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
