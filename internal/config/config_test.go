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
	if s.Interval != 6*time.Hour || s.Listen != ":9153" || s.LogFormat != FormatConsole || s.Events {
		t.Errorf("serve defaults: interval %s, listen %q, log format %q, events %t", s.Interval, s.Listen, s.LogFormat, s.Events)
	}
	if s.LogColor != ColorAuto {
		t.Errorf("log color %q, want auto", s.LogColor)
	}
	if s.Watch != WatchAll || s.Concurrency != 4 || s.Timeout != 30*time.Second || s.LogLevel != "info" {
		t.Errorf("common defaults: %+v", s.Common)
	}

	cfg, err = parse(t, nil, CmdCheck)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Check.LogFormat != FormatConsole || cfg.Check.Format != OutputTable || cfg.Check.Write {
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

func TestEventsOn(t *testing.T) {
	t.Parallel()
	cfg, err := parse(t, map[string]string{"BEACON_EVENTS": "true"}, CmdServe)
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.Serve.Events {
		t.Error("BEACON_EVENTS=true left events off")
	}
	cfg, err = parse(t, nil, CmdInstall, "--events")
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.Install.Events {
		t.Error("install --events not set")
	}
}

func TestLogColorEnv(t *testing.T) {
	t.Parallel()
	cfg, err := parse(t, map[string]string{"BEACON_LOG_COLOR": "never"}, CmdServe)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Serve.LogColor != ColorNever {
		t.Errorf("log color %q, want the variable's never", cfg.Serve.LogColor)
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
	cfg, err := parse(t, nil, CmdServe, "--project", "default, web,,")
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"default", "web"}; !slices.Equal(cfg.Serve.Projects, want) {
		t.Errorf("projects %q, want %q", cfg.Serve.Projects, want)
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
		{"bad colour", nil, []string{CmdCheck, "--log-color", "loud"}, "log-color"},
		{"bad format", nil, []string{CmdStatus, "--format", "yaml"}, "format"},
		{"bad watch", nil, []string{CmdCheck, "--watch", "some"}, "watch"},
		{"zero concurrency", nil, []string{CmdCheck, "--concurrency", "0"}, "concurrency"},
		{"cert without key", nil, []string{CmdCheck, "--client-cert", "c.crt"}, "together"},
		{"remote and url", nil, []string{CmdCheck, "--remote", "r", "--incus", "https://example.com:8443"}, "mutually exclusive"},
		{"token without incus", nil, []string{CmdServe, "--token", "t", "--state-dir", "/path/to/state"}, "token needs incus"},
		{"token without state dir", map[string]string{"BEACON_TOKEN": "t"}, []string{CmdServe, "--incus", "https://example.com:8443"}, "token needs state-dir"},
		{"token and cert", nil, []string{CmdServe, "--incus", "https://example.com:8443", "--token", "t", "--state-dir", "/s", "--client-cert", "c", "--client-key", "k"}, "token and client-cert"},
		{"token and token file", nil, []string{CmdServe, "--incus", "https://example.com:8443", "--state-dir", "/s", "--token", "t", "--token-file", "/f"}, "token and token-file"},
		{"token file without state dir", nil, []string{CmdServe, "--incus", "https://example.com:8443", "--token-file", "/f"}, "token needs state-dir"},
		{"bad env value", map[string]string{"BEACON_CONCURRENCY": "many"}, []string{CmdCheck}, "BEACON_CONCURRENCY"},
		{"notify errors alone", nil, []string{CmdServe, "--notify-errors"}, "notify-errors needs"},
		{"install notify errors alone", nil, []string{CmdInstall, "--notify-errors"}, "notify-errors needs notify-url"},
		{"install notify command", nil, []string{CmdInstall, "--notify-command", "x"}, "not defined"},
		{"notify on check", nil, []string{CmdCheck, "--notify-url", "https://example.com"}, "not defined"},
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

func TestProjectFlag(t *testing.T) {
	t.Parallel()
	for _, cmd := range []string{CmdCheck, CmdStatus, CmdServe} {
		cfg, err := parse(t, map[string]string{"BEACON_PROJECT": "default,web"}, cmd)
		if err != nil {
			t.Fatalf("%s: %v", cmd, err)
		}
		if got, want := commonOf(cfg).Projects, []string{"default", "web"}; !slices.Equal(got, want) {
			t.Errorf("%s: projects %q, want %q", cmd, got, want)
		}
	}
	if _, err := parse(t, nil, CmdCheck, "--projects", "web"); !errors.Is(err, ErrUsage) {
		t.Errorf("--projects: err = %v, want a usage error", err)
	}
}

func TestStatusHasNoRegistryFlags(t *testing.T) {
	t.Parallel()
	var out strings.Builder
	if _, err := Parse([]string{CmdStatus, "-h"}, env(nil), &out); !errors.Is(err, ErrHelp) {
		t.Fatalf("err = %v, want ErrHelp", err)
	}
	for _, f := range []string{"-concurrency", "-timeout", "-registry-auth-file"} {
		if strings.Contains(out.String(), f) {
			t.Errorf("status -h lists %s", f)
		}
	}
	if !strings.Contains(out.String(), "-project ") {
		t.Error("status -h lacks -project")
	}
}

func commonOf(cfg *Config) Common {
	switch {
	case cfg.Check != nil:
		return cfg.Check.Common
	case cfg.Status != nil:
		return cfg.Status.Common
	default:
		return cfg.Serve.Common
	}
}

func TestNotifyFlags(t *testing.T) {
	t.Parallel()
	cfg, err := parse(t, nil, CmdServe, "--notify-url", "https://example.com/a", "--notify-url", "discord://example.com/b  slack://example.com/c",
		"--notify-command", "/path/to/notify --to ops", "--notify-errors")
	if err != nil {
		t.Fatal(err)
	}
	n := cfg.Serve.Notify
	if want := []string{"https://example.com/a", "discord://example.com/b", "slack://example.com/c"}; !slices.Equal(n.NotifyURLs, want) {
		t.Errorf("urls %q, want %q", n.NotifyURLs, want)
	}
	if n.NotifyCommand != "/path/to/notify --to ops" || !n.NotifyErrors {
		t.Errorf("notify %+v", n)
	}
	cfg, err = parse(t, map[string]string{"BEACON_NOTIFY_URL": "https://example.com/a slack://example.com/c"}, CmdServe)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Serve.NotifyURLs) != 2 {
		t.Errorf("BEACON_NOTIFY_URL gave %q, want two URLs", cfg.Serve.NotifyURLs)
	}
	cfg, err = parse(t, map[string]string{"BEACON_NOTIFY_URL_FILE": "/var/lib/incus-beacon/notify-urls", "BEACON_NOTIFY_ERRORS": "true"}, CmdServe)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Serve.NotifyURLFile != "/var/lib/incus-beacon/notify-urls" || !cfg.Serve.NotifyErrors {
		t.Errorf("as install sets it: %+v", cfg.Serve.Notify)
	}
	cfg, err = parse(t, nil, CmdInstall, "--notify-url", "https://example.com/a", "--notify-url", "slack://example.com/b", "--notify-errors")
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Install.NotifyURLs) != 2 || !cfg.Install.NotifyErrs {
		t.Errorf("install notify %q %t", cfg.Install.NotifyURLs, cfg.Install.NotifyErrs)
	}
}

func TestInstallFlags(t *testing.T) {
	t.Parallel()
	cfg, err := parse(t, nil, CmdInstall, "--remote", "r", "--project", "default,apps")
	if err != nil {
		t.Fatal(err)
	}
	c := cfg.Install
	if c.Name != "incus-beacon" || c.Wait != time.Minute || c.Remote != "r" || !slices.Equal(c.Projects, []string{"default", "apps"}) {
		t.Errorf("install config %+v", c)
	}
	if _, err := parse(t, nil, CmdInstall, "--client-cert", "c"); !errors.Is(err, ErrUsage) {
		t.Errorf("install took a connection flag it doesn't use: err %v", err)
	}
	cfg, err = parse(t, nil, CmdUninstall, "--name", "b")
	if err != nil || cfg.Uninstall.Name != "b" {
		t.Errorf("uninstall: %+v, %v", cfg.Uninstall, err)
	}
}

func TestRemoteProject(t *testing.T) {
	t.Parallel()
	for _, cmd := range []string{CmdInstall, CmdUninstall} {
		cfg, err := parse(t, nil, cmd, "--remote", "r:apps")
		if err != nil {
			t.Fatalf("%s: %v", cmd, err)
		}
		var c Common
		if cfg.Install != nil {
			c = cfg.Install.Common
		} else {
			c = cfg.Uninstall.Common
		}
		if c.Remote != "r" || c.RemoteProject != "apps" {
			t.Errorf("%s: remote %q, project %q", cmd, c.Remote, c.RemoteProject)
		}
	}
	cfg, err := parse(t, map[string]string{"BEACON_REMOTE": "r:apps"}, CmdInstall)
	if err != nil || cfg.Install.Remote != "r" || cfg.Install.RemoteProject != "apps" {
		t.Errorf("from env: %+v, %v", cfg, err)
	}
	cfg, err = parse(t, nil, CmdInstall, "--remote", "r")
	if err != nil || cfg.Install.Remote != "r" || cfg.Install.RemoteProject != "" {
		t.Errorf("plain remote: %+v, %v", cfg, err)
	}
	for _, v := range []string{"r:", ":apps", ":"} {
		if _, err := parse(t, nil, CmdInstall, "--remote", v); !errors.Is(err, ErrUsage) {
			t.Errorf("install --remote %q: err = %v, want a usage error", v, err)
		}
	}
	for _, cmd := range []string{CmdCheck, CmdStatus, CmdServe} {
		_, err := parse(t, nil, cmd, "--remote", "r:apps")
		if !errors.Is(err, ErrUsage) || !strings.Contains(err.Error(), "--project") {
			t.Errorf("%s --remote r:apps: err = %v, want a usage error pointing at --project", cmd, err)
		}
	}
}
