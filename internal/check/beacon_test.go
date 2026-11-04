package check

import (
	"testing"
	"time"

	"github.com/tallica/incus-beacon/internal/incus"
)

func TestBeacon(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 4, 12, 0, 0, 0, time.FixedZone("CET", 3600))
	const stamp = "2026-11-04T11:00:00Z"
	const earlier = "2026-11-01T08:00:00Z"
	tests := []struct {
		name string
		prev incus.Beacon
		r    Result
		want incus.Beacon
	}{
		{
			"first check",
			incus.Beacon{},
			Result{Status: Current, Current: "ghcr.io/example/app:1"},
			incus.Beacon{Status: "current", Current: "ghcr.io/example/app:1", Since: stamp},
		},
		{
			"unchanged keeps since",
			incus.Beacon{Status: "update-available", Current: "a:1", Latest: "2", Since: earlier},
			Result{Status: UpdateAvailable, Current: "a:1", Latest: "2"},
			incus.Beacon{Status: "update-available", Current: "a:1", Latest: "2", Since: earlier},
		},
		{
			"newer latest moves since",
			incus.Beacon{Status: "update-available", Current: "a:1", Latest: "2", Since: earlier},
			Result{Status: UpdateAvailable, Current: "a:1", Latest: "3"},
			incus.Beacon{Status: "update-available", Current: "a:1", Latest: "3", Since: stamp},
		},
		{
			"error keeps last latest",
			incus.Beacon{Status: "update-available", Current: "a:1", Latest: "2", Since: earlier},
			Result{Status: Error, Current: "a:1", Reason: "rate limited"},
			incus.Beacon{Status: "error", Current: "a:1", Latest: "2", Error: "rate limited", Since: stamp},
		},
		{
			"recovery clears the error",
			incus.Beacon{Status: "error", Current: "a:1", Error: "registry timeout", Since: earlier},
			Result{Status: Current, Current: "a:1"},
			incus.Beacon{Status: "current", Current: "a:1", Since: stamp},
		},
	}
	for _, tt := range tests {
		tt.r.Target.Beacon = tt.prev
		if got := Beacon(tt.r, now); got != tt.want {
			t.Errorf("%s:\n got %+v\nwant %+v", tt.name, got, tt.want)
		}
	}
}
