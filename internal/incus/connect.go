// Package incus reads the watched OCI instances from the Incus API.
package incus

import (
	"cmp"
	"context"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"strings"
	"sync"

	incusclient "github.com/lxc/incus/v7/client"
	"github.com/lxc/incus/v7/shared/cliconfig"
)

type ConnectOptions struct {
	Remote            string
	RemoteProject     string // replaces the remote's current project
	URL               string
	ClientCert        string
	ClientKey         string
	ServerFingerprint string
	Token             string
	TokenFile         string
	StateDir          string
	UserAgent         string
}

// Connect uses URL with a given, saved or token-enrolled certificate, else a CLI remote.
func Connect(ctx context.Context, opts ConnectOptions) (incusclient.InstanceServer, error) {
	if opts.URL == "" {
		return connectRemote(opts.Remote, opts.RemoteProject)
	}
	if opts.ClientCert != "" {
		cert, err := os.ReadFile(opts.ClientCert)
		if err != nil {
			return nil, fmt.Errorf("client certificate: %w", err)
		}
		key, err := os.ReadFile(opts.ClientKey)
		if err != nil {
			return nil, fmt.Errorf("client key: %w", err)
		}
		return connectTrusted(ctx, opts.URL, cert, key, opts.ServerFingerprint, opts.UserAgent)
	}
	if opts.StateDir != "" {
		st, err := loadState(opts.StateDir)
		switch {
		case err == nil:
			fp := cmp.Or(opts.ServerFingerprint, st.fingerprint)
			srv, err := connectTrusted(ctx, opts.URL, st.cert, st.key, fp, opts.UserAgent)
			if errors.Is(err, errUntrusted) {
				return nil, fmt.Errorf("%w; the certificate in %s was enrolled earlier: remove it and give a new --token", err, opts.StateDir)
			}
			return srv, err
		case !errors.Is(err, fs.ErrNotExist):
			return nil, err
		}
	}
	if opts.Token != "" || opts.TokenFile != "" {
		return enroll(ctx, opts)
	}
	return nil, errors.New("--incus needs --client-cert and --client-key, or --token (or --token-file) and --state-dir")
}

func connectTrusted(ctx context.Context, url string, cert, key []byte, fingerprint, userAgent string) (incusclient.InstanceServer, error) {
	srv, err := dial(ctx, url, cert, key, fingerprint, userAgent)
	if err != nil {
		return nil, err
	}
	return srv, trusted(srv)
}

func dial(ctx context.Context, url string, cert, key []byte, fingerprint, userAgent string) (incusclient.InstanceServer, error) {
	args := &incusclient.ConnectionArgs{
		TLSClientCert: string(cert),
		TLSClientKey:  string(key),
		UserAgent:     userAgent,
		SkipGetEvents: true,
	}
	if fingerprint != "" {
		want := normalizeFingerprint(fingerprint)
		args.InsecureSkipVerify = true
		args.TransportWrapper = func(t *http.Transport) incusclient.HTTPTransporter {
			t.TLSClientConfig.VerifyPeerCertificate = pinFingerprint(want)
			return transporter{t: t}
		}
	}
	srv, err := incusclient.ConnectIncusWithContext(ctx, url, args)
	if err != nil {
		return nil, fmt.Errorf("connect to %s: %w", url, err)
	}
	return srv, nil
}

func connectRemote(name, project string) (incusclient.InstanceServer, error) {
	conf, err := cliconfig.LoadConfig("")
	if err != nil {
		return nil, fmt.Errorf("load incus CLI config: %w", err)
	}
	if name == "" {
		name = conf.DefaultRemote
	}
	remote, ok := conf.Remotes[name]
	if !ok {
		return nil, fmt.Errorf("remote %q: not in the incus CLI config", name)
	}
	if remote.Protocol != "" && remote.Protocol != "incus" {
		return nil, fmt.Errorf("remote %q: protocol %s is not an Incus server", name, remote.Protocol)
	}
	srv, err := conf.GetInstanceServer(name)
	if err != nil {
		return nil, fmt.Errorf("connect to remote %q: %w", name, err)
	}
	if err := trusted(srv); err != nil {
		return nil, err
	}
	if project == "" {
		return srv, nil
	}
	if _, _, err := srv.GetProject(project); err != nil {
		return nil, fmt.Errorf("project %s on remote %q: %w", project, name, err)
	}
	return srv.UseProject(project), nil
}

var errUntrusted = errors.New("the server doesn't trust this client certificate")

// trusted fails early: Incus hides its API extensions from untrusted clients.
func trusted(srv incusclient.InstanceServer) error {
	info, _, err := srv.GetServer()
	if err != nil {
		return fmt.Errorf("read server info: %w", err)
	}
	if info.Auth != "trusted" {
		return fmt.Errorf("%w; add it with incus config trust add-certificate", errUntrusted)
	}
	return nil
}

type transporter struct{ t *http.Transport }

func (t transporter) RoundTrip(r *http.Request) (*http.Response, error) { return t.t.RoundTrip(r) }

func (t transporter) Transport() *http.Transport { return t.t }

// normalizeFingerprint accepts the colon-separated form incus prints too.
func normalizeFingerprint(fp string) string {
	return strings.ToLower(strings.ReplaceAll(strings.TrimSpace(fp), ":", ""))
}

func pinFingerprint(want string) func([][]byte, [][]*x509.Certificate) error {
	return func(raw [][]byte, _ [][]*x509.Certificate) error {
		if len(raw) == 0 {
			return errors.New("server sent no certificate")
		}
		sum := sha256.Sum256(raw[0])
		if got := hex.EncodeToString(sum[:]); got != want {
			return fmt.Errorf("server certificate fingerprint %s does not match --server-fingerprint", got)
		}
		return nil
	}
}

// Conn shares one connection, made on first use, between goroutines.
type Conn struct {
	Options ConnectOptions

	mu  sync.Mutex
	srv incusclient.InstanceServer
}

// Get holds the lock while connecting so a one-time token is spent only once.
func (c *Conn) Get(ctx context.Context) (incusclient.InstanceServer, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.srv == nil {
		srv, err := Connect(ctx, c.Options)
		if err != nil {
			return nil, err
		}
		c.srv = srv
	}
	return c.srv, nil
}

func (c *Conn) Drop() {
	c.mu.Lock()
	c.srv = nil
	c.mu.Unlock()
}
