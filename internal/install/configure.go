package install

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"slices"
	"strings"
	"time"

	incusclient "github.com/lxc/incus/v7/client"
	"github.com/lxc/incus/v7/shared/api"
	"github.com/rs/zerolog"

	"github.com/tallica/incus-beacon/internal/config"
	"github.com/tallica/incus-beacon/internal/incus"
	"github.com/tallica/incus-beacon/internal/log"
	"github.com/tallica/incus-beacon/internal/policy"
	"github.com/tallica/incus-beacon/internal/update"
)

// ConfigureOptions changes only non-nil settings; an empty or zero one does what install does without the flag.
type ConfigureOptions struct {
	Name         string
	APIURL       *string
	Projects     *[]string
	Watch        *string
	Events       *bool
	Track        *string
	NotifyURLs   *[]string
	NotifyErrs   *bool
	Interval     *time.Duration
	Cooldown     *time.Duration
	Update       *bool
	UpdateWindow *string
	Wait         time.Duration
	Log          zerolog.Logger
	Audit        zerolog.Logger // as for Options; a no-op configure writes none
}

const envPrefix = "environment.BEACON_"

// change is what Configure was asked to change, with what it takes to undo it.
type change struct {
	name, pool, vol                   string
	cfg, before                       map[string]string
	oldURLs, newURLs                  []string
	cert                              *api.Certificate
	oldProjects, newProjects          []string
	cfgChange, urlsChange, certChange bool
}

// planChange works out the new settings, refusing any serve would, and which of the three places they live change.
func planChange(srv incusclient.InstanceServer, inst *api.Instance, opts ConfigureOptions) (*change, error) {
	c := &change{name: opts.Name, before: maps.Clone(inst.Config)}
	var err error
	if c.cfg, err = configured(srv, inst, opts); err != nil {
		return nil, err
	}
	if err := config.CheckServeEnv(serveEnv(c.cfg)); err != nil {
		return nil, fmt.Errorf("the instance's serve would refuse these settings: %w", err)
	}

	c.pool, c.vol = inst.Devices[stateDev]["pool"], VolumeName(c.name)
	if opts.NotifyURLs != nil {
		if c.oldURLs, err = readNotifyURLs(srv, c.pool, c.vol); err != nil {
			return nil, err
		}
		c.newURLs = *opts.NotifyURLs
	}
	c.urlsChange = opts.NotifyURLs != nil && !slices.Equal(*opts.NotifyURLs, c.oldURLs)
	if opts.Projects != nil {
		if c.cert, err = certificate(srv, c.name); err != nil {
			return nil, err
		}
		c.oldProjects, c.newProjects = c.cert.Projects, *opts.Projects
	}
	c.certChange = c.cert != nil && restriction(*opts.Projects) != restriction(c.cert.Projects)
	c.cfgChange = !maps.Equal(c.cfg, c.before)
	return c, nil
}

func (c *change) changes() bool { return c.cfgChange || c.urlsChange || c.certChange }

// apply writes the new settings, or with forward false the old ones back.
func (c *change) apply(ctx context.Context, srv incusclient.InstanceServer, forward bool) error {
	from, to, urls, projects := c.cfg, c.before, c.oldURLs, c.oldProjects
	if forward {
		from, to, urls, projects = c.before, c.cfg, c.newURLs, c.newProjects
	}
	var errs []error
	if c.certChange {
		errs = append(errs, restrict(srv, c.cert.Fingerprint, projects))
	}
	if c.urlsChange {
		errs = append(errs, setNotifyURLs(srv, c.pool, c.vol, urls))
	}
	if c.cfgChange {
		errs = append(errs, rewriteKeys(ctx, srv, c.name, from, to))
	}
	return errors.Join(errs...)
}

// Configure follows docs/ARCHITECTURE.md#connecting-to-incus.
func Configure(ctx context.Context, srv incusclient.InstanceServer, opts ConfigureOptions) (err error) {
	srv, inst, err := installed(srv, opts.Name, opts.Log)
	if err != nil {
		return err
	}
	c, err := planChange(srv, inst, opts)
	if err != nil {
		return err
	}
	name := opts.Name
	if !c.changes() {
		opts.Log.Info().Str("instance", name).Msg("settings already as given; nothing changed")
		return nil
	}
	audit := log.AuditFor(opts.Audit, log.Process(), inst.Project, name).
		With().
		Strs("settings", changed(c.before, c.cfg, c.urlsChange, c.certChange)).
		Logger()
	log.AuditBegin(audit, "configure").Send()
	defer func() { log.AuditEnd(audit, "configure", err).Send() }()
	running := inst.StatusCode != api.Stopped
	// A track change alone needs no restart: serve reads policy keys every cycle.
	restart := running && (c.urlsChange || !maps.Equal(envOf(c.cfg), envOf(c.before)))

	// Stopped while it changes, serve never runs with the certificate and its environment disagreeing.
	if restart {
		if err := update.SetState(ctx, srv, name, "stop"); err != nil {
			return fmt.Errorf("stop %s: %w", name, err)
		}
	}
	if err := c.apply(ctx, srv, true); err != nil {
		rerr := rollBack(audit, "configure.rollback", func() error {
			rerr := c.apply(ctx, srv, false)
			if restart {
				rerr = errors.Join(rerr, update.SetState(ctx, srv, name, "start"))
			}
			return rerr
		})
		return errors.Join(fmt.Errorf("configure %s: %w", name, err), rerr)
	}
	opts.Log.Info().Str("instance", name).Msg("settings changed")
	switch {
	case !running:
		opts.Log.Warn().
			Str("instance", name).
			Msg("stopped, so not checked beyond serve's flag parsing; the settings apply when it starts")
		return nil
	case !restart:
		return nil
	}
	return restartReady(ctx, srv, opts, audit, c)
}

// restartReady starts the instance and waits for /ready, rolling back to the old settings if it never gets there.
func restartReady(
	ctx context.Context,
	srv incusclient.InstanceServer,
	opts ConfigureOptions,
	audit zerolog.Logger,
	c *change,
) error {
	name := opts.Name
	err := startReady(ctx, srv, name, opts.Wait)
	if err == nil {
		opts.Log.Info().Str("instance", name).Msg("restarted and ready")
		return nil
	}
	err = fmt.Errorf("configure %s: %w", name, err)
	opts.Log.Warn().Err(err).Msg("rolling back")
	// The old settings come back running, as the instance was before configure, whether the start or the probe failed.
	rerr := rollBack(audit, "configure.rollback", func() error {
		if err := stopIfRunning(ctx, srv, name); err != nil {
			return err
		}
		if err := c.apply(ctx, srv, false); err != nil {
			return err
		}
		return startReady(ctx, srv, name, opts.Wait)
	})
	if rerr != nil {
		return errors.Join(err, fmt.Errorf("roll back: %w", rerr))
	}
	return fmt.Errorf("%w; rolled back to the old settings, which are ready", err)
}

// settingKeys maps each configure flag to the instance key it sets.
var settingKeys = []struct{ flag, key string }{
	{"api-url", envPrefix + "INCUS"},
	{"project", envPrefix + "PROJECT"},
	{"watch", envPrefix + "WATCH"},
	{"events", envPrefix + "EVENTS"},
	{"track", policy.Prefix + "track"},
	{"notify-url", envPrefix + "NOTIFY_URL_FILE"},
	{"notify-errors", envPrefix + "NOTIFY_ERRORS"},
	{"interval", envPrefix + "INTERVAL"},
	{"cooldown", envPrefix + "COOLDOWN"},
	{"update", envPrefix + "UPDATE"},
	{"update-window", envPrefix + "UPDATE_WINDOW"},
}

// changed names the settings that differ, never their values: notify URLs hold tokens.
func changed(before, after map[string]string, urls, cert bool) []string {
	var names []string
	for _, s := range settingKeys {
		if before[s.key] != after[s.key] || s.flag == "notify-url" && urls || s.flag == "project" && cert {
			names = append(names, s.flag)
		}
	}
	return names
}

func configured(srv incusclient.InstanceServer, inst *api.Instance, opts ConfigureOptions) (map[string]string, error) {
	cfg := maps.Clone(inst.Config)
	set := func(key, v string) {
		if v == "" {
			delete(cfg, key)
		} else {
			cfg[key] = v
		}
	}
	env := func(name, v string) { set(envPrefix+name, v) }
	onOff := func(name string, b *bool) {
		if b != nil {
			env(name, when(*b, "true"))
		}
	}
	duration := func(name string, d *time.Duration) {
		if d != nil {
			env(name, when(*d > 0, d.String()))
		}
	}

	if opts.APIURL != nil {
		url := *opts.APIURL
		if url == "" {
			_, network, err := profileDefaults(srv)
			if err != nil {
				return nil, err
			}
			if url, err = detectURL(srv, network); err != nil {
				return nil, err
			}
		}
		env("INCUS", url)
	}
	if opts.Projects != nil {
		for _, p := range *opts.Projects {
			if _, _, err := srv.GetProject(p); err != nil {
				return nil, fmt.Errorf("project %s: %w", p, err)
			}
		}
		env("PROJECT", strings.Join(*opts.Projects, ","))
	}
	if opts.Watch != nil {
		env("WATCH", when(*opts.Watch != config.WatchAll, *opts.Watch))
	}
	if opts.Track != nil {
		track := *opts.Track
		if track == "" {
			track = trackFor("", inst.Config["image.id"])
		}
		set(policy.Prefix+"track", track)
	}
	if opts.NotifyURLs != nil {
		env("NOTIFY_URL_FILE", when(len(*opts.NotifyURLs) > 0, stateDir+"/"+notifyFile))
	}
	if opts.UpdateWindow != nil {
		env("UPDATE_WINDOW", *opts.UpdateWindow)
	}
	onOff("EVENTS", opts.Events)
	onOff("NOTIFY_ERRORS", opts.NotifyErrs)
	onOff("UPDATE", opts.Update)
	duration("INTERVAL", opts.Interval)
	duration("COOLDOWN", opts.Cooldown)
	return cfg, nil
}

// when is v if cond holds, else empty, which leaves the key unset.
func when(cond bool, v string) string {
	if cond {
		return v
	}
	return ""
}

func envOf(cfg map[string]string) map[string]string {
	env := map[string]string{}
	for k, v := range cfg {
		if name, ok := strings.CutPrefix(k, envPrefix); ok {
			env["BEACON_"+name] = v
		}
	}
	return env
}

// serveEnv adds what the image's Containerfile sets.
func serveEnv(cfg map[string]string) map[string]string {
	env := envOf(cfg)
	if env["BEACON_STATE_DIR"] == "" {
		env["BEACON_STATE_DIR"] = stateDir
	}
	return env
}

// rewriteKeys writes only the keys from and to differ in, so keys someone else changed meanwhile stay.
func rewriteKeys(ctx context.Context, srv incusclient.InstanceServer, name string, from, to map[string]string) error {
	err := incus.UpdateInstance(ctx, srv, name, func(put *api.InstancePut) error {
		put.Config = maps.Clone(put.Config)
		for k := range keysOf(from, to) {
			if v, ok := to[k]; ok {
				put.Config[k] = v
			} else {
				delete(put.Config, k)
			}
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("update %s: %w", name, err)
	}
	return nil
}

func keysOf(a, b map[string]string) map[string]bool {
	keys := map[string]bool{}
	for k, v := range a {
		if w, ok := b[k]; !ok || w != v {
			keys[k] = true
		}
	}
	for k := range b {
		if _, ok := a[k]; !ok {
			keys[k] = true
		}
	}
	return keys
}

func startReady(ctx context.Context, srv incusclient.InstanceServer, name string, wait time.Duration) error {
	if err := update.SetState(ctx, srv, name, "start"); err != nil {
		return fmt.Errorf("start %s: %w", name, err)
	}
	return waitReady(ctx, srv, name, wait)
}

// certificate is the one install trusted under the instance's name.
func certificate(srv incusclient.InstanceServer, name string) (*api.Certificate, error) {
	certs, err := srv.GetCertificates()
	if err != nil {
		return nil, err
	}
	i := slices.IndexFunc(certs, func(c api.Certificate) bool { return c.Name == name })
	if i < 0 {
		return nil, fmt.Errorf("no trusted certificate is named %s, so its projects can't change", name)
	}
	c := certs[i] // kept for the rollback, so not shared with what the server returned
	c.Projects = slices.Clone(c.Projects)
	return &c, nil
}

// restriction compares project lists in any order; none means unrestricted.
func restriction(projects []string) string {
	p := slices.Clone(projects)
	slices.Sort(p)
	return strings.Join(p, ",")
}

func restrict(srv incusclient.InstanceServer, fp string, projects []string) error {
	c, etag, err := srv.GetCertificate(fp)
	if err != nil {
		return fmt.Errorf("read certificate %s: %w", fp, err)
	}
	put := c.Writable()
	put.Restricted, put.Projects = len(projects) > 0, projects
	if err := srv.UpdateCertificate(fp, put, etag); err != nil {
		return fmt.Errorf("restrict certificate %s: %w", c.Name, err)
	}
	return nil
}

// readNotifyURLs is nil when the volume has no URL file.
func readNotifyURLs(srv incusclient.InstanceServer, pool, vol string) ([]string, error) {
	c, err := srv.GetStoragePoolVolumeFileSFTP(pool, "custom", vol)
	if err != nil {
		return nil, fmt.Errorf("open volume %s: %w", vol, err)
	}
	defer c.Close()
	f, err := c.Open("/" + notifyFile)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("open %s in %s: %w", notifyFile, vol, err)
	}
	defer f.Close()
	var b strings.Builder
	if _, err := f.WriteTo(&b); err != nil {
		return nil, fmt.Errorf("read %s in %s: %w", notifyFile, vol, err)
	}
	return strings.Fields(b.String()), nil
}

// setNotifyURLs removes the old file first, since install leaves it read-only.
func setNotifyURLs(srv incusclient.InstanceServer, pool, vol string, urls []string) error {
	c, err := srv.GetStoragePoolVolumeFileSFTP(pool, "custom", vol)
	if err != nil {
		return fmt.Errorf("open volume %s: %w", vol, err)
	}
	err = c.Remove("/" + notifyFile)
	c.Close()
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove %s in %s: %w", notifyFile, vol, err)
	}
	if len(urls) == 0 {
		return nil
	}
	return writeNotifyURLs(srv, pool, vol, urls)
}
