// Package install creates and removes a serve instance that joins Incus with a trust token.
package install

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"regexp"
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
	"github.com/tallica/incus-beacon/internal/registry"
	"github.com/tallica/incus-beacon/internal/semver"
	"github.com/tallica/incus-beacon/internal/update"
)

const (
	stateDir   = "/var/lib/incus-beacon"
	secretsDir = "/run/secrets"
	tokenFile  = secretsDir + "/token"
	stateDev   = "state"
	logDir     = "/var/log/incus-beacon"
	logDev     = "log"
	auditFile  = logDir + "/audit.log"
	notifyFile = "notify-urls"
	imageUID   = 65534
	imageRepo  = "ghcr.io/tallica/incus-beacon"
)

type Options struct {
	Name        string
	Image       string
	StoragePool string
	Projects    []string
	Watch       string
	// Settings are serve's for the instance; a zero value leaves serve's default.
	config.Settings
	Wait  time.Duration
	Log   zerolog.Logger
	Audit zerolog.Logger // gets begin and done or failed; the zero Logger writes none
}

// DefaultName is what install calls the instance; legacyName is what it did before 0.6.
const (
	DefaultName = "incus-beacond"
	legacyName  = "incus-beacon"
)

// ResolveName is name, else the default, else an instance from before 0.6 under the old name.
func ResolveName(srv incusclient.InstanceServer, name string, logger zerolog.Logger) string {
	if name != "" {
		return name
	}
	if _, _, err := findInstance(srv, DefaultName, logger); notFound(err) {
		if _, _, err := findInstance(srv, legacyName, logger); err == nil {
			logger.Info().Str("instance", legacyName).Msg("using the instance from before 0.6")
			return legacyName
		}
	}
	return DefaultName
}

// VolumeName is the state volume of the instance called name.
func VolumeName(name string) string { return name + "-state" }

// LogVolumeName is the volume holding the audit log of the instance called name.
func LogVolumeName(name string) string { return name + "-log" }

var release = regexp.MustCompile(`^v?(\d+)\.(\d+)\.\d+$`)

// DefaultImage follows the installer's major.minor; a dev build gets latest.
func DefaultImage(version string) (image string, released bool) {
	if m := release.FindStringSubmatch(version); m != nil {
		return imageRepo + ":" + m[1] + "." + m[2], true
	}
	return imageRepo + ":latest", false
}

// trackFor picks semver only for a version tag: latest and digests have none to compare.
func trackFor(track, image string) string {
	if track != "" {
		return track
	}
	if ref, err := registry.ParseRef(image); err == nil {
		if _, ok := semver.Parse(ref.Tag); ok {
			return policy.TrackSemver
		}
	}
	return policy.TrackDigest
}

// rollBack runs undo between the op's audit begin and end events.
func rollBack(audit zerolog.Logger, op string, undo func() error) error {
	log.AuditBegin(audit, op).Send()
	rerr := undo()
	log.AuditEnd(audit, op, rerr).Send()
	return rerr
}

// Install undoes what it created if a step before the start fails.
func Install(ctx context.Context, srv incusclient.InstanceServer, opts Options) (err error) {
	if opts.Name == "" {
		if _, _, err := findInstance(srv, legacyName, opts.Log); err == nil {
			return fmt.Errorf(
				"an instance %s from before 0.6 exists; upgrade it with incus-beacon upgrade, or give --name to install another",
				legacyName,
			)
		}
		opts.Name = DefaultName
	}
	if err := checkFree(srv, opts.Name); err != nil {
		return err
	}
	pool, network, err := profileDefaults(srv)
	if err != nil {
		return err
	}
	if opts.StoragePool != "" {
		pool = opts.StoragePool
	}
	if pool == "" {
		return errors.New("the default profile has no root disk; give --storage-pool")
	}
	apiURL := opts.APIURL
	if apiURL == "" {
		if apiURL, err = detectURL(srv, network); err != nil {
			return err
		}
	}
	// configured also refuses a missing project: Incus accepts a token for one, and the instance then never joins.
	s, track := opts.Settings, trackFor(opts.Track, opts.Image)
	s.NotifyErrs = s.NotifyErrs && len(s.NotifyURLs) > 0 // with nothing to notify, serve would refuse the flag
	fresh := &api.Instance{Config: map[string]string{}}
	env, err := configured(srv, fresh, ConfigureOptions{
		APIURL:       &apiURL,
		Projects:     &opts.Projects,
		Watch:        &opts.Watch,
		Events:       &s.Events,
		Track:        &track,
		NotifyURLs:   &s.NotifyURLs,
		NotifyErrs:   &s.NotifyErrs,
		Interval:     &s.Interval,
		Cooldown:     &s.Cooldown,
		Update:       &s.Update,
		UpdateWindow: &s.UpdateWindow,
	})
	if err != nil {
		return err
	}
	env[envPrefix+"TOKEN_FILE"] = tokenFile
	env[envPrefix+"AUDIT_LOG"] = auditFile
	server, alias, err := update.SplitImage(opts.Image)
	if err != nil {
		return err
	}

	audit := log.AuditFor(opts.Audit, log.Process(), "", opts.Name).
		With().
		Str("image", opts.Image).
		Str("pool", pool).
		Str("volume", VolumeName(opts.Name)).
		Str("log_volume", LogVolumeName(opts.Name)).
		Logger()
	log.AuditBegin(audit, "install").Send()
	defer func() { log.AuditEnd(audit, "install", err).Send() }()

	var undo []func() error
	rollback := func(err error) error {
		log.AuditBegin(audit, "install.rollback").Send()
		var rerr error
		for _, f := range slices.Backward(undo) {
			rerr = errors.Join(rerr, f())
		}
		log.AuditEnd(audit, "install.rollback", rerr).Send()
		return errors.Join(err, rerr)
	}

	vol := VolumeName(opts.Name)
	if err := createVolume(srv, pool, vol); err != nil {
		return rollback(err)
	}
	undo = append(undo, func() error { return srv.DeleteStoragePoolVolume(pool, "custom", vol) })
	opts.Log.Info().Str("pool", pool).Str("volume", vol).Msg("volume created")

	logVol := LogVolumeName(opts.Name)
	created, err := ensureLogVolume(srv, pool, logVol, opts.Log)
	if err != nil {
		return rollback(err)
	}
	if created {
		undo = append(undo, func() error { return srv.DeleteStoragePoolVolume(pool, "custom", logVol) })
	}

	if len(opts.NotifyURLs) > 0 {
		if err := writeNotifyURLs(srv, pool, vol, opts.NotifyURLs); err != nil {
			return rollback(err)
		}
		opts.Log.Info().
			Int("urls", len(opts.NotifyURLs)).
			Str("file", vol+"/"+notifyFile).
			Msg("notification URLs stored")
	}

	token, opID, err := createToken(srv, opts.Name, opts.Projects)
	if err != nil {
		return rollback(err)
	}
	undo = append(undo, func() error { return tokens(srv).DeleteOperation(opID) })

	op, err := srv.CreateInstance(api.InstancesPost{
		Name: opts.Name,
		Type: api.InstanceTypeContainer,
		Source: api.InstanceSource{
			Type: "image", Protocol: "oci", Server: server, Alias: alias,
		},
		Config: env,
		Devices: map[string]map[string]string{
			stateDev: disk(pool, vol, stateDir),
			logDev:   disk(pool, logVol, logDir),
		},
	})
	if err == nil {
		err = op.WaitContext(ctx)
	}
	if err != nil {
		return rollback(fmt.Errorf("create instance %s from %s: %w", opts.Name, opts.Image, err))
	}
	// A rollback finishes even when the caller gives up: no undo step stops on ctx.
	undo = append(undo, func() error { return deleteInstance(ctx, srv, opts.Name) })
	opts.Log.Info().Str("instance", opts.Name).Str("image", opts.Image).Str("incus", apiURL).Msg("instance created")

	if err := pushToken(srv, opts.Name, token); err != nil {
		return rollback(err)
	}
	if err := update.SetState(ctx, srv, opts.Name, "start"); err != nil {
		return rollback(fmt.Errorf("start %s: %w", opts.Name, err))
	}

	// Past here the instance stays: its log says why it didn't join.
	if err := waitTrusted(ctx, srv, opts.Name, opts.Wait); err != nil {
		return err
	}
	if err := srv.DeleteInstanceFile(opts.Name, tokenFile); err != nil {
		opts.Log.Warn().Err(err).Str("file", tokenFile).Msg("spent token not removed")
	}
	opts.Log.Info().Str("instance", opts.Name).Msg("joined Incus")
	return nil
}

type UninstallOptions struct {
	Name      string
	DeleteLog bool // also the log volume, which is otherwise kept for reading after the uninstall
	Log       zerolog.Logger
	Audit     zerolog.Logger // as for Options
}

// Uninstall tolerates any of its parts being gone already.
func Uninstall(ctx context.Context, srv incusclient.InstanceServer, opts UninstallOptions) (err error) {
	name, logger := opts.Name, opts.Log
	srv, inst, ferr := findInstance(srv, name, logger)
	project := ""
	if ferr == nil {
		project = inst.Project
	}
	audit := log.AuditFor(opts.Audit, log.Process(), project, name)
	log.AuditBegin(audit, "uninstall").Send()
	var removed []string
	kept := ""
	defer func() {
		e := log.AuditEnd(audit, "uninstall", err)
		if len(removed) > 0 {
			e = e.Strs("removed", removed)
		}
		if kept != "" {
			e = e.Str("kept_volume", kept)
		}
		e.Send()
	}()
	var errs []error
	pool := ""
	switch {
	case ferr == nil:
		pool = inst.Devices[stateDev]["pool"]
		if inst.StatusCode != api.Stopped {
			if err := update.SetState(ctx, srv, name, "stop"); err != nil {
				errs = append(errs, fmt.Errorf("stop %s: %w", name, err))
			}
		}
		if err := deleteInstance(ctx, srv, name); err != nil {
			errs = append(errs, fmt.Errorf("delete %s: %w", name, err))
		} else {
			removed = append(removed, "instance "+name)
			logger.Info().Str("instance", name).Msg("instance deleted")
		}
	case !notFound(ferr):
		errs = append(errs, ferr)
	}
	for _, vol := range []string{VolumeName(name), LogVolumeName(name)} {
		p := pool
		if p == "" {
			found, err := poolOf(srv, vol)
			if err != nil {
				errs = append(errs, err)
				continue
			}
			p = found
		}
		switch {
		case p == "": // in no pool: gone already
		case vol == LogVolumeName(name) && !opts.DeleteLog:
			if ok, err := volumeExists(srv, p, vol); err != nil {
				errs = append(errs, err)
			} else if ok {
				kept = vol
				logger.Info().
					Str("pool", p).
					Str("volume", vol).
					Msg("log volume kept; uninstall --delete-log removes it")
			}
		default:
			if deleted, err := deleteVolume(srv, p, vol); err != nil {
				errs = append(errs, err)
			} else if deleted {
				removed = append(removed, "volume "+vol)
				logger.Info().Str("pool", p).Str("volume", vol).Msg("volume deleted")
			}
		}
	}
	revoked, err := revokeTokens(srv, name, logger)
	for _, id := range revoked {
		removed = append(removed, "trust token "+id)
	}
	if err != nil {
		errs = append(errs, err)
	}
	certs, err := srv.GetCertificates()
	if err != nil {
		return errors.Join(append(errs, err)...)
	}
	for _, c := range certs {
		if c.Name != name {
			continue
		}
		if err := srv.DeleteCertificate(c.Fingerprint); err != nil {
			errs = append(errs, fmt.Errorf("remove certificate %s: %w", c.Fingerprint, err))
		} else {
			removed = append(removed, "certificate "+c.Fingerprint)
			logger.Info().Str("certificate", c.Name).Msg("certificate removed")
		}
	}
	return errors.Join(errs...)
}

func createVolume(srv incusclient.InstanceServer, pool, vol string) error {
	err := srv.CreateStoragePoolVolume(pool, api.StorageVolumesPost{
		Name: vol,
		Type: "custom",
		Config: map[string]string{
			"initial.uid": fmt.Sprint(imageUID),
			"initial.gid": fmt.Sprint(imageUID),
		},
	})
	if err != nil {
		return fmt.Errorf("create volume %s: %w", vol, err)
	}
	return nil
}

// ensureLogVolume reuses a log volume already there, records and all.
func ensureLogVolume(
	srv incusclient.InstanceServer,
	pool, vol string,
	logger zerolog.Logger,
) (created bool, err error) {
	err = createVolume(srv, pool, vol)
	switch {
	case err == nil:
		logger.Info().Str("pool", pool).Str("volume", vol).Msg("log volume created")
		return true, nil
	case api.StatusErrorCheck(err, http.StatusConflict): // Incus's only conflict here is the name being taken
		logger.Info().Str("pool", pool).Str("volume", vol).Msg("reusing the log volume")
		return false, nil
	}
	return false, err
}

func disk(pool, vol, path string) map[string]string {
	return map[string]string{"type": "disk", "pool": pool, "source": vol, "path": path}
}

func deleteVolume(srv incusclient.InstanceServer, pool, vol string) (deleted bool, err error) {
	err = srv.DeleteStoragePoolVolume(pool, "custom", vol)
	switch {
	case err == nil:
		return true, nil
	case notFound(err):
		return false, nil
	}
	return false, fmt.Errorf("delete volume %s: %w", vol, err)
}

func poolOf(srv incusclient.InstanceServer, vol string) (string, error) {
	pools, err := srv.GetStoragePoolNames()
	if err != nil {
		return "", fmt.Errorf("list storage pools: %w", err)
	}
	for _, p := range pools {
		if ok, err := volumeExists(srv, p, vol); err != nil || ok {
			return p, err
		}
	}
	return "", nil
}

func volumeExists(srv incusclient.InstanceServer, pool, vol string) (bool, error) {
	_, _, err := srv.GetStoragePoolVolume(pool, "custom", vol)
	switch {
	case err == nil:
		return true, nil
	case notFound(err):
		return false, nil
	}
	return false, fmt.Errorf("read volume %s: %w", vol, err)
}

// findInstance also searches other projects; the server it returns uses the instance's.
func findInstance(
	srv incusclient.InstanceServer,
	name string,
	logger zerolog.Logger,
) (incusclient.InstanceServer, *api.Instance, error) {
	inst, _, err := srv.GetInstance(name)
	if !notFound(err) {
		return srv, inst, err
	}
	p, perr := projectOf(srv, name)
	if perr != nil {
		return srv, nil, perr
	}
	if p == "" {
		return srv, nil, err
	}
	logger.Info().Str("instance", name).Str("project", p).Msg("found in another project")
	srv = srv.UseProject(p)
	inst, _, err = srv.GetInstance(name)
	return srv, inst, err
}

// projectOf finds the project of the instance called name, "" when there is none.
func projectOf(srv incusclient.InstanceServer, name string) (string, error) {
	insts, err := srv.GetInstancesAllProjects(api.InstanceTypeAny)
	if err != nil {
		return "", fmt.Errorf("list instances: %w", err)
	}
	for _, inst := range insts {
		if inst.Name == name {
			return inst.Project, nil
		}
	}
	return "", nil
}

// checkFree refuses a taken name: uninstall removes certificates by name.
func checkFree(srv incusclient.InstanceServer, name string) error {
	if _, _, err := srv.GetInstance(name); err == nil {
		return fmt.Errorf("instance %s already exists", name)
	} else if !notFound(err) {
		return err
	}
	certs, err := srv.GetCertificates()
	if err != nil {
		return err
	}
	if slices.ContainsFunc(certs, func(c api.Certificate) bool { return c.Name == name }) {
		return fmt.Errorf("a trusted certificate is already named %s; pick another --name", name)
	}
	return nil
}

// profileDefaults reads the root pool and NIC network an instance gets.
func profileDefaults(srv incusclient.InstanceServer) (pool, network string, err error) {
	p, _, err := srv.GetProfile("default")
	if err != nil {
		return "", "", fmt.Errorf("read the default profile: %w", err)
	}
	for _, d := range p.Devices {
		switch d["type"] {
		case "disk":
			if d["path"] == "/" {
				pool = d["pool"]
			}
		case "nic":
			if network == "" {
				network = d["network"]
			}
		}
	}
	return pool, network, nil
}

// detectURL falls back to the bridge address, which a wildcard listener answers on.
func detectURL(srv incusclient.InstanceServer, network string) (string, error) {
	info, _, err := srv.GetServer()
	if err != nil {
		return "", err
	}
	addr := info.Config["core.https_address"]
	if addr == "" {
		return "", errors.New("core.https_address is unset, so Incus has no network API; set it, or give --api-url")
	}
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		host, port = addr, "8443"
	}
	if host != "" && host != "0.0.0.0" && host != "::" {
		return "https://" + net.JoinHostPort(host, port), nil
	}
	if network == "" {
		return "", errors.New("the default profile has no managed network to reach Incus on; give --api-url")
	}
	n, _, err := srv.GetNetwork(network)
	if err != nil {
		return "", fmt.Errorf("read network %s: %w", network, err)
	}
	ip, _, err := net.ParseCIDR(n.Config["ipv4.address"])
	if err != nil {
		return "", fmt.Errorf("network %s has no IPv4 address; give --api-url", network)
	}
	return "https://" + net.JoinHostPort(ip.String(), port), nil
}

func createToken(srv incusclient.InstanceServer, name string, projects []string) (token, opID string, err error) {
	op, err := tokens(srv).CreateCertificateToken(api.CertificatesPost{
		Name:       name,
		Type:       api.CertificateTypeClient,
		Restricted: len(projects) > 0,
		Projects:   projects,
		Token:      true,
	})
	if err != nil {
		return "", "", fmt.Errorf("create trust token: %w", err)
	}
	meta := op.Get()
	tok, err := meta.ToCertificateAddToken()
	if err != nil {
		return "", "", errors.Join(fmt.Errorf("read trust token: %w", err), tokens(srv).DeleteOperation(meta.ID))
	}
	return tok.String(), meta.ID, nil
}

// tokens is where Incus keeps trust token operations, whatever the project.
func tokens(srv incusclient.InstanceServer) incusclient.InstanceServer {
	return srv.UseProject(api.ProjectDefaultName)
}

// revokeTokens removes tokens an install left unredeemed and gives their operation IDs.
func revokeTokens(srv incusclient.InstanceServer, name string, logger zerolog.Logger) ([]string, error) {
	ops, err := tokens(srv).GetOperations()
	if err != nil {
		return nil, fmt.Errorf("list trust tokens: %w", err)
	}
	var revoked []string
	var errs []error
	for _, op := range ops {
		if op.Class != api.OperationClassToken || op.StatusCode != api.Running {
			continue
		}
		tok, err := op.ToCertificateAddToken()
		if err != nil || tok.ClientName != name {
			continue
		}
		if err := tokens(srv).DeleteOperation(op.ID); err != nil {
			errs = append(errs, fmt.Errorf("revoke trust token: %w", err))
		} else {
			revoked = append(revoked, op.ID)
			logger.Info().Str("token", name).Msg("unused trust token revoked")
		}
	}
	return revoked, errors.Join(errs...)
}

func pushToken(srv incusclient.InstanceServer, name, token string) error {
	dirs := []struct {
		path     string
		uid      int64
		mode     int
		existsOK bool
	}{{"/run", 0, 0o755, true}, {secretsDir, imageUID, 0o700, false}}
	for _, d := range dirs {
		err := srv.CreateInstanceFile(name, d.path, incusclient.InstanceFileArgs{
			Type: "directory", UID: d.uid, GID: d.uid, Mode: d.mode,
		})
		if err != nil && (!d.existsOK || !exists(err)) {
			return fmt.Errorf("create %s in %s: %w", d.path, name, err)
		}
	}
	err := srv.CreateInstanceFile(name, tokenFile, incusclient.InstanceFileArgs{
		Content: strings.NewReader(token), UID: imageUID, GID: imageUID, Mode: 0o400, Type: "file",
	})
	if err != nil {
		return fmt.Errorf("push the token into %s: %w", name, err)
	}
	return nil
}

// writeNotifyURLs keeps the URLs, which hold tokens, out of the instance config.
func writeNotifyURLs(srv incusclient.InstanceServer, pool, vol string, urls []string) error {
	c, err := srv.GetStoragePoolVolumeFileSFTP(pool, "custom", vol)
	if err != nil {
		return fmt.Errorf("open volume %s: %w", vol, err)
	}
	defer c.Close()
	f, err := c.OpenFile("/"+notifyFile, os.O_WRONLY|os.O_CREATE|os.O_TRUNC)
	if err != nil {
		return fmt.Errorf("create %s in %s: %w", notifyFile, vol, err)
	}
	_, err = f.Write([]byte(strings.Join(urls, "\n") + "\n"))
	err = errors.Join(err, f.Close(), c.Chmod("/"+notifyFile, 0o400), c.Chown("/"+notifyFile, imageUID, imageUID))
	if err != nil {
		return fmt.Errorf("write %s in %s: %w", notifyFile, vol, err)
	}
	return nil
}

func deleteInstance(ctx context.Context, srv incusclient.InstanceServer, name string) error {
	op, err := srv.DeleteInstance(name)
	if err != nil {
		return err
	}
	wctx, cancel := incus.OpContext(ctx)
	defer cancel()
	return op.WaitContext(wctx)
}

// waitTrusted polls: the certificate appears once the token is redeemed.
func waitTrusted(ctx context.Context, srv incusclient.InstanceServer, name string, wait time.Duration) error {
	wctx, cancel := context.WithTimeout(ctx, wait)
	defer cancel()
	tick := time.NewTicker(2 * time.Second)
	defer tick.Stop()
	for {
		certs, err := srv.GetCertificates()
		if err != nil {
			return fmt.Errorf("list certificates: %w", err)
		}
		if slices.ContainsFunc(certs, func(c api.Certificate) bool { return c.Name == name }) {
			return nil
		}
		select {
		case <-wctx.Done():
			if ctx.Err() != nil {
				return fmt.Errorf("wait for %s to join: %w", name, ctx.Err())
			}
			return fmt.Errorf("%s did not join within %s; see incus console %s --show-log", name, wait, name)
		case <-tick.C:
		}
	}
}

func notFound(err error) bool { return api.StatusErrorCheck(err, http.StatusNotFound) }

func exists(err error) bool {
	return api.StatusErrorCheck(err, http.StatusConflict) || strings.Contains(err.Error(), "exists")
}
