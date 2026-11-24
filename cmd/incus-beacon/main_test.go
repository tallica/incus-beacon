package main

import (
	"strings"
	"testing"
	"time"

	"github.com/tallica/incus-beacon/internal/config"
	"github.com/tallica/incus-beacon/internal/incus"
	"github.com/tallica/incus-beacon/internal/version"
)

func runArgs(t *testing.T, args ...string) (code int, stdout, stderr string) {
	t.Helper()
	var out, errOut strings.Builder
	code = run(t.Context(), args, func(string) string { return "" }, &out, &errOut)
	return code, out.String(), errOut.String()
}

func TestVersion(t *testing.T) {
	t.Parallel()
	code, out, _ := runArgs(t, "version")
	if code != exitOK || strings.TrimSpace(out) != version.String() {
		t.Errorf("code %d, stdout %q", code, out)
	}
}

func TestExitCodes(t *testing.T) {
	t.Parallel()
	tests := []struct {
		args []string
		want int
	}{
		{nil, exitUsage},
		{[]string{"-h"}, exitOK},
		{[]string{"nope"}, exitUsage},
		{[]string{"serve", "--interval", "10s"}, exitUsage},
		{[]string{"status", "--remote", "no-such-remote-for-tests"}, exitError},
	}
	for _, tt := range tests {
		if code, _, _ := runArgs(t, tt.args...); code != tt.want {
			t.Errorf("%q: exit %d, want %d", tt.args, code, tt.want)
		}
	}
}

// Every command reaches its own code: an unreachable remote is an error, never a
// panic. serve is left out, since it retries the remote until it is stopped.
func TestEveryCommandRuns(t *testing.T) {
	t.Parallel()
	extra := map[string][]string{config.CmdUpdate: {"--instance", "web"}, config.CmdConfigure: {"--events"}}
	for _, cmd := range config.Commands {
		if cmd == config.CmdVersion || cmd == config.CmdServe {
			continue
		}
		args := append([]string{cmd, "--remote", "no-such-remote-for-tests"}, extra[cmd]...)
		if code, _, errOut := runArgs(t, args...); code != exitError {
			t.Errorf("%q: exit %d, want %d; stderr %s", args, code, exitError, errOut)
		}
	}
}

func TestLogsGoToStderr(t *testing.T) {
	t.Parallel()
	_, out, errOut := runArgs(t, "status", "--remote", "no-such-remote-for-tests", "--log-format", "json")
	if out != "" {
		t.Errorf("stdout %q, want nothing", out)
	}
	if !strings.Contains(errOut, `"command":"status"`) {
		t.Errorf("stderr %q, want a JSON log line naming the command", errOut)
	}
}

func TestClaimedElsewhere(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 23, 12, 0, 0, 0, time.UTC)
	for name, tc := range map[string]struct {
		claim string
		want  bool
	}{
		"no claim":   {"", false},
		"live":       {`{"owner":"other/7","expires":"2026-11-23T12:30:00Z"}`, true},
		"abandoned":  {`{"owner":"other/7","expires":"2026-11-23T11:30:00Z"}`, false},
		"unreadable": {"not json", true},
	} {
		if got := claimedElsewhere(incus.Target{Claim: tc.claim}, now); got != tc.want {
			t.Errorf("%s: claimedElsewhere %v, want %v", name, got, tc.want)
		}
	}
}
