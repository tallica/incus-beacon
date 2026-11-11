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
		policy.Prefix + "status":  b.Status,
		policy.Prefix + "current": b.Current,
		policy.Prefix + "latest":  b.Latest,
		policy.Prefix + "error":   b.Error,
		policy.Prefix + "since":   b.Since,
	}
}

func legacyStatusKeys() []string {
	keys := make([]string, 0, len(statusNames))
	for _, n := range statusNames {
		keys = append(keys, policy.LegacyPrefix+n)
	}
	return keys
}

// Incus sends this, with no status code, while a start or stop holds the instance.
const busyText = "Instance is busy running"

const (
	busyRetry    = 500 * time.Millisecond
	busyMaxRetry = 8 * time.Second
)

// WriteStatus skips the API when b is unchanged and under the new names; "" removes a key.
func WriteStatus(ctx context.Context, srv incusclient.InstanceServer, t Target, b Beacon) (bool, error) {
	if b == t.Beacon && !t.LegacyStatus {
		return false, nil
	}
	project := srv.UseProject(t.Project)
	var err error
	conflicts, wait := 0, busyRetry
	for {
		if err = ctx.Err(); err != nil {
			return false, err
		}
		err = writeOnce(project, t.Instance, b)
		if api.StatusErrorCheck(err, http.StatusPreconditionFailed) && conflicts == 0 {
			conflicts++
			continue
		}
		if err == nil || !strings.Contains(err.Error(), busyText) || wait > busyMaxRetry {
			break
		}
		select {
		case <-ctx.Done():
			return false, ctx.Err()
		case <-time.After(wait):
		}
		wait *= 2
	}
	if err != nil {
		return false, fmt.Errorf("write status on %s/%s: %w", t.Project, t.Instance, err)
	}
	return true, nil
}

func writeOnce(srv incusclient.InstanceServer, name string, b Beacon) error {
	inst, etag, err := srv.GetInstance(name)
	if err != nil {
		return err
	}
	put := inst.Writable()
	if put.Config == nil {
		put.Config = map[string]string{}
	}
	for _, k := range legacyStatusKeys() {
		delete(put.Config, k)
	}
	for k, v := range b.keys() {
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
