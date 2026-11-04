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
	"github.com/tallica/incus-beacon/internal/log"
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
	return dispatch(ctx, cfg, logger)
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

func dispatch(_ context.Context, cfg *config.Config, logger zerolog.Logger) int {
	logger.Error().Str("command", cfg.Command).Msg("not implemented yet")
	return exitUsage
}
