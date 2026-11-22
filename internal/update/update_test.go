package update

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestSplitImage(t *testing.T) {
	t.Parallel()
	digest := "@sha256:" + strings.Repeat("a", 64)
	for _, tt := range []struct{ ref, server, alias string }{
		{"ghcr.io/tallica/incus-beacon:0.1", "https://ghcr.io", "tallica/incus-beacon:0.1"},
		{"ghcr.io/tallica/incus-beacon", "https://ghcr.io", "tallica/incus-beacon"},
		{"localhost:5000/team/app:v1.2-rc.1", "https://localhost:5000", "team/app:v1.2-rc.1"},
		{"ghcr.io/tallica/incus-beacon:0.6.0-beta.2" + digest, "https://ghcr.io", "tallica/incus-beacon:0.6.0-beta.2" + digest},
	} {
		server, alias, err := SplitImage(tt.ref)
		if err != nil || server != tt.server || alias != tt.alias {
			t.Errorf("%s: got %s %s %v, want %s %s", tt.ref, server, alias, err, tt.server, tt.alias)
		}
	}
	for _, tt := range []struct{ ref, why string }{
		{"tallica/incus-beacon", "no registry"},
		{"image=ghcr.io/tallica/incus-beacon:0.6", "a stray flag name"},
		{"ghcr.io/Tallica/incus-beacon", "repository paths are lower case"},
		{"ghcr.io/tallica/incus-beacon:", "an empty tag"},
		{"ghcr.io/tallica/incus-beacon:0.6 ", "a trailing space"},
		{"ghcr.io/tallica/incus-beacon@sha256:abc", "a short digest"},
		{"ghcr.io/tallica/incus-beacon@sha512:" + strings.Repeat("a", 128), "a digest other than sha256"},
	} {
		if _, _, err := SplitImage(tt.ref); err == nil {
			t.Errorf("%q accepted despite %s", tt.ref, tt.why)
		}
	}
}

func TestPoll(t *testing.T) {
	t.Parallel()
	calls := 0
	err := Poll(context.Background(), time.Second, time.Millisecond, func(context.Context) error {
		if calls++; calls < 3 {
			return errors.New("not yet")
		}
		return nil
	})
	if err != nil || calls != 3 {
		t.Errorf("got %v after %d calls", err, calls)
	}

	err = Poll(context.Background(), 20*time.Millisecond, time.Millisecond, func(context.Context) error {
		return errors.New("probe failed")
	})
	if err == nil || !strings.Contains(err.Error(), "probe failed") {
		t.Errorf("timeout error %v does not carry the last failure", err)
	}
}
