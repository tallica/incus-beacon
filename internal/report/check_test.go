package report

import (
	"bytes"
	"testing"

	"github.com/tallica/incus-beacon/internal/check"
	"github.com/tallica/incus-beacon/internal/incus"
)

func checkResults() []check.Result {
	tg := func(project, name string) incus.Target { return incus.Target{Project: project, Instance: name} }
	return []check.Result{
		{Target: tg("apps", "api"), Status: check.UpdateAvailable, Current: "ghcr.io/example/api:1.2.0", Latest: "1.3.0"},
		{Target: tg("apps", "worker"), Status: check.UpdateAvailable, Current: "ghcr.io/example/worker:3", Latest: "3@sha256:bbbbbbbbbbbb"},
		{Target: tg("apps", "orphan"), Status: check.Unknown, Reason: "no registry ref"},
		{Target: tg("default", "db"), Status: check.Error, Current: "docker.io/library/postgres:17", Reason: "rate limited until 2026-11-04T18:00:00Z"},
		{Target: tg("default", "web"), Status: check.Current, Current: "docker.io/library/nginx:1.27"},
	}
}

func TestCheckTable(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	if err := WriteCheck(&buf, "table", CheckRows(checkResults())); err != nil {
		t.Fatal(err)
	}
	golden(t, "check.txt", buf.Bytes())
}

func TestCheckJSON(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	if err := WriteCheck(&buf, "json", CheckRows(checkResults())); err != nil {
		t.Fatal(err)
	}
	golden(t, "check.json", buf.Bytes())
}
