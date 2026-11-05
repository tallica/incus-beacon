package incus

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
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

	mu        sync.Mutex
	requests  []string
	versions  map[string]int // project/name -> ETag counter
	conflicts int            // PUTs to refuse with 412 before accepting
	puts      int
	untrusted bool
}

func (f *fakeIncus) auth() string {
	if f.untrusted {
		return "untrusted"
	}
	return "trusted"
}

func (f *fakeIncus) connect(t *testing.T) incusclient.InstanceServer {
	t.Helper()
	ts := httptest.NewServer(f)
	t.Cleanup(ts.Close)
	target, err := url.Parse(ts.URL)
	if err != nil {
		t.Fatal(err)
	}
	client := &http.Client{Transport: rewrite{target: target, t: &http.Transport{}}}
	srv, err := incusclient.ConnectIncusHTTP(&incusclient.ConnectionArgs{SkipGetEvents: true}, client)
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
			APIExtensions: []string{"instance_all_projects", "projects"},
			Auth:          f.auth(),
		}})
	case r.URL.Path == "/1.0/projects":
		urls := []string{"/1.0/projects/" + api.ProjectDefaultName}
		for _, inst := range f.instances {
			if u := "/1.0/projects/" + project(inst.Project); !slices.Contains(urls, u) {
				urls = append(urls, u)
			}
		}
		sync200(w, urls)
	case r.URL.Path == "/1.0/instances":
		var out []api.Instance
		for _, inst := range f.instances {
			if q.Get("all-projects") == "true" || project(inst.Project) == project(q.Get("project")) {
				out = append(out, inst)
			}
		}
		sync200(w, out)
	case strings.HasPrefix(r.URL.Path, "/1.0/instances/"):
		f.serveInstance(w, r, strings.TrimPrefix(r.URL.Path, "/1.0/instances/"), project(q.Get("project")))
	case strings.HasPrefix(r.URL.Path, "/1.0/operations/"):
		sync200(w, api.Operation{ID: "op", Status: "Success", StatusCode: api.Success})
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

func (f *fakeIncus) serveInstance(w http.ResponseWriter, r *http.Request, name, proj string) {
	i := slices.IndexFunc(f.instances, func(inst api.Instance) bool {
		return inst.Name == name && project(inst.Project) == proj
	})
	if i < 0 {
		notFound(w)
		return
	}
	if f.versions == nil {
		f.versions = map[string]int{}
	}
	key := proj + "/" + name
	etag := fmt.Sprintf(`"v%d"`, f.versions[key])
	switch r.Method {
	case http.MethodGet:
		w.Header().Set("ETag", etag)
		sync200(w, f.instances[i])
	case http.MethodPut:
		if f.conflicts > 0 || r.Header.Get("If-Match") != etag {
			f.conflicts--
			f.versions[key]++
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusPreconditionFailed)
			_ = json.NewEncoder(w).Encode(map[string]any{"type": "error", "error": "ETag mismatch", "error_code": 412})
			return
		}
		var put api.InstancePut
		if err := json.NewDecoder(r.Body).Decode(&put); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		f.puts++
		f.versions[key]++
		f.instances[i].Config = put.Config
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"type": "async", "status": "Operation created", "status_code": 100,
			"operation": "/1.0/operations/op",
			"metadata":  api.Operation{ID: "op", Status: "Running", StatusCode: api.Running},
		})
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
// It is an incus HTTPTransporter, which operation waits require.
type rewrite struct {
	target *url.URL
	t      *http.Transport
}

func (rw rewrite) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.URL.Scheme = rw.target.Scheme
	r.URL.Host = rw.target.Host
	return rw.t.RoundTrip(r)
}

func (rw rewrite) Transport() *http.Transport { return rw.t }
