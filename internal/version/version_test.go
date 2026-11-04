package version

import "testing"

func TestString(t *testing.T) {
	Version, Commit, Date = "v1.2.3", "0123456789abcdef", "2026-01-01T00:00:00Z"
	t.Cleanup(func() { Version, Commit, Date = "dev", "", "" })

	if got, want := String(), "v1.2.3 (0123456, 2026-01-01T00:00:00Z)"; got != want {
		t.Errorf("String() = %q, want %q", got, want)
	}
}
