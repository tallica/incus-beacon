package incus

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	incusclient "github.com/lxc/incus/v7/client"
	"github.com/lxc/incus/v7/shared/api"

	"github.com/tallica/incus-beacon/internal/policy"
)

var statusNames = []string{"status", "current", "latest", "error", "since"}

func (b Beacon) keys() map[string]string {
	return map[string]string{
		policy.Prefix + "status":        b.Status,
		policy.Prefix + "current":       b.Current,
		policy.Prefix + "current-image": b.CurrentImage,
		policy.Prefix + "latest":        b.Latest,
		policy.Prefix + "error":         b.Error,
		policy.Prefix + "since":         b.Since,
	}
}

// Update is what the last automatic or manual update left on an instance.
type Update struct {
	Applied string // the ref beacon last left it on: rebuilt onto, or rolled back to
	// AppliedImage is the fingerprint Applied was recorded for; another means someone moved it since.
	AppliedImage string
	Requested    string // the ref it was asked for before beacon first moved it
	Status       string
	Reason       string
	Failed       string // a ref that failed, not tried again until another is offered
}

// ClaimKey holds the claim of the process updating the instance, while it does.
const ClaimKey = policy.Prefix + "update-claim"

// Keys maps u to its config keys; "" removes a key.
func (u Update) Keys() map[string]string {
	return map[string]string{
		policy.Prefix + "applied":       u.Applied,
		policy.Prefix + "applied-image": u.AppliedImage,
		policy.Prefix + "requested":     u.Requested,
		policy.Prefix + "update-status": u.Status,
		policy.Prefix + "update-reason": u.Reason,
		policy.Prefix + "update-failed": u.Failed,
	}
}

func readUpdate(config map[string]string) Update {
	return Update{
		Applied:      config[policy.Prefix+"applied"],
		AppliedImage: config[policy.Prefix+"applied-image"],
		Requested:    config[policy.Prefix+"requested"],
		Status:       config[policy.Prefix+"update-status"],
		Reason:       config[policy.Prefix+"update-reason"],
		Failed:       config[policy.Prefix+"update-failed"],
	}
}

func legacyStatusKeys() []string {
	keys := make([]string, 0, len(statusNames))
	for _, n := range statusNames {
		keys = append(keys, policy.LegacyPrefix+n)
	}
	return keys
}

// Incus sends these, with no status code: a start or stop holds the instance, or another
// config update did and this request was dropped once it ended (operationlock in Incus 7.6).
const (
	busyText   = "Instance is busy running"
	lockedText = "A matching non-reusable operation has now succeeded"
)

// IsBusy reports a request Incus refused for another operation on the instance; it may be retried.
func IsBusy(err error) bool {
	return err != nil && (strings.Contains(err.Error(), busyText) || strings.Contains(err.Error(), lockedText))
}

const (
	busyRetry    = 500 * time.Millisecond
	busyMaxRetry = 8 * time.Second
)

// WriteStatus skips the API when b is unchanged and under the new names; "" removes a key.
func WriteStatus(ctx context.Context, srv incusclient.InstanceServer, t Target, b Beacon) (bool, error) {
	if b == t.Beacon && !t.LegacyStatus {
		return false, nil
	}
	if err := write(ctx, srv, t, b.keys(), legacyStatusKeys()); err != nil {
		return false, fmt.Errorf("write status on %s/%s: %w", t.Project, t.Instance, err)
	}
	return true, nil
}

// WriteUpdate records u, skipping the API when it is unchanged; "" removes a key.
func WriteUpdate(ctx context.Context, srv incusclient.InstanceServer, t Target, u Update) (bool, error) {
	if u == t.Update {
		return false, nil
	}
	if err := write(ctx, srv, t, u.Keys(), nil); err != nil {
		return false, fmt.Errorf("write update keys on %s/%s: %w", t.Project, t.Instance, err)
	}
	return true, nil
}

func write(ctx context.Context, srv incusclient.InstanceServer, t Target, keys map[string]string, drop []string) error {
	project := srv.UseProject(t.Project)
	var err error
	conflicts, wait := 0, busyRetry
	for {
		if err = ctx.Err(); err != nil {
			return err
		}
		err = writeOnce(project, t.Instance, keys, drop)
		if api.StatusErrorCheck(err, http.StatusPreconditionFailed) && conflicts == 0 {
			conflicts++
			continue
		}
		if err == nil || !IsBusy(err) || wait > busyMaxRetry {
			break
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(wait):
		}
		wait *= 2
	}
	return err
}

func writeOnce(srv incusclient.InstanceServer, name string, keys map[string]string, drop []string) error {
	inst, etag, err := srv.GetInstance(name)
	if err != nil {
		return err
	}
	put := inst.Writable()
	if put.Config == nil {
		put.Config = map[string]string{}
	}
	for _, k := range drop {
		delete(put.Config, k)
	}
	for k, v := range keys {
		if v == "" {
			delete(put.Config, k)
		} else {
			put.Config[k] = v
		}
	}
	op, err := srv.UpdateInstance(name, put, etag)
	if err != nil {
		return err
	}
	return op.Wait()
}
