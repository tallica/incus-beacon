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
	server, alias, err := SplitImage("ghcr.io/tallica/incus-beacon:0.1")
	if err != nil || server != "https://ghcr.io" || alias != "tallica/incus-beacon:0.1" {
		t.Errorf("got %s %s %v", server, alias, err)
	}
	if _, _, err := SplitImage("tallica/incus-beacon"); err == nil {
		t.Error("a ref without a registry was accepted")
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
