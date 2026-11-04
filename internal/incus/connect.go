// Package incus reads the watched OCI instances from the Incus API.
package incus

import (
	"context"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"

	incusclient "github.com/lxc/incus/v7/client"
	"github.com/lxc/incus/v7/shared/cliconfig"
)

type ConnectOptions struct {
	Remote            string
	URL               string
	ClientCert        string
	ClientKey         string
	ServerFingerprint string
	TokenFile         string
	UserAgent         string
}

// Connect uses URL with a client certificate, else an incus CLI remote.
func Connect(ctx context.Context, opts ConnectOptions) (incusclient.InstanceServer, error) {
	if opts.URL == "" {
		return connectRemote(opts.Remote)
	}
	if opts.TokenFile != "" {
		return nil, errors.New("--token-file is not supported yet; use --client-cert and --client-key")
	}
	if opts.ClientCert == "" {
		return nil, errors.New("--incus needs --client-cert and --client-key")
	}
	cert, err := os.ReadFile(opts.ClientCert)
	if err != nil {
		return nil, fmt.Errorf("client certificate: %w", err)
	}
	key, err := os.ReadFile(opts.ClientKey)
	if err != nil {
		return nil, fmt.Errorf("client key: %w", err)
	}
	args := &incusclient.ConnectionArgs{
		TLSClientCert: string(cert),
		TLSClientKey:  string(key),
		UserAgent:     opts.UserAgent,
		SkipGetEvents: true,
	}
	if opts.ServerFingerprint != "" {
		want := normalizeFingerprint(opts.ServerFingerprint)
		args.InsecureSkipVerify = true
		args.TransportWrapper = func(t *http.Transport) incusclient.HTTPTransporter {
			t.TLSClientConfig.VerifyPeerCertificate = pinFingerprint(want)
			return transporter{t: t}
		}
	}
	srv, err := incusclient.ConnectIncusWithContext(ctx, opts.URL, args)
	if err != nil {
		return nil, fmt.Errorf("connect to %s: %w", opts.URL, err)
	}
	return srv, trusted(srv)
}

func connectRemote(name string) (incusclient.InstanceServer, error) {
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
	return srv, trusted(srv)
}

// trusted fails early: Incus hides its API extensions from untrusted clients.
func trusted(srv incusclient.InstanceServer) error {
	info, _, err := srv.GetServer()
	if err != nil {
		return fmt.Errorf("read server info: %w", err)
	}
	if info.Auth != "trusted" {
		return errors.New("the server doesn't trust this client certificate; add it with incus config trust add-certificate")
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
