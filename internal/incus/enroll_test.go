package incus

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/lxc/incus/v7/shared/api"
)

// trustServer is a TLS Incus that trusts a client certificate once it redeems
// the token.
type trustServer struct {
	*httptest.Server
	secret string

	mu      sync.Mutex
	trusted map[string]bool // client cert fingerprints
	posts   int
}

func newTrustServer(t *testing.T) *trustServer {
	t.Helper()
	ts := &trustServer{secret: "s3cret", trusted: map[string]bool{}}
	ts.Server = httptest.NewUnstartedServer(http.HandlerFunc(ts.serve))
	ts.TLS = &tls.Config{ClientAuth: tls.RequestClientCert}
	ts.StartTLS()
	t.Cleanup(ts.Close)
	return ts
}

func (ts *trustServer) redeemed() int {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	return ts.posts
}

func (ts *trustServer) fingerprint() string {
	sum := sha256.Sum256(ts.Certificate().Raw)
	return hex.EncodeToString(sum[:])
}

func (ts *trustServer) token() string {
	tok := api.CertificateAddToken{
		ClientName:  "incus-beacon",
		Fingerprint: ts.fingerprint(),
		Addresses:   []string{strings.TrimPrefix(ts.URL, "https://")},
		Secret:      ts.secret,
	}
	return tok.String()
}

func (ts *trustServer) serve(w http.ResponseWriter, r *http.Request) {
	peer := ""
	if certs := r.TLS.PeerCertificates; len(certs) > 0 {
		sum := sha256.Sum256(certs[0].Raw)
		peer = hex.EncodeToString(sum[:])
	}
	ts.mu.Lock()
	defer ts.mu.Unlock()
	switch {
	case r.URL.Path == "/1.0":
		auth := "untrusted"
		if ts.trusted[peer] {
			auth = "trusted"
		}
		sync200(w, api.Server{ServerUntrusted: api.ServerUntrusted{Auth: auth}})
	case r.URL.Path == "/1.0/certificates" && r.Method == http.MethodPost:
		ts.posts++
		var post api.CertificatesPost
		_ = json.NewDecoder(r.Body).Decode(&post)
		if post.TrustToken != ts.token() || peer == "" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusForbidden)
			_ = json.NewEncoder(w).Encode(map[string]any{"type": "error", "error": "not authorized", "error_code": 403})
			return
		}
		ts.trusted[peer] = true
		ts.secret += "-spent"
		sync200(w, nil)
	default:
		notFound(w)
	}
}

func TestEnrollWithToken(t *testing.T) {
	t.Parallel()
	ts := newTrustServer(t)
	dir := filepath.Join(t.TempDir(), "state")
	opts := ConnectOptions{URL: ts.URL, Token: ts.token(), StateDir: dir}
	if _, err := Connect(context.Background(), opts); err != nil {
		t.Fatalf("enroll: %v", err)
	}
	for _, name := range []string{stateCert, stateKey, stateFingerprint} {
		fi, err := os.Stat(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		if fi.Mode().Perm() != 0o600 {
			t.Errorf("%s mode %v, want 0600", name, fi.Mode().Perm())
		}
	}
	if fp, _ := os.ReadFile(filepath.Join(dir, stateFingerprint)); strings.TrimSpace(string(fp)) != ts.fingerprint() {
		t.Errorf("stored fingerprint %q, want the token's %s", fp, ts.fingerprint())
	}
	if leftovers, _ := filepath.Glob(filepath.Join(dir, "*.new")); len(leftovers) > 0 {
		t.Errorf("staged files left: %v", leftovers)
	}

	// The token is spent: the next start must use the saved pair, not redeem.
	if _, err := Connect(context.Background(), opts); err != nil {
		t.Fatalf("reconnect: %v", err)
	}
	if n := ts.redeemed(); n != 1 {
		t.Errorf("%d token redemptions, want 1", n)
	}
}

func TestEnrollRefusedTokenSavesNothing(t *testing.T) {
	t.Parallel()
	ts := newTrustServer(t)
	dir := t.TempDir()
	good := ts.token()
	ts.secret = "other"
	_, err := Connect(context.Background(), ConnectOptions{URL: ts.URL, Token: good, StateDir: dir})
	if err == nil || !strings.Contains(err.Error(), "redeem trust token") {
		t.Fatalf("err %v, want a redeem error", err)
	}
	if entries, _ := os.ReadDir(dir); len(entries) > 0 {
		t.Errorf("state dir not empty after a refused token: %v", entries)
	}
}

func TestEnrollUnwritableStateKeepsToken(t *testing.T) {
	t.Parallel()
	if os.Getuid() == 0 {
		t.Skip("root writes anywhere")
	}
	ts := newTrustServer(t)
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
	_, err := Connect(context.Background(), ConnectOptions{URL: ts.URL, Token: ts.token(), StateDir: dir})
	if err == nil || !strings.Contains(err.Error(), "state") {
		t.Fatalf("err %v, want a state error", err)
	}
	if ts.redeemed() != 0 {
		t.Errorf("token redeemed although the state dir is unwritable")
	}
}

func TestEnrollBadToken(t *testing.T) {
	t.Parallel()
	_, err := Connect(context.Background(), ConnectOptions{URL: "https://192.168.1.10:8443", Token: "not-a-token", StateDir: t.TempDir()})
	if err == nil || !strings.Contains(err.Error(), "trust token") {
		t.Errorf("err %v, want a trust token error", err)
	}
}

func TestSavedPairUntrustedSaysWhatToDo(t *testing.T) {
	t.Parallel()
	ts := newTrustServer(t)
	dir := t.TempDir()
	opts := ConnectOptions{URL: ts.URL, Token: ts.token(), StateDir: dir}
	if _, err := Connect(context.Background(), opts); err != nil {
		t.Fatal(err)
	}
	ts.mu.Lock()
	clear(ts.trusted)
	ts.mu.Unlock()
	_, err := Connect(context.Background(), opts)
	if err == nil || !strings.Contains(err.Error(), "remove it and give a new --token") {
		t.Errorf("err %v, want the re-enroll hint", err)
	}
}

func TestGenerateCertIsBackdated(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)
	certPEM, keyPEM, err := generateCert("incus-beacon", now)
	if err != nil {
		t.Fatal(err)
	}
	pair, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		t.Fatal(err)
	}
	if !pair.Leaf.NotBefore.Before(now.Add(-59 * time.Minute)) {
		t.Errorf("NotBefore %s, want an hour before %s", pair.Leaf.NotBefore, now)
	}
}
