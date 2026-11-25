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

// Configure follows docs/ARCHITECTURE.md#connecting-to-incus.
func Configure(ctx context.Context, srv incusclient.InstanceServer, opts ConfigureOptions) (err error) {
	srv, inst, err := installed(srv, opts.Name, opts.Log)
	if err != nil {
		return err
	}
	name, before := opts.Name, maps.Clone(inst.Config)
	cfg, err := configured(srv, inst, opts)
	if err != nil {
		return err
	}
	if err := config.CheckServeEnv(serveEnv(cfg)); err != nil {
		return fmt.Errorf("the instance's serve would refuse these settings: %w", err)
	}

	pool, vol := inst.Devices[stateDev]["pool"], VolumeName(name)
	var oldURLs []string
	if opts.NotifyURLs != nil {
		if oldURLs, err = readNotifyURLs(srv, pool, vol); err != nil {
			return err
		}
	}
	urlsChange := opts.NotifyURLs != nil && !slices.Equal(*opts.NotifyURLs, oldURLs)
	var cert *api.Certificate
	if opts.Projects != nil {
		if cert, err = certificate(srv, name); err != nil {
			return err
		}
	}
	certChange := cert != nil && restriction(*opts.Projects) != restriction(cert.Projects)
	cfgChange := !maps.Equal(cfg, before)
	if !cfgChange && !urlsChange && !certChange {
		opts.Log.Info().Str("instance", name).Msg("settings already as given; nothing changed")
		return nil
	}
	audit := log.AuditFor(opts.Audit, log.Process(), inst.Project, name).With().Strs("settings", changed(before, cfg, urlsChange, certChange)).Logger()
	log.AuditBegin(audit, "configure").Send()
	defer func() { log.AuditEnd(audit, "configure", err).Send() }()
	running := inst.StatusCode != api.Stopped
	// A track change alone needs no restart: serve reads policy keys every cycle.
	restart := running && (urlsChange || !maps.Equal(envOf(cfg), envOf(before)))

	apply := func(from, to map[string]string, urls, projects []string) error {
		var errs []error
		if certChange {
			errs = append(errs, restrict(srv, cert.Fingerprint, projects))
		}
		if urlsChange {
			errs = append(errs, setNotifyURLs(srv, pool, vol, urls))
		}
		if cfgChange {
			errs = append(errs, rewriteKeys(ctx, srv, name, from, to))
		}
		return errors.Join(errs...)
	}
	var newURLs, oldProjects, newProjects []string
	if opts.NotifyURLs != nil {
		newURLs = *opts.NotifyURLs
	}
	if cert != nil {
		oldProjects, newProjects = cert.Projects, *opts.Projects
	}
	undo := func() error { return apply(cfg, before, oldURLs, oldProjects) }

	// Stopped while it changes, serve never runs with the certificate and its environment disagreeing.
	if restart {
		if err := update.SetState(ctx, srv, name, "stop"); err != nil {
			return fmt.Errorf("stop %s: %w", name, err)
		}
	}
	if err := apply(before, cfg, newURLs, newProjects); err != nil {
		log.AuditBegin(audit, "configure.rollback").Send()
		rerr := undo()
		if restart {
			rerr = errors.Join(rerr, update.SetState(ctx, srv, name, "start"))
		}
		log.AuditEnd(audit, "configure.rollback", rerr).Send()
		return errors.Join(fmt.Errorf("configure %s: %w", name, err), rerr)
	}
	opts.Log.Info().Str("instance", name).Msg("settings changed")
	switch {
	case !running:
		opts.Log.Warn().Str("instance", name).Msg("stopped, so not checked beyond serve's flag parsing; the settings apply when it starts")
		return nil
	case !restart:
		return nil
	}

	err = startReady(ctx, srv, name, opts.Wait)
	if err == nil {
		opts.Log.Info().Str("instance", name).Msg("restarted and ready")
		return nil
	}
	err = fmt.Errorf("configure %s: %w", name, err)
	opts.Log.Warn().Err(err).Msg("rolling back")
	log.AuditBegin(audit, "configure.rollback").Send()
	// The old settings come back running, as the instance was before configure, whether the start or the probe failed.
	rerr := stopIfRunning(ctx, srv, name)
	if rerr == nil {
		rerr = undo()
	}
	if rerr == nil {
		rerr = startReady(ctx, srv, name, opts.Wait)
	}
	log.AuditEnd(audit, "configure.rollback", rerr).Send()
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
