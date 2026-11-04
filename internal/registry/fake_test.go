package registry

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

type blob struct {
	mediaType string
	body      []byte
	digest    string
}

// fakeRegistry serves the parts of the OCI distribution API incus-beacon uses.
type fakeRegistry struct {
	mu        sync.Mutex
	manifests map[string]map[string]blob // repo -> tag or digest -> manifest
	tags      map[string][]string
	auth      map[string]string // repo -> "user:pass"
	header    http.Header       // added to every response
	status    int               // when set, every manifest request answers with it
	requests  []string          // "METHOD path"
	host      string
}

func newFakeRegistry(t *testing.T) *fakeRegistry {
	t.Helper()
	f := &fakeRegistry{
		manifests: map[string]map[string]blob{},
		tags:      map[string][]string{},
		auth:      map[string]string{},
		header:    http.Header{},
	}
	ts := httptest.NewServer(f)
	t.Cleanup(ts.Close)
	u, err := url.Parse(ts.URL)
	if err != nil {
		t.Fatal(err)
	}
	f.host = u.Host
	return f
}

func (f *fakeRegistry) add(repo, ref string, b blob) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.manifests[repo] == nil {
		f.manifests[repo] = map[string]blob{}
	}
	f.manifests[repo][b.digest] = b
	if ref != "" {
		f.manifests[repo][ref] = b
	}
}

func (f *fakeRegistry) count(method string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, r := range f.requests {
		if strings.HasPrefix(r, method+" ") && !strings.HasSuffix(r, " /v2/") {
			n++
		}
	}
	return n
}

func (f *fakeRegistry) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requests = append(f.requests, r.Method+" "+r.URL.Path)
	for k, v := range f.header {
		w.Header()[k] = v
	}
	if r.URL.Path == "/v2/" {
		return
	}
	rest := strings.TrimPrefix(r.URL.Path, "/v2/")
	repo, ref, isManifest := strings.Cut(rest, "/manifests/")
	if !isManifest {
		repo, _, _ = strings.Cut(rest, "/tags/list")
	}
	if want, ok := f.auth[repo]; ok {
		user, pass, _ := r.BasicAuth()
		if user+":"+pass != want {
			w.Header().Set("WWW-Authenticate", `Basic realm="test"`)
			http.Error(w, `{"errors":[{"code":"UNAUTHORIZED"}]}`, http.StatusUnauthorized)
			return
		}
	}
	if isManifest {
		f.serveManifest(w, r, repo, ref)
		return
	}
	f.serveTags(w, r, repo)
}

func (f *fakeRegistry) serveManifest(w http.ResponseWriter, r *http.Request, repo, ref string) {
	if f.status != 0 {
		w.WriteHeader(f.status)
		return
	}
	b, ok := f.manifests[repo][ref]
	if !ok {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		fmt.Fprint(w, `{"errors":[{"code":"MANIFEST_UNKNOWN"}]}`)
		return
	}
	w.Header().Set("Content-Type", b.mediaType)
	w.Header().Set("Docker-Content-Digest", b.digest)
	w.Header().Set("Content-Length", strconv.Itoa(len(b.body)))
	if r.Method == http.MethodGet {
		_, _ = w.Write(b.body)
	}
}

// serveTags pages two tags at a time, as registries may.
func (f *fakeRegistry) serveTags(w http.ResponseWriter, r *http.Request, repo string) {
	tags := f.tags[repo]
	start := 0
	if last := r.URL.Query().Get("last"); last != "" {
		start = slices.Index(tags, last) + 1
	}
	end := min(start+2, len(tags))
	if end < len(tags) {
		w.Header().Set("Link", fmt.Sprintf(`</v2/%s/tags/list?n=2&last=%s>; rel="next"`, repo, tags[end-1]))
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"name": repo, "tags": tags[start:end]})
}

func newBlob(t *testing.T, mediaType string, v any) blob {
	t.Helper()
	body, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(body)
	return blob{mediaType: mediaType, body: body, digest: "sha256:" + hex.EncodeToString(sum[:])}
}

func manifestBlob(t *testing.T, mediaType string, layers ...string) blob {
	t.Helper()
	m := ocispec.Manifest{MediaType: mediaType}
	m.SchemaVersion = 2
	for _, l := range layers {
		m.Layers = append(m.Layers, ocispec.Descriptor{MediaType: ocispec.MediaTypeImageLayerGzip, Digest: digestOf(l), Size: 1})
	}
	return newBlob(t, mediaType, m)
}

func indexBlob(t *testing.T, mediaType string, entries map[string]blob) blob {
	t.Helper()
	idx := ocispec.Index{MediaType: mediaType}
	idx.SchemaVersion = 2
	keys := make([]string, 0, len(entries))
	for k := range entries {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	for _, k := range keys {
		b := entries[k]
		parts := strings.Split(k, "/")
		p := &ocispec.Platform{OS: parts[0], Architecture: parts[1]}
		if len(parts) > 2 {
			p.Variant = parts[2]
		}
		idx.Manifests = append(idx.Manifests, ocispec.Descriptor{
			MediaType: b.mediaType, Digest: digestOf(b.digest), Size: int64(len(b.body)), Platform: p,
		})
	}
	return newBlob(t, mediaType, idx)
}

func writeAuthFile(t *testing.T, host, userPass string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.json")
	cfg := map[string]any{"auths": map[string]any{
		host: map[string]string{"auth": base64.StdEncoding.EncodeToString([]byte(userPass))},
	}}
	b, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func newTestClient(t *testing.T, authFile string) *Client {
	t.Helper()
	if authFile == "" {
		authFile = filepath.Join(t.TempDir(), "missing.json")
	}
	c, err := New(Options{AuthFile: authFile})
	if err != nil {
		t.Fatal(err)
	}
	c.plainHTTP = true
	return c
}

func mustRef(t *testing.T, s string) Ref {
	t.Helper()
	r, err := ParseRef(s)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func digestOf(s string) digest.Digest { return digest.Digest(s) }
