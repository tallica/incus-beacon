package incus

import (
	"cmp"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"time"

	incusclient "github.com/lxc/incus/v7/client"
	"github.com/lxc/incus/v7/shared/api"
	localtls "github.com/lxc/incus/v7/shared/tls"
)

// Files in --state-dir.
const (
	stateCert        = "client.crt"
	stateKey         = "client.key"
	stateFingerprint = "server.fingerprint"
)

type state struct {
	cert, key   []byte
	fingerprint string
}

// loadState reports fs.ErrNotExist when nothing has been enrolled yet.
func loadState(dir string) (state, error) {
	var st state
	var err error
	if st.cert, err = os.ReadFile(filepath.Join(dir, stateCert)); err != nil {
		return st, err
	}
	if st.key, err = os.ReadFile(filepath.Join(dir, stateKey)); err != nil {
		return st, fmt.Errorf("state: %w", err)
	}
	fp, err := os.ReadFile(filepath.Join(dir, stateFingerprint))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return st, fmt.Errorf("state: %w", err)
	}
	st.fingerprint = strings.TrimSpace(string(fp))
	return st, nil
}

// enroll has Incus trust a new certificate, pinned by the token's fingerprint.
func enroll(ctx context.Context, opts ConnectOptions) (incusclient.InstanceServer, error) {
	token := opts.Token
	if opts.TokenFile != "" {
		b, err := os.ReadFile(opts.TokenFile)
		if err != nil {
			return nil, fmt.Errorf("token file: %w", err)
		}
		token = string(b)
	}
	token = strings.TrimSpace(token)
	tok, err := localtls.CertificateTokenDecode(token)
	if err != nil {
		return nil, fmt.Errorf("trust token: %w", err)
	}
	fp := normalizeFingerprint(cmp.Or(opts.ServerFingerprint, tok.Fingerprint))
	cert, key, err := generateCert(tok.ClientName, time.Now())
	if err != nil {
		return nil, fmt.Errorf("generate client certificate: %w", err)
	}
	// Staged first: Incus spends a token even when the rest fails.
	commit, err := stageState(opts.StateDir, state{cert: cert, key: key, fingerprint: fp})
	if err != nil {
		return nil, err
	}
	srv, err := dial(ctx, opts.URL, cert, key, fp, opts.UserAgent)
	if err != nil {
		return nil, errors.Join(err, commit(false))
	}
	err = srv.CreateCertificate(api.CertificatesPost{
		CertificatePut: api.CertificatePut{Name: tok.ClientName, Type: api.CertificateTypeClient},
		TrustToken:     token,
	})
	if err != nil {
		return nil, errors.Join(fmt.Errorf("redeem trust token: %w", err), commit(false))
	}
	if err := commit(true); err != nil {
		return nil, fmt.Errorf("trust token redeemed, but the certificate was not saved, so a new token is needed: %w", err)
	}
	return srv, trusted(srv)
}

// stageState writes .new files; commit renames them into place, or removes them.
func stageState(dir string, st state) (commit func(keep bool) error, err error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("state: %w", err)
	}
	files := []struct {
		name string
		data []byte
	}{
		{stateKey, st.key},
		{stateFingerprint, []byte(st.fingerprint + "\n")},
		// The cert goes last: it is what loadState looks for.
		{stateCert, st.cert},
	}
	var staged []string
	remove := func() error {
		var errs []error
		for _, p := range staged {
			if err := os.Remove(p + ".new"); err != nil && !errors.Is(err, os.ErrNotExist) {
				errs = append(errs, err)
			}
		}
		return errors.Join(errs...)
	}
	for _, f := range files {
		p := filepath.Join(dir, f.name)
		staged = append(staged, p)
		if err := os.WriteFile(p+".new", f.data, 0o600); err != nil {
			return nil, errors.Join(fmt.Errorf("state: %w", err), remove())
		}
	}
	return func(keep bool) error {
		if !keep {
			return remove()
		}
		for _, p := range staged {
			if err := os.Rename(p+".new", p); err != nil {
				return fmt.Errorf("state: %w", err)
			}
		}
		return nil
	}, nil
}

// generateCert backdates an hour: Incus refuses a certificate not yet valid by its clock.
func generateCert(name string, now time.Time) (certPEM, keyPEM []byte, err error) {
	key, err := ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
	if err != nil {
		return nil, nil, err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, nil, err
	}
	tmpl := x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: name},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.AddDate(10, 0, 0),
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, &tmpl, &tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, nil, err
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return nil, nil, err
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}), nil
}
