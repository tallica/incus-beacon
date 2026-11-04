package config

import (
	"errors"
	"io"
	"slices"
	"strings"
	"testing"
	"time"
)

func env(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func parse(t *testing.T, vars map[string]string, args ...string) (*Config, error) {
	t.Helper()
	return Parse(args, env(vars), io.Discard)
}

func TestDefaultsPerCommand(t *testing.T) {
	t.Parallel()
	cfg, err := parse(t, nil, CmdServe)
	if err != nil {
		t.Fatal(err)
	}
	s := cfg.Serve
	if s.Interval != 6*time.Hour || s.Listen != ":9153" || s.LogFormat != FormatJSON {
		t.Errorf("serve defaults: interval %s, listen %q, log format %q", s.Interval, s.Listen, s.LogFormat)
	}
	if s.Watch != WatchAll || s.Concurrency != 4 || s.Timeout != 30*time.Second || s.LogLevel != "info" {
		t.Errorf("common defaults: %+v", s.Common)
	}

	cfg, err = parse(t, nil, CmdCheck)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Check.LogFormat != FormatAuto || cfg.Check.Format != OutputTable || cfg.Check.Write {
		t.Errorf("check defaults: %+v", cfg.Check)
	}
}

func TestFlagOverridesEnv(t *testing.T) {
	t.Parallel()
	vars := map[string]string{"BEACON_LOG_LEVEL": "warn", "BEACON_INTERVAL": "2h"}
	cfg, err := parse(t, vars, CmdServe, "--log-level", "debug")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Serve.LogLevel != "debug" {
		t.Errorf("log level %q, want the flag's debug", cfg.Serve.LogLevel)
	}
	if cfg.Serve.Interval != 2*time.Hour {
		t.Errorf("interval %s, want the variable's 2h", cfg.Serve.Interval)
	}
}

func TestEnvName(t *testing.T) {
	t.Parallel()
	if got := EnvName("registry-auth-file"); got != "BEACON_REGISTRY_AUTH_FILE" {
		t.Errorf("EnvName = %q", got)
	}
}

func TestListFlags(t *testing.T) {
	t.Parallel()
	cfg, err := parse(t, nil, CmdServe,
		"--notify-url", "ntfy://ntfy.sh/a", "--notify-url", "slack://x@y",
		"--projects", "default, web,,")
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"ntfy://ntfy.sh/a", "slack://x@y"}; !slices.Equal(cfg.Serve.NotifyURLs, want) {
		t.Errorf("notify urls %q, want %q", cfg.Serve.NotifyURLs, want)
	}
	if want := []string{"default", "web"}; !slices.Equal(cfg.Serve.Projects, want) {
		t.Errorf("projects %q, want %q", cfg.Serve.Projects, want)
	}

	cfg, err = parse(t, map[string]string{"BEACON_NOTIFY_URL": "ntfy://ntfy.sh/a  slack://x@y"}, CmdServe)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Serve.NotifyURLs) != 2 {
		t.Errorf("notify urls from env: %q", cfg.Serve.NotifyURLs)
	}
}

func TestUsageErrors(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		vars map[string]string
		args []string
		want string
	}{
		{"no command", nil, nil, "no command"},
		{"unknown command", nil, []string{"frobnicate"}, "unknown command"},
		{"unknown flag", nil, []string{CmdCheck, "--nope"}, "not defined"},
		{"extra argument", nil, []string{CmdStatus, "web"}, "unexpected argument"},
		{"short interval", nil, []string{CmdServe, "--interval", "10s"}, "at least 1m"},
		{"bad level", nil, []string{CmdCheck, "--log-level", "loud"}, "log-level"},
		{"bad format", nil, []string{CmdStatus, "--format", "yaml"}, "format"},
		{"bad watch", nil, []string{CmdCheck, "--watch", "some"}, "watch"},
		{"zero concurrency", nil, []string{CmdCheck, "--concurrency", "0"}, "concurrency"},
		{"cert without key", nil, []string{CmdCheck, "--client-cert", "c.crt"}, "together"},
		{"remote and url", nil, []string{CmdCheck, "--remote", "r", "--incus", "https://example.com:8443"}, "mutually exclusive"},
		{"bad env value", map[string]string{"BEACON_CONCURRENCY": "many"}, []string{CmdCheck}, "BEACON_CONCURRENCY"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, err := parse(t, tt.vars, tt.args...)
			if !errors.Is(err, ErrUsage) {
				t.Fatalf("err = %v, want a usage error", err)
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("err = %q, want it to mention %q", err, tt.want)
			}
		})
	}
}

func TestHelp(t *testing.T) {
	t.Parallel()
	for _, args := range [][]string{{"-h"}, {"help"}, {CmdCheck, "-h"}} {
		var out strings.Builder
		_, err := Parse(args, env(nil), &out)
		if !errors.Is(err, ErrHelp) {
			t.Errorf("%q: err = %v, want ErrHelp", args, err)
		}
		if out.Len() == 0 {
			t.Errorf("%q: printed no usage", args)
		}
	}
}

func TestVersionAliases(t *testing.T) {
	t.Parallel()
	for _, arg := range []string{CmdVersion, "-version", "--version"} {
		cfg, err := parse(t, nil, arg)
		if err != nil || cfg.Command != CmdVersion {
			t.Errorf("%q: cfg %+v, err %v", arg, cfg, err)
		}
	}
}
