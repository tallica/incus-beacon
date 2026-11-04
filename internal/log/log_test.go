package log

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/tallica/incus-beacon/internal/config"
)

func TestLevelFilters(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	logger, err := New(&buf, "warn", config.FormatJSON)
	if err != nil {
		t.Fatal(err)
	}
	logger.Info().Msg("hidden")
	logger.Warn().Str("instance", "web").Msg("shown")

	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) != 1 {
		t.Fatalf("got %d lines, want 1: %q", len(lines), buf.String())
	}
	var rec map[string]any
	if err := json.Unmarshal([]byte(lines[0]), &rec); err != nil {
		t.Fatal(err)
	}
	if rec["level"] != "warn" || rec["instance"] != "web" || rec["message"] != "shown" || rec["time"] == nil {
		t.Errorf("record %v", rec)
	}
}

func TestAutoIsJSONOffTerminal(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	logger, err := New(&buf, "info", config.FormatAuto)
	if err != nil {
		t.Fatal(err)
	}
	logger.Info().Msg("hello")
	if !json.Valid(bytes.TrimSpace(buf.Bytes())) {
		t.Errorf("auto format to a buffer is not JSON: %q", buf.String())
	}
}

func TestConsoleFormat(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	logger, err := New(&buf, "info", config.FormatConsole)
	if err != nil {
		t.Fatal(err)
	}
	logger.Info().Msg("hello")
	if json.Valid(bytes.TrimSpace(buf.Bytes())) || !strings.Contains(buf.String(), "hello") {
		t.Errorf("console output %q", buf.String())
	}
}

func TestInvalid(t *testing.T) {
	t.Parallel()
	if _, err := New(&bytes.Buffer{}, "loud", config.FormatJSON); err == nil {
		t.Error("unknown level accepted")
	}
	if _, err := New(&bytes.Buffer{}, "info", "xml"); err == nil {
		t.Error("unknown format accepted")
	}
}
