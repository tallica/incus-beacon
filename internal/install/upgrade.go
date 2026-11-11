package install

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	incusclient "github.com/lxc/incus/v7/client"
	"github.com/lxc/incus/v7/shared/api"
	"github.com/rs/zerolog"

	"github.com/tallica/incus-beacon/internal/semver"
)

type UpgradeOptions struct {
	Name  string
	Image string
	Force bool // allow a downgrade
	Wait  time.Duration
	Log   zerolog.Logger
}

// readyProbe is the image's own HEALTHCHECK.
var readyProbe = []string{"wget", "-q", "-O", "/dev/null", "http://127.0.0.1:9153/ready"}

// Upgrade follows docs/ARCHITECTURE.md#connecting-to-incus.
func Upgrade(ctx context.Context, srv incusclient.InstanceServer, opts UpgradeOptions) error {
	srv, inst, err := findInstance(srv, opts.Name, opts.Log)
	if notFound(err) {
		return fmt.Errorf("instance %s not found; install it first", opts.Name)
	}
	if err != nil {
		return err
	}
	if inst.Devices[stateDev]["source"] != VolumeName(opts.Name) || inst.Config["environment.BEACON_TOKEN_FILE"] != tokenFile {
		return fmt.Errorf("instance %s was not created by install", opts.Name)
	}
	server, alias, err := splitImage(opts.Image)
	if err != nil {
		return err
	}
	from := inst.Config["image.id"]
	if older(tagOf(alias), tagOf(from)) && !opts.Force {
		return fmt.Errorf("%s is older than the running %s; give --force to downgrade", opts.Image, from)
	}
	// Incus refuses to rebuild an instance that has snapshots.
	snaps, err := srv.GetInstanceSnapshotNames(opts.Name)
	if err != nil {
		return fmt.Errorf("list snapshots of %s: %w", opts.Name, err)
	}
	if len(snaps) > 0 {
		return fmt.Errorf("instance %s has snapshots (%s), which rebuild refuses; delete them first", opts.Name, strings.Join(snaps, ", "))
	}
	old := inst.Config["volatile.base_image"]
	if old == "" {
		return fmt.Errorf("instance %s records no base image to roll back to", opts.Name)
	}

	if inst.StatusCode != api.Stopped {
		if err := setState(ctx, srv, opts.Name, "stop"); err != nil {
			return fmt.Errorf("stop %s: %w", opts.Name, err)
		}
	}
	err = rebuild(ctx, srv, opts.Name, api.InstanceSource{Type: "image", Protocol: "oci", Server: server, Alias: alias})
	if err == nil {
		opts.Log.Info().Str("instance", opts.Name).Str("from", from).Str("image", opts.Image).Msg("instance rebuilt")
		err = setState(ctx, srv, opts.Name, "start")
	}
	if err == nil {
		err = waitReady(ctx, srv, opts.Name, opts.Wait)
	}
	if err == nil {
		opts.Log.Info().Str("instance", opts.Name).Str("image", opts.Image).Msg("upgraded and ready")
		return nil
	}

	err = fmt.Errorf("upgrade %s to %s: %w", opts.Name, opts.Image, err)
	opts.Log.Warn().Err(err).Str("image", from).Msg("rolling back")
	rerr := setState(ctx, srv, opts.Name, "stop")
	if rerr == nil {
		rerr = rebuild(ctx, srv, opts.Name, api.InstanceSource{Type: "image", Fingerprint: old})
	}
	if rerr == nil {
		rerr = setState(ctx, srv, opts.Name, "start")
	}
	if rerr != nil {
		return errors.Join(err, fmt.Errorf("roll back to %s: %w", from, rerr))
	}
	return fmt.Errorf("%w; rolled back to %s", err, from)
}

// older is true only for two version tags of one shape, such as 0.4 and 0.5.
func older(tag, than string) bool {
	v, ok := semver.Parse(tag)
	w, ok2 := semver.Parse(than)
	return ok && ok2 && v.SameShape(w) && v.Less(w)
}

// tagOf is the tag after the last colon, unless that colon is a registry port.
func tagOf(ref string) string {
	i := strings.LastIndexByte(ref, ':')
	if i < 0 || strings.Contains(ref[i:], "/") {
		return ""
	}
	return ref[i+1:]
}

func rebuild(ctx context.Context, srv incusclient.InstanceServer, name string, src api.InstanceSource) error {
	op, err := srv.RebuildInstance(name, api.InstanceRebuildPost{Source: src})
	if err != nil {
		return err
	}
	return op.WaitContext(ctx)
}

func waitReady(ctx context.Context, srv incusclient.InstanceServer, name string, wait time.Duration) error {
	ctx, cancel := context.WithTimeout(ctx, wait)
	defer cancel()
	tick := time.NewTicker(2 * time.Second)
	defer tick.Stop()
	var last error
	for {
		if last = probe(ctx, srv, name); last == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("not ready within %s: %w", wait, last)
		case <-tick.C:
		}
	}
}

func probe(ctx context.Context, srv incusclient.InstanceServer, name string) error {
	op, err := srv.ExecInstance(name, api.InstanceExecPost{Command: readyProbe}, nil)
	if err != nil {
		return err
	}
	if err := op.WaitContext(ctx); err != nil {
		return err
	}
	if code, _ := op.Get().Metadata["return"].(float64); code != 0 {
		return fmt.Errorf("/ready probe exited %d", int(code))
	}
	return nil
}
