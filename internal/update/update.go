// Package update holds the Incus calls that rebuild an instance on another image.
package update

import (
	"context"
	"fmt"
	"strings"
	"time"

	incusclient "github.com/lxc/incus/v7/client"
	"github.com/lxc/incus/v7/shared/api"
)

// SplitImage turns registry/repo:tag into the OCI source Incus pulls from.
func SplitImage(ref string) (server, alias string, err error) {
	host, rest, ok := strings.Cut(ref, "/")
	if !ok || !strings.ContainsAny(host, ".:") || rest == "" {
		return "", "", fmt.Errorf("image %q: want registry/repository[:tag]", ref)
	}
	return "https://" + host, rest, nil
}

// SetState runs a start or stop and waits for it; a stop is forced.
func SetState(ctx context.Context, srv incusclient.InstanceServer, name, action string) error {
	op, err := srv.UpdateInstanceState(name, api.InstanceStatePut{Action: action, Timeout: -1, Force: action == "stop"}, "")
	if err != nil {
		return err
	}
	return op.WaitContext(ctx)
}

// Stop asks the instance to stop within timeout, then forces it.
func Stop(ctx context.Context, srv incusclient.InstanceServer, name string, timeout time.Duration) error {
	// A timeout of 0 would make Incus force the stop at once.
	op, err := srv.UpdateInstanceState(name, api.InstanceStatePut{Action: "stop", Timeout: max(int(timeout.Seconds()), 1)}, "")
	if err == nil {
		err = op.WaitContext(ctx)
	}
	if err == nil {
		return nil
	}
	return SetState(ctx, srv, name, "stop")
}

// Rebuild resets the root filesystem from src; config, devices and volumes stay.
func Rebuild(ctx context.Context, srv incusclient.InstanceServer, name string, src api.InstanceSource) error {
	op, err := srv.RebuildInstance(name, api.InstanceRebuildPost{Source: src})
	if err != nil {
		return err
	}
	return op.WaitContext(ctx)
}

// Poll calls check every interval until it succeeds or wait has passed.
func Poll(ctx context.Context, wait, every time.Duration, check func(context.Context) error) error {
	ctx, cancel := context.WithTimeout(ctx, wait)
	defer cancel()
	tick := time.NewTicker(every)
	defer tick.Stop()
	var last error
	for {
		if last = check(ctx); last == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("not ready within %s: %w", wait, last)
		case <-tick.C:
		}
	}
}
