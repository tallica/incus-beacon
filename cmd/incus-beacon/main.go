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
	results := check.New(reg, c.Concurrency, logger).Run(ctx, targets)
	failed := 0
	if c.Write {
		var written int
		written, failed = writeResults(ctx, srv, results, logger)
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
func writeResults(ctx context.Context, srv incusclient.InstanceServer, results []check.Result, logger zerolog.Logger) (written, failed int) {
	now := time.Now()
	for i := range results {
		r := &results[i]
		changed, err := incus.WriteStatus(ctx, srv, r.Target, check.Beacon(*r, now))
		if err != nil {
			logger.Warn().Err(err).Str("project", r.Target.Project).Str("instance", r.Target.Instance).Msg("status not recorded")
			failed++
			continue
		}
		if changed {
			written++
		}
	}
	return written, failed
}

func runServe(ctx context.Context, c *config.Serve, logger zerolog.Logger) error {
	logger.Info().Str("version", version.String()).Msg("incus-beacon")
	reg, err := registry.New(registry.Options{
		AuthFile:  c.RegistryAuthFile,
		Timeout:   c.Timeout,
		UserAgent: userAgent(),
	})
	if err != nil {
		return err
	}
	engine := check.New(reg, c.Concurrency, logger)
	var srv incusclient.InstanceServer

	cycle := func(ctx context.Context) (serve.Summary, error) {
		if srv == nil {
			s, err := incus.Connect(ctx, connectOptions(c.Common))
			if err != nil {
				return serve.Summary{}, err
			}
			srv = s
		}
		targets, err := incus.Targets(ctx, srv, incus.ListOptions{
			Projects: c.Projects,
			WatchAll: c.Watch == config.WatchAll,
		})
		if err != nil {
			srv = nil
			return serve.Summary{}, err
		}
		results := engine.Run(ctx, targets)
		sum := serve.Summary{Targets: len(results)}
		for i := range results {
			switch results[i].Status {
			case check.UpdateAvailable:
				sum.Updates++
			case check.Error:
				sum.Errors++
			case check.Current, check.Unknown:
			}
		}
		sum.Written, sum.Failed = writeResults(ctx, srv, results, logger)
		return sum, nil
	}

	ln, err := net.Listen("tcp", c.Listen)
	if err != nil {
		return fmt.Errorf("listen: %w", err)
	}
	logger.Info().Str("listen", ln.Addr().String()).Stringer("interval", c.Interval).Msg("serving")
	return (&serve.Server{Interval: c.Interval, Cycle: cycle, Log: logger}).Run(ctx, ln)
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
	image := c.Image
	if image == "" {
		var released bool
		image, released = install.DefaultImage(version.Version)
		if !released {
			logger.Warn().Str("version", version.Version).Str("image", image).
				Msg("unreleased build: the image may be older than this installer; give --image to choose")
		}
	}
	srv, err := incus.Connect(ctx, incus.ConnectOptions{Remote: c.Remote, UserAgent: userAgent()})
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
		Wait:        c.Wait,
		Log:         logger,
	})
}

func runUninstall(ctx context.Context, c *config.Uninstall, logger zerolog.Logger) error {
	srv, err := incus.Connect(ctx, incus.ConnectOptions{Remote: c.Remote, UserAgent: userAgent()})
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
