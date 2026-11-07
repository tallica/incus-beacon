// Package install creates and removes a serve instance that joins Incus with a trust token.
package install

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"time"

	incusclient "github.com/lxc/incus/v7/client"
	"github.com/lxc/incus/v7/shared/api"
	"github.com/rs/zerolog"
)

const (
	stateDir   = "/var/lib/incus-beacon"
	secretsDir = "/run/secrets"
	tokenFile  = secretsDir + "/token"
	stateDev   = "state"
	imageUID   = 65534
	imageRepo  = "ghcr.io/tallica/incus-beacon"
)

type Options struct {
	Name        string
	Image       string
	StoragePool string
	APIURL      string
	Projects    []string
	Watch       string
	Events      bool
	Wait        time.Duration
	Log         zerolog.Logger
}

// VolumeName is the state volume of the instance called name.
func VolumeName(name string) string { return name + "-state" }

var release = regexp.MustCompile(`^v?(\d+)\.(\d+)\.\d+$`)

// DefaultImage follows the installer's major.minor; a dev build gets latest.
func DefaultImage(version string) (image string, released bool) {
	if m := release.FindStringSubmatch(version); m != nil {
		return imageRepo + ":" + m[1] + "." + m[2], true
	}
	return imageRepo + ":latest", false
}

// Install undoes what it created if a step before the start fails.
func Install(ctx context.Context, srv incusclient.InstanceServer, opts Options) error {
	if err := checkFree(srv, opts.Name); err != nil {
		return err
	}
	// Incus accepts a token for a missing project; the instance then never joins.
	for _, p := range opts.Projects {
		if _, _, err := srv.GetProject(p); err != nil {
			return fmt.Errorf("project %s: %w", p, err)
		}
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
	server, alias, err := splitImage(opts.Image)
	if err != nil {
		return err
	}

	var undo []func() error
	rollback := func(err error) error {
		for _, f := range slices.Backward(undo) {
			err = errors.Join(err, f())
		}
		return err
	}

	vol := VolumeName(opts.Name)
	err = srv.CreateStoragePoolVolume(pool, api.StorageVolumesPost{
		Name: vol,
		Type: "custom",
		StorageVolumePut: api.StorageVolumePut{Config: map[string]string{
			"initial.uid": fmt.Sprint(imageUID),
			"initial.gid": fmt.Sprint(imageUID),
		}},
	})
	if err != nil {
		return fmt.Errorf("create volume %s: %w", vol, err)
	}
	undo = append(undo, func() error { return srv.DeleteStoragePoolVolume(pool, "custom", vol) })
	opts.Log.Info().Str("pool", pool).Str("volume", vol).Msg("volume created")

	token, opID, err := createToken(srv, opts.Name, opts.Projects)
	if err != nil {
		return rollback(err)
	}
	undo = append(undo, func() error { return tokens(srv).DeleteOperation(opID) })

	env := map[string]string{
		"environment.BEACON_INCUS":      apiURL,
		"environment.BEACON_TOKEN_FILE": tokenFile,
	}
	if len(opts.Projects) > 0 {
		env["environment.BEACON_PROJECT"] = strings.Join(opts.Projects, ",")
	}
	if opts.Watch != "" && opts.Watch != "all" {
		env["environment.BEACON_WATCH"] = opts.Watch
	}
	if opts.Events {
		env["environment.BEACON_EVENTS"] = "true"
	}
	op, err := srv.CreateInstance(api.InstancesPost{
		Name: opts.Name,
		Type: api.InstanceTypeContainer,
		Source: api.InstanceSource{
			Type: "image", Protocol: "oci", Server: server, Alias: alias,
		},
		InstancePut: api.InstancePut{
			Config: env,
			Devices: map[string]map[string]string{stateDev: {
				"type": "disk", "pool": pool, "source": vol, "path": stateDir,
			}},
		},
	})
	if err == nil {
		err = op.WaitContext(ctx)
	}
	if err != nil {
		return rollback(fmt.Errorf("create instance %s from %s: %w", opts.Name, opts.Image, err))
	}
	undo = append(undo, func() error { return deleteInstance(srv, opts.Name) })
	opts.Log.Info().Str("instance", opts.Name).Str("image", opts.Image).Str("incus", apiURL).Msg("instance created")

	if err := pushToken(srv, opts.Name, token); err != nil {
		return rollback(err)
	}
	if err := setState(ctx, srv, opts.Name, "start"); err != nil {
		return rollback(fmt.Errorf("start %s: %w", opts.Name, err))
	}

	// Past here the instance stays: its log says why it didn't join.
	if err := waitTrusted(ctx, srv, opts.Name, opts.Wait); err != nil {
		return fmt.Errorf("%w; see incus console %s --show-log", err, opts.Name)
	}
	if err := srv.DeleteInstanceFile(opts.Name, tokenFile); err != nil {
		opts.Log.Warn().Err(err).Str("file", tokenFile).Msg("spent token not removed")
	}
	opts.Log.Info().Str("instance", opts.Name).Msg("joined Incus")
	return nil
}

// Uninstall tolerates any of its parts being gone already.
func Uninstall(ctx context.Context, srv incusclient.InstanceServer, name string, log zerolog.Logger) error {
	var errs []error
	pool := ""
	inst, _, err := srv.GetInstance(name)
	switch {
	case err == nil:
		pool = inst.Devices[stateDev]["pool"]
		if inst.StatusCode != api.Stopped {
			if err := setState(ctx, srv, name, "stop"); err != nil {
				errs = append(errs, fmt.Errorf("stop %s: %w", name, err))
			}
		}
		if err := deleteInstance(srv, name); err != nil {
			errs = append(errs, fmt.Errorf("delete %s: %w", name, err))
		} else {
			log.Info().Str("instance", name).Msg("instance deleted")
		}
	case !notFound(err):
		errs = append(errs, err)
	}
	if pool == "" {
		pool, _, _ = profileDefaults(srv)
	}
	if pool != "" {
		vol := VolumeName(name)
		err := srv.DeleteStoragePoolVolume(pool, "custom", vol)
		switch {
		case err == nil:
			log.Info().Str("volume", vol).Msg("volume deleted")
		case !notFound(err):
			errs = append(errs, fmt.Errorf("delete volume %s: %w", vol, err))
		}
	}
	if err := revokeTokens(srv, name, log); err != nil {
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
			log.Info().Str("certificate", c.Name).Msg("certificate removed")
		}
	}
	return errors.Join(errs...)
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

// splitImage turns registry/repo:tag into the OCI source Incus pulls from.
func splitImage(ref string) (server, alias string, err error) {
	host, rest, ok := strings.Cut(ref, "/")
	if !ok || !strings.ContainsAny(host, ".:") || rest == "" {
		return "", "", fmt.Errorf("image %q: want registry/repository[:tag]", ref)
	}
	return "https://" + host, rest, nil
}

func createToken(srv incusclient.InstanceServer, name string, projects []string) (token, opID string, err error) {
	op, err := tokens(srv).CreateCertificateToken(api.CertificatesPost{
		CertificatePut: api.CertificatePut{
			Name:       name,
			Type:       api.CertificateTypeClient,
			Restricted: len(projects) > 0,
			Projects:   projects,
		},
		Token: true,
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

// revokeTokens removes tokens an install left unredeemed.
func revokeTokens(srv incusclient.InstanceServer, name string, log zerolog.Logger) error {
	ops, err := tokens(srv).GetOperations()
	if err != nil {
		return fmt.Errorf("list trust tokens: %w", err)
	}
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
			log.Info().Str("token", name).Msg("unused trust token revoked")
		}
	}
	return errors.Join(errs...)
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

func setState(ctx context.Context, srv incusclient.InstanceServer, name, action string) error {
	op, err := srv.UpdateInstanceState(name, api.InstanceStatePut{Action: action, Timeout: -1, Force: action == "stop"}, "")
	if err != nil {
		return err
	}
	return op.WaitContext(ctx)
}

func deleteInstance(srv incusclient.InstanceServer, name string) error {
	op, err := srv.DeleteInstance(name)
	if err != nil {
		return err
	}
	return op.Wait()
}

// waitTrusted polls: the certificate appears once the token is redeemed.
func waitTrusted(ctx context.Context, srv incusclient.InstanceServer, name string, wait time.Duration) error {
	ctx, cancel := context.WithTimeout(ctx, wait)
	defer cancel()
	tick := time.NewTicker(2 * time.Second)
	defer tick.Stop()
	for {
		certs, err := srv.GetCertificates()
		if err != nil {
			return err
		}
		if slices.ContainsFunc(certs, func(c api.Certificate) bool { return c.Name == name }) {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("%s did not join within %s", name, wait)
		case <-tick.C:
		}
	}
}

func notFound(err error) bool { return api.StatusErrorCheck(err, http.StatusNotFound) }

func exists(err error) bool {
	return api.StatusErrorCheck(err, http.StatusConflict) || strings.Contains(err.Error(), "exists")
}
