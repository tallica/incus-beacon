package update

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"os"
	"path"
	"slices"
	"strconv"
	"strings"
	"time"

	incusclient "github.com/lxc/incus/v7/client"
	"github.com/lxc/incus/v7/shared/api"
	"github.com/rs/zerolog"

	"github.com/tallica/incus-beacon/internal/incus"
	"github.com/tallica/incus-beacon/internal/registry"
)

// Outcomes, as written to user.incus-beacon.update-status.
const (
	Updated    = "updated"
	RolledBack = "rolled-back"
	Failed     = "failed" // the rollback failed too
	Refused    = "refused"
	// Ready is Preview's outcome when Apply would go ahead; it is never written.
	Ready = "ready"
)

const (
	snapshotPrefix = "incus-beacon-"
	healthEnabled  = "user.healthcheck.enabled"
	healthStatus   = "user.healthcheck.status"
)

type Registry interface {
	Resolve(ctx context.Context, ref registry.Ref, p registry.Platform) (registry.Image, error)
	Env(ctx context.Context, ref registry.Ref, img registry.Image) ([]string, error)
}

type Options struct {
	StopTimeout time.Duration
	Wait        time.Duration // for the health gate, after each start
	Settle      time.Duration // how long the instance must stay running
	Keep        int           // beacon's snapshots kept per volume
	// Lease is how long a claim holds after each step; an older one counts as abandoned.
	Lease time.Duration
	Every time.Duration // poll interval
	Owner string        // names this process in a claim
	Now   func() time.Time
	Log   zerolog.Logger
}

func (o Options) withDefaults() Options {
	o.StopTimeout = cmp.Or(o.StopTimeout, 30*time.Second)
	o.Wait = cmp.Or(o.Wait, 5*time.Minute)
	o.Settle = cmp.Or(o.Settle, 30*time.Second)
	o.Lease = cmp.Or(o.Lease, 30*time.Minute+2*o.Wait)
	o.Every = cmp.Or(o.Every, 2*time.Second)
	if o.Owner == "" {
		host, _ := os.Hostname()
		o.Owner = cmp.Or(host, "incus-beacon") + "/" + strconv.Itoa(os.Getpid())
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	return o
}

type Outcome struct {
	Status     string
	Reason     string
	From, To   string
	Snapshots  []string // pool/volume/snapshot
	Unrestored []string // pool/volume, shared with another instance
	Dropped    []string // environment keys that held an image's own values
	// Kept keys are overrides or, with OldEnvUnread, maybe copies from an unreadable old image.
	Kept         []string
	OldEnvUnread bool
}

// NextRef is current's repository at latest's tag; a rebuilt tag reads tag@digest.
func NextRef(current, latest string) (string, error) {
	ref, err := registry.ParseRef(current)
	if err != nil {
		return "", err
	}
	tag, _, _ := strings.Cut(latest, "@")
	if tag == "" {
		return "", fmt.Errorf("no tag in %q", latest)
	}
	ref.Tag, ref.Digest = tag, ""
	return ref.String(), nil
}

// claim, in incus.ClaimKey, holds what another process needs to roll back if this one dies.
type claim struct {
	Owner    string            `json:"owner"`
	Expires  time.Time         `json:"expires"`
	Step     int               `json:"step"` // the last step begun, -1 before the first
	To       string            `json:"to"`
	From     string            `json:"from_image"`
	Snapshot string            `json:"snapshot,omitempty"`
	Volumes  []string          `json:"volumes,omitempty"` // pool/name, restored on rollback
	Dropped  map[string]string `json:"dropped,omitempty"`
}

var errTaken = errors.New("the claim was taken over")

// Abandoned reports a readable claim whose lease has run out.
func Abandoned(raw string, now time.Time) bool {
	var c claim
	return raw != "" && json.Unmarshal([]byte(raw), &c) == nil && !now.Before(c.Expires)
}

// Apply follows docs/ARCHITECTURE.md#updating and records the outcome on the instance.
func Apply(ctx context.Context, srv incusclient.InstanceServer, reg Registry, t incus.Target, to string, opts Options) (Outcome, error) {
	return newUpdater(srv, reg, t, to, opts.withDefaults()).apply(ctx)
}

func newUpdater(srv incusclient.InstanceServer, reg Registry, t incus.Target, to string, opts Options) *updater {
	return &updater{
		srv: srv.UseProject(t.Project), reg: reg, t: t, opts: opts,
		log: opts.Log.With().Str("project", t.Project).Str("instance", t.Instance).Str("to", to).Logger(),
		o:   Outcome{From: t.Ref, To: to},
	}
}

type updater struct {
	srv  incusclient.InstanceServer
	reg  Registry
	t    incus.Target
	opts Options
	log  zerolog.Logger

	inst *api.Instance
	c    claim
	vols []volume
	o    Outcome
}

type volume struct {
	pool, name string
	shared     bool
}

type step struct {
	name     string
	do, undo func(context.Context) error
}

// steps run in order; a failure undoes every step begun, the failed one first.
func (u *updater) steps() []step {
	return []step{
		{"stop", u.stop, u.startOld},
		{"snapshot", u.snapshot, nil},
		{
			"drop environment keys", func(ctx context.Context) error { return u.setEnv(ctx, nil, u.c.Dropped) },
			func(ctx context.Context) error { return u.setEnv(ctx, u.c.Dropped, nil) },
		},
		{"rebuild", u.rebuild, u.rebuildOld},
		{"start", u.start, u.restore},
	}
}

// Preview runs Apply's read-only checks and says what it would do; it changes nothing.
func Preview(ctx context.Context, srv incusclient.InstanceServer, reg Registry, t incus.Target, to string, opts Options) Outcome {
	u := newUpdater(srv, reg, t, to, opts.withDefaults())
	if _, reason := u.prepare(ctx); reason != "" {
		u.o.Status, u.o.Reason = Refused, reason
		return u.o
	}
	if u.c.Owner != "" {
		u.o.Status, u.o.Reason = Refused, "an interrupted update by "+u.c.Owner+" would be rolled back first"
		return u.o
	}
	u.o.Status = Ready
	for _, v := range u.vols {
		u.o.Snapshots = append(u.o.Snapshots, v.pool+"/"+v.name)
		if v.shared {
			u.o.Unrestored = append(u.o.Unrestored, v.pool+"/"+v.name)
		}
	}
	return u.o
}

// prepare runs every check before a claim; an abandoned claim is left in u.c, with no reason.
func (u *updater) prepare(ctx context.Context) (dropped map[string]string, reason string) {
	inst, _, err := u.srv.GetInstance(u.t.Instance)
	if err != nil {
		return nil, err.Error()
	}
	u.inst = inst
	if raw := inst.Config[incus.ClaimKey]; raw != "" {
		if err := json.Unmarshal([]byte(raw), &u.c); err != nil {
			return nil, "unreadable " + incus.ClaimKey + "; remove it once no update runs: " + err.Error()
		}
		if u.opts.Now().Before(u.c.Expires) {
			return nil, "being updated by " + u.c.Owner + ", whose claim holds until " + u.c.Expires.UTC().Format(time.RFC3339)
		}
		return nil, ""
	}
	if reason := u.refusal(); reason != "" {
		return nil, reason
	}
	if dropped, err = u.staleEnv(ctx); err != nil {
		return nil, "read the images' environment: " + err.Error()
	}
	if u.vols, err = u.volumes(); err != nil {
		return nil, err.Error()
	}
	if _, _, err := SplitImage(u.o.To); err != nil {
		return nil, err.Error()
	}
	return dropped, ""
}

func (u *updater) apply(ctx context.Context) (Outcome, error) {
	dropped, reason := u.prepare(ctx)
	switch {
	case reason != "" && u.c.Owner != "":
		return u.busy(reason)
	case reason != "":
		return u.refuse(ctx, reason)
	case u.c.Owner != "":
		return u.recover(ctx, u.c)
	}

	u.c = claim{
		Owner: u.opts.Owner, Step: -1, To: u.o.To, From: u.inst.Config["volatile.base_image"], Dropped: dropped,
		Snapshot: snapshotPrefix + u.opts.Now().UTC().Format("20060102T150405"),
	}
	for _, v := range u.vols {
		if !v.shared {
			u.c.Volumes = append(u.c.Volumes, v.pool+"/"+v.name)
		}
	}
	if err := u.acquire(ctx); err != nil {
		return u.busy("claim the instance: " + err.Error())
	}
	u.log.Info().Msg("updating")
	for i, s := range u.steps() {
		if err := u.begin(ctx, i); err != nil {
			if errors.Is(err, errTaken) {
				return u.abandon(err)
			}
			return u.unwind(ctx, i-1, "record the claim: "+err.Error())
		}
		if err := u.bounded(ctx, s.do); err != nil {
			return u.unwind(ctx, i, s.name+": "+err.Error())
		}
	}
	u.prune()
	u.o.Status = Updated
	u.log.Info().Msg("updated")
	return u.o, u.release(ctx)
}

func (u *updater) refuse(ctx context.Context, reason string) (Outcome, error) {
	u.o.Status, u.o.Reason = Refused, reason
	u.log.Warn().Str("reason", reason).Msg("update refused")
	_, err := incus.WriteUpdate(ctx, u.srv, u.t, u.record())
	return u.o, err
}

// busy refuses without writing: another process's update owns the instance's keys.
func (u *updater) busy(reason string) (Outcome, error) {
	u.o.Status, u.o.Reason = Refused, reason
	u.log.Warn().Str("reason", reason).Msg("update refused")
	return u.o, nil
}

// abandon stops without touching the instance: the process that took the claim owns it now.
func (u *updater) abandon(err error) (Outcome, error) {
	u.o.Status, u.o.Reason = Failed, err.Error()
	u.log.Error().Err(err).Msg("update abandoned")
	return u.o, nil
}

// recover rolls back an update whose claim expired, from what the claim recorded.
func (u *updater) recover(ctx context.Context, c claim) (Outcome, error) {
	u.c, u.o.To = c, c.To
	for _, v := range c.Volumes {
		pool, name, _ := strings.Cut(v, "/")
		u.vols = append(u.vols, volume{pool: pool, name: name})
	}
	steps := u.steps()
	u.c.Step = min(c.Step, len(steps)-1)
	u.c.Owner, u.c.Expires = u.opts.Owner, u.opts.Now().Add(u.opts.Lease)
	if err := u.write(ctx, func(cfg map[string]string) error {
		if cfg[incus.ClaimKey] == "" {
			return errTaken
		}
		return u.putClaim(cfg)
	}); err != nil {
		return u.refuse(ctx, "take over the abandoned claim of "+c.Owner+": "+err.Error())
	}
	cause := "the update by " + c.Owner + " was interrupted"
	if u.c.Step >= 0 {
		cause += " at " + steps[u.c.Step].name
	}
	u.log.Warn().Str("reason", cause).Msg("recovering")
	return u.unwind(ctx, u.c.Step, cause)
}

func (u *updater) unwind(ctx context.Context, begun int, cause string) (Outcome, error) {
	// A rollback must finish even when the caller gives up or the process is stopping.
	ctx = context.WithoutCancel(ctx)
	u.o.Reason = cause
	if begun < 0 {
		// Nothing changed, so the image is not to blame.
		u.o.Status = Refused
		u.log.Warn().Str("reason", cause).Msg("update stopped before any step")
		return u.o, u.release(ctx)
	}
	u.log.Warn().Str("reason", cause).Msg("rolling back")
	steps := u.steps()
	for i := begun; i >= 0; i-- {
		if steps[i].undo == nil {
			continue
		}
		// Renewed per undo, so a long rollback is never taken for an abandoned one.
		if err := u.begin(ctx, i); errors.Is(err, errTaken) {
			return u.abandon(err)
		} else if err != nil {
			u.log.Warn().Err(err).Msg("claim not renewed; rolling back anyway")
		}
		if err := u.bounded(ctx, steps[i].undo); err != nil {
			u.o.Status, u.o.Reason = Failed, cause+"; roll back "+steps[i].name+": "+err.Error()
			u.log.Error().Str("reason", u.o.Reason).Msg("rollback failed")
			return u.o, u.release(ctx)
		}
	}
	u.o.Status = RolledBack
	u.log.Warn().Str("reason", cause).Msg("rolled back")
	return u.o, u.release(ctx)
}

// bounded keeps a step or undo inside the claim's lease, so a hung call can't race a takeover.
func (u *updater) bounded(ctx context.Context, f func(context.Context) error) error {
	ctx, cancel := context.WithTimeout(ctx, u.opts.Lease-u.opts.Lease/10)
	defer cancel()
	return f(ctx)
}

func (u *updater) record() incus.Update {
	prev := u.t.Update
	switch u.o.Status {
	case Updated:
		return incus.Update{Applied: u.o.To, Requested: cmp.Or(prev.Requested, u.t.Ref), Status: Updated}
	case Refused:
		prev.Status, prev.Reason = Refused, u.o.Reason
		return prev
	default:
		prev.Status, prev.Reason, prev.Failed = u.o.Status, u.o.Reason, u.o.To
		return prev
	}
}

// writeTimeout bounds one claim or config write, its retries and its operation wait.
const writeTimeout = time.Minute

// write changes the instance's own config under its ETag, retrying as ic-healthd writes meanwhile.
func (u *updater) write(ctx context.Context, change func(map[string]string) error) error {
	ctx, cancel := context.WithTimeout(ctx, writeTimeout)
	defer cancel()
	var err error
	for range 10 {
		var inst *api.Instance
		var etag string
		if inst, etag, err = u.srv.GetInstance(u.t.Instance); err != nil {
			return err
		}
		put := inst.Writable()
		if put.Config == nil {
			put.Config = map[string]string{}
		}
		if err := change(put.Config); err != nil {
			return err
		}
		var op incusclient.Operation
		if op, err = u.srv.UpdateInstance(u.t.Instance, put, etag); err == nil {
			err = op.WaitContext(ctx)
		}
		if err == nil || !api.StatusErrorCheck(err, http.StatusPreconditionFailed) && !incus.IsBusy(err) {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(u.opts.Every):
		}
	}
	return err
}

func (u *updater) putClaim(cfg map[string]string) error {
	b, err := json.Marshal(u.c)
	cfg[incus.ClaimKey] = string(b)
	return err
}

// ours fails unless the stored claim is still this process's.
func (u *updater) ours(cfg map[string]string) error {
	var c claim
	if err := json.Unmarshal([]byte(cfg[incus.ClaimKey]), &c); err != nil || c.Owner != u.opts.Owner {
		return fmt.Errorf("%w by %s", errTaken, cmp.Or(c.Owner, "nobody"))
	}
	return nil
}

func (u *updater) acquire(ctx context.Context) error {
	u.c.Expires = u.opts.Now().Add(u.opts.Lease)
	return u.write(ctx, func(cfg map[string]string) error {
		if cfg[incus.ClaimKey] != "" {
			return errors.New("another process claimed it first")
		}
		return u.putClaim(cfg)
	})
}

func (u *updater) begin(ctx context.Context, i int) error {
	u.c.Step, u.c.Expires = i, u.opts.Now().Add(u.opts.Lease)
	return u.write(ctx, func(cfg map[string]string) error {
		if err := u.ours(cfg); err != nil {
			return err
		}
		return u.putClaim(cfg)
	})
}

// release drops the claim and records the outcome in the same write.
func (u *updater) release(ctx context.Context) error {
	return u.write(context.WithoutCancel(ctx), func(cfg map[string]string) error {
		if err := u.ours(cfg); err != nil {
			return err
		}
		delete(cfg, incus.ClaimKey)
		for k, v := range u.record().Keys() {
			if v == "" {
				delete(cfg, k)
			} else {
				cfg[k] = v
			}
		}
		return nil
	})
}

func (u *updater) refusal() string {
	cfg := u.inst.ExpandedConfig
	switch {
	case cfg["user.incus-compose.managed"] == "true":
		return "managed by incus-compose, whose instances are not updated yet"
	case isBeacon(cfg, u.o.To):
		return "an incus-beacon instance; use incus-beacon upgrade"
	case u.o.To == u.t.Update.Failed:
		return "the update to " + u.o.To + " failed before; waiting for a newer image"
	case u.inst.StatusCode != api.Running:
		return "the instance is " + strings.ToLower(u.inst.Status) + "; only a running one is updated, so its health can be checked"
	case u.inst.Config["volatile.base_image"] == "":
		return "no base image recorded to roll back to"
	}
	snaps, err := u.srv.GetInstanceSnapshotNames(u.t.Instance)
	if err != nil {
		return "list snapshots: " + err.Error()
	}
	if len(snaps) > 0 {
		return "the instance has snapshots (" + strings.Join(snaps, ", ") + "), which rebuild refuses"
	}
	srv, _, err := u.srv.GetServer()
	if err != nil {
		return "read the server version: " + err.Error()
	}
	if v := srv.Environment.ServerVersion; !atLeast(v, 7, 6) {
		return "Incus " + v + " keeps the old image's environment through a rebuild; 7.6 or later is needed"
	}
	return ""
}

func isBeacon(cfg map[string]string, to string) bool {
	for k := range cfg {
		if strings.HasPrefix(k, "environment.BEACON_") {
			return true
		}
	}
	ref, err := registry.ParseRef(to)
	return err == nil && path.Base(ref.Repository) == "incus-beacon"
}

// atLeast compares the major and minor parts of an Incus version such as 7.6 or 7.6.1.
func atLeast(v string, major, minor int) bool {
	parts := strings.SplitN(v, ".", 3)
	if len(parts) < 2 {
		return false
	}
	digits := strings.IndexFunc(parts[1], func(r rune) bool { return r < '0' || r > '9' })
	if digits < 0 {
		digits = len(parts[1])
	}
	ma, err := strconv.Atoi(parts[0])
	mi, err2 := strconv.Atoi(parts[1][:digits])
	if err != nil || err2 != nil {
		return false
	}
	return ma > major || ma == major && mi >= minor
}

// staleEnv picks the local environment keys that hold a value of the old or the new image's own.
func (u *updater) staleEnv(ctx context.Context) (map[string]string, error) {
	local := map[string]string{}
	for k, v := range u.inst.Config {
		if name, ok := strings.CutPrefix(k, "environment."); ok {
			local[name] = v
		}
	}
	if len(local) == 0 {
		return nil, nil
	}
	platform, err := registry.PlatformFor(u.t.Arch)
	if err != nil {
		return nil, err
	}
	own := map[string]bool{}
	oldRead := false
	read := func(s string, mustMatch bool) error {
		ref, err := registry.ParseRef(s)
		if err != nil {
			return err
		}
		img, err := u.reg.Resolve(ctx, ref, platform)
		if err != nil {
			return err
		}
		// A moved tag no longer names the old image, so its environment says nothing.
		if mustMatch && img.Fingerprint != u.t.BaseImage {
			return nil
		}
		env, err := u.reg.Env(ctx, ref, img)
		for _, kv := range env {
			own[kv] = true
		}
		oldRead = oldRead || mustMatch && err == nil
		return err
	}
	if err := read(u.o.To, false); err != nil {
		return nil, err
	}
	if u.t.ImageRef != "" {
		if err := read(u.t.ImageRef, true); err != nil {
			return nil, err
		}
	}
	stale := map[string]string{}
	for name, v := range local {
		if own[name+"="+v] {
			stale["environment."+name] = v
			u.o.Dropped = append(u.o.Dropped, "environment."+name)
		} else {
			u.o.Kept = append(u.o.Kept, "environment."+name)
		}
	}
	slices.Sort(u.o.Dropped)
	slices.Sort(u.o.Kept)
	u.o.OldEnvUnread = !oldRead
	if !oldRead && len(u.o.Kept) > 0 {
		u.log.Warn().Strs("kept", u.o.Kept).Msg("the old image can't be read, so its copied environment keys can't be told from overrides; kept them")
	}
	return stale, nil
}

func (u *updater) setEnv(ctx context.Context, add, drop map[string]string) error {
	if len(add) == 0 && len(drop) == 0 {
		return nil
	}
	return u.write(ctx, func(cfg map[string]string) error {
		for k := range drop {
			delete(cfg, k)
		}
		maps.Copy(cfg, add)
		return nil
	})
}

func (u *updater) volumes() ([]volume, error) {
	var vols []volume
	for _, name := range slices.Sorted(maps.Keys(u.inst.ExpandedDevices)) {
		d := u.inst.ExpandedDevices[name]
		src := strings.TrimPrefix(d["source"], "custom/")
		if d["type"] != "disk" || d["pool"] == "" || src == "" || strings.Contains(src, "/") {
			continue
		}
		vol, _, err := u.srv.GetStoragePoolVolume(d["pool"], "custom", src)
		if err != nil {
			return nil, fmt.Errorf("read volume %s/%s: %w", d["pool"], src, err)
		}
		vols = append(vols, volume{pool: d["pool"], name: src, shared: len(vol.UsedBy) > 1})
	}
	return vols, nil
}

func (u *updater) state() (api.StatusCode, error) {
	inst, _, err := u.srv.GetInstance(u.t.Instance)
	if err != nil {
		return 0, err
	}
	return inst.StatusCode, nil
}

func (u *updater) stop(ctx context.Context) error {
	if code, err := u.state(); err != nil || code != api.Stopped {
		if err := Stop(ctx, u.srv, u.t.Instance, u.opts.StopTimeout); err != nil {
			return err
		}
	}
	return u.stopSeen(ctx)
}

// stopSeen waits for ic-healthd's "stopped", so no later status is a leftover from before.
func (u *updater) stopSeen(ctx context.Context) error {
	if u.inst.ExpandedConfig[healthEnabled] != "true" {
		return nil
	}
	err := Poll(ctx, u.opts.StopTimeout, u.opts.Every, func(context.Context) error {
		inst, _, err := u.srv.GetInstance(u.t.Instance)
		if err == nil && inst.Config[healthStatus] != "stopped" {
			err = errors.New("status " + cmp.Or(inst.Config[healthStatus], "unset"))
		}
		return err
	})
	if err != nil {
		return fmt.Errorf("the health check did not report the stop; is ic-healthd running? %w", err)
	}
	return nil
}

func (u *updater) start(ctx context.Context) error {
	if err := SetState(ctx, u.srv, u.t.Instance, "start"); err != nil {
		return err
	}
	return u.gate(ctx)
}

// startOld undoes the stop: by then the instance is on the old image and volumes again.
func (u *updater) startOld(ctx context.Context) error {
	if code, err := u.state(); err == nil && code == api.Running {
		return nil
	}
	return u.start(ctx)
}

func (u *updater) rebuild(ctx context.Context) error {
	server, alias, err := SplitImage(u.c.To)
	if err != nil {
		return err
	}
	return Rebuild(ctx, u.srv, u.t.Instance, api.InstanceSource{Type: "image", Protocol: "oci", Server: server, Alias: alias})
}

// rebuildOld undoes the rebuild from the old fingerprint, which Incus still caches.
func (u *updater) rebuildOld(ctx context.Context) error {
	if err := u.stop(ctx); err != nil {
		return err
	}
	return Rebuild(ctx, u.srv, u.t.Instance, api.InstanceSource{Type: "image", Fingerprint: u.c.From})
}

func (u *updater) snapshot(ctx context.Context) error {
	for _, v := range u.vols {
		op, err := u.srv.CreateStoragePoolVolumeSnapshot(v.pool, "custom", v.name, api.StorageVolumeSnapshotsPost{Name: u.c.Snapshot})
		if err == nil {
			err = op.WaitContext(ctx)
		}
		if err != nil {
			return fmt.Errorf("%s/%s: %w", v.pool, v.name, err)
		}
		u.o.Snapshots = append(u.o.Snapshots, v.pool+"/"+v.name+"/"+u.c.Snapshot)
	}
	return nil
}

// restore undoes the start: the new image may have changed the volumes.
func (u *updater) restore(ctx context.Context) error {
	if err := u.stop(ctx); err != nil {
		return err
	}
	var errs []error
	for _, v := range u.vols {
		if v.shared {
			u.o.Unrestored = append(u.o.Unrestored, v.pool+"/"+v.name)
			continue
		}
		vol, etag, err := u.srv.GetStoragePoolVolume(v.pool, "custom", v.name)
		if err == nil {
			put := vol.Writable()
			put.Restore = u.c.Snapshot
			err = u.srv.UpdateStoragePoolVolume(v.pool, "custom", v.name, put, etag)
		}
		if err != nil {
			errs = append(errs, fmt.Errorf("%s/%s: %w", v.pool, v.name, err))
		}
	}
	return errors.Join(errs...)
}

// prune keeps the newest Keep of beacon's snapshots per volume; a failure only logs.
func (u *updater) prune() {
	if u.opts.Keep <= 0 {
		return
	}
	for _, v := range u.vols {
		names, err := u.srv.GetStoragePoolVolumeSnapshotNames(v.pool, "custom", v.name)
		if err != nil {
			u.log.Warn().Err(err).Str("volume", v.pool+"/"+v.name).Msg("snapshots not pruned")
			continue
		}
		var ours []string
		for _, n := range names {
			if strings.HasPrefix(n, snapshotPrefix) {
				ours = append(ours, n)
			}
		}
		slices.Sort(ours)
		for _, n := range ours[:max(len(ours)-u.opts.Keep, 0)] {
			op, err := u.srv.DeleteStoragePoolVolumeSnapshot(v.pool, "custom", v.name, n)
			if err == nil {
				err = op.Wait()
			}
			if err != nil {
				u.log.Warn().Err(err).Str("snapshot", v.pool+"/"+v.name+"/"+n).Msg("snapshot not pruned")
			}
		}
	}
}

// gate wants Settle of running and, with a health check, healthy; a stop or unhealthy fails at once.
func (u *updater) gate(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, u.opts.Wait)
	defer cancel()
	settled := u.opts.Now().Add(u.opts.Settle)
	health := u.inst.ExpandedConfig[healthEnabled] == "true"
	tick := time.NewTicker(u.opts.Every)
	defer tick.Stop()
	last := "not yet settled"
	for {
		inst, _, err := u.srv.GetInstance(u.t.Instance)
		switch {
		case err != nil:
			last = err.Error()
		case inst.StatusCode != api.Running:
			return fmt.Errorf("the instance is %s after the start", strings.ToLower(inst.Status))
		case health && inst.Config[healthStatus] == "unhealthy":
			return errors.New("the health check reports unhealthy")
		case u.opts.Now().Before(settled):
		case !health || inst.Config[healthStatus] == "healthy":
			return nil
		default:
			last = "health check status " + cmp.Or(inst.Config[healthStatus], "unset")
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("not healthy within %s: %s", u.opts.Wait, last)
		case <-tick.C:
		}
	}
}
