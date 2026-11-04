// Command incus-beacon reports newer registry images for Incus OCI instances.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"

	"github.com/rs/zerolog"

	"github.com/tallica/incus-beacon/internal/config"
	"github.com/tallica/incus-beacon/internal/incus"
	"github.com/tallica/incus-beacon/internal/log"
	"github.com/tallica/incus-beacon/internal/report"
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
	logger, err := log.New(stderr, common.LogLevel, common.LogFormat)
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
	default:
		return cfg.Serve.Common
	}
}

func dispatch(ctx context.Context, cfg *config.Config, logger zerolog.Logger, stdout io.Writer) int {
	logger = logger.With().Str("command", cfg.Command).Logger()
	var err error
	switch cfg.Command {
	case config.CmdStatus:
		err = runStatus(ctx, cfg.Status, stdout)
	default:
		logger.Error().Msg("not implemented yet")
		return exitUsage
	}
	if err != nil {
		logger.Error().Err(err).Send()
		return exitError
	}
	return exitOK
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

func connectOptions(c config.Common) incus.ConnectOptions {
	return incus.ConnectOptions{
		Remote:            c.Remote,
		URL:               c.Incus,
		ClientCert:        c.ClientCert,
		ClientKey:         c.ClientKey,
		ServerFingerprint: c.ServerFingerprint,
		TokenFile:         c.TokenFile,
		UserAgent:         "incus-beacon/" + version.Version,
	}
}
