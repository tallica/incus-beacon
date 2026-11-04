package main

import (
	"strings"
	"testing"

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
		{[]string{"check"}, exitUsage},
	}
	for _, tt := range tests {
		if code, _, _ := runArgs(t, tt.args...); code != tt.want {
			t.Errorf("%q: exit %d, want %d", tt.args, code, tt.want)
		}
	}
}

func TestLogsGoToStderr(t *testing.T) {
	t.Parallel()
	_, out, errOut := runArgs(t, "check", "--log-format", "json")
	if out != "" {
		t.Errorf("stdout %q, want nothing", out)
	}
	if !strings.Contains(errOut, `"command":"check"`) {
		t.Errorf("stderr %q, want a JSON log line naming the command", errOut)
	}
}
