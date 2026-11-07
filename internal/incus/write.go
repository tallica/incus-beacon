package incus

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	incusclient "github.com/lxc/incus/v7/client"
	"github.com/lxc/incus/v7/shared/api"
)

func (b Beacon) keys() map[string]string {
	return map[string]string{
		"user.beacon.status":  b.Status,
		"user.beacon.current": b.Current,
		"user.beacon.latest":  b.Latest,
		"user.beacon.error":   b.Error,
		"user.beacon.since":   b.Since,
	}
}

// Incus sends this, with no status code, while a start or stop holds the instance.
const busyText = "Instance is busy running"

const (
	busyRetry    = 500 * time.Millisecond
	busyMaxRetry = 8 * time.Second
)

// WriteStatus skips the API when b is unchanged; an empty value removes a key.
func WriteStatus(ctx context.Context, srv incusclient.InstanceServer, t Target, b Beacon) (bool, error) {
	if b == t.Beacon {
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
