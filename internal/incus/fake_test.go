package incus

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"

	incusclient "github.com/lxc/incus/v7/client"
	"github.com/lxc/incus/v7/shared/api"
)

// fakeIncus serves the few /1.0 endpoints incus-beacon reads.
type fakeIncus struct {
	instances []api.Instance
	images    map[string]api.Image // key: project/fingerprint

	mu       sync.Mutex
	requests []string
}

func (f *fakeIncus) connect(t *testing.T) incusclient.InstanceServer {
	t.Helper()
	ts := httptest.NewServer(f)
	t.Cleanup(ts.Close)
	target, err := url.Parse(ts.URL)
	if err != nil {
		t.Fatal(err)
	}
	client := &http.Client{Transport: rewrite{target}}
	srv, err := incusclient.ConnectIncusHTTP(nil, client)
	if err != nil {
		t.Fatal(err)
	}
	return srv
}

func (f *fakeIncus) count(prefix string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, r := range f.requests {
		if strings.HasPrefix(r, prefix) {
			n++
		}
	}
	return n
}

func (f *fakeIncus) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.requests = append(f.requests, r.URL.Path)
	f.mu.Unlock()

	q := r.URL.Query()
	switch {
	case r.URL.Path == "/1.0":
		sync200(w, api.Server{ServerUntrusted: api.ServerUntrusted{
			APIExtensions: []string{"instance_all_projects"},
			Auth:          "trusted",
		}})
	case r.URL.Path == "/1.0/instances":
		var out []api.Instance
		for _, inst := range f.instances {
			if q.Get("all-projects") == "true" || project(inst.Project) == project(q.Get("project")) {
				out = append(out, inst)
			}
		}
		sync200(w, out)
	case strings.HasPrefix(r.URL.Path, "/1.0/images/"):
		fp := strings.TrimPrefix(r.URL.Path, "/1.0/images/")
		img, ok := f.images[project(q.Get("project"))+"/"+fp]
		if !ok {
			notFound(w)
			return
		}
		sync200(w, img)
	default:
		notFound(w)
	}
}

func sync200(w http.ResponseWriter, metadata any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"type": "sync", "status": "Success", "status_code": 200, "metadata": metadata,
	})
}

func notFound(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusNotFound)
	_ = json.NewEncoder(w).Encode(map[string]any{"type": "error", "error": "not found", "error_code": 404})
}

// rewrite sends the client's fixed https://custom.socket requests to the fake.
type rewrite struct{ target *url.URL }

func (rw rewrite) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.URL.Scheme = rw.target.Scheme
	r.URL.Host = rw.target.Host
	return http.DefaultTransport.RoundTrip(r)
}
