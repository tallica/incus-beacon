package report

import (
	"bytes"
	"errors"
	"flag"
	"os"
	"path/filepath"
	"testing"

	"github.com/tallica/incus-beacon/internal/incus"
)

var update = flag.Bool("update", false, "rewrite golden files in testdata/")

func golden(t *testing.T, name string, got []byte) {
	t.Helper()
	path := filepath.Join("testdata", name)
	if *update {
		if err := os.WriteFile(path, got, 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%v (run go test -update to create it)", err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("%s differs:\n--- got\n%s\n--- want\n%s", name, got, want)
	}
}

func statusTargets() []incus.Target {
	return []incus.Target{
		{
			Project: "apps", Instance: "api", Ref: "ghcr.io/example/api:1.2.0",
			Beacon: incus.Beacon{
				Status: "update-available", Current: "ghcr.io/example/api:1.2.0",
				Latest: "1.3.0", Since: "2026-11-01T10:00:00Z",
			},
		},
		{Project: "apps", Instance: "bad", Ref: "ghcr.io/example/bad:1", PolicyErr: errors.New(`track "weird": must be digest or semver`)},
		{Project: "apps", Instance: "orphan"},
		{
			Project: "default", Instance: "db", Ref: "docker.io/library/postgres:17",
			Beacon: incus.Beacon{
				Status: "error", Current: "docker.io/library/postgres:17",
				Error: "rate limited", Since: "2026-11-02T08:30:00Z",
			},
		},
		{Project: "default", Instance: "web", Ref: "docker.io/library/nginx:1.27", Beacon: incus.Beacon{Status: "current"}},
	}
}

func TestStatusTable(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	if err := WriteStatus(&buf, "table", StatusRows(statusTargets(), false)); err != nil {
		t.Fatal(err)
	}
	golden(t, "status.txt", buf.Bytes())
}

func TestStatusJSON(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	if err := WriteStatus(&buf, "json", StatusRows(statusTargets(), false)); err != nil {
		t.Fatal(err)
	}
	golden(t, "status.json", buf.Bytes())
}

func TestStatusOnlyUpdates(t *testing.T) {
	t.Parallel()
	rows := StatusRows(statusTargets(), true)
	if len(rows) != 1 || rows[0].Instance != "api" {
		t.Errorf("only-updates rows %+v, want just apps/api", rows)
	}
	var buf bytes.Buffer
	if err := WriteStatus(&buf, "json", StatusRows(nil, false)); err != nil {
		t.Fatal(err)
	}
	if got := buf.String(); got != "[]\n" {
		t.Errorf("empty JSON %q, want []", got)
	}
}
