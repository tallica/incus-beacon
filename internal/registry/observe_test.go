package registry

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/tallica/incus-beacon/internal/metrics"
)

func TestRequestsAreCounted(t *testing.T) {
	t.Parallel()
	f := newFakeRegistry(t)
	f.add("example/app", "1.2", manifestBlob(t, ocispec.MediaTypeImageManifest, layerA))
	var set metrics.Set
	c, err := New(Options{AuthFile: filepath.Join(t.TempDir(), "missing.json"), Metrics: &set})
	if err != nil {
		t.Fatal(err)
	}
	c.plainHTTP = true
	if _, err := c.Head(t.Context(), mustRef(t, f.host+"/example/app:1.2")); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Head(t.Context(), mustRef(t, f.host+"/example/app:9.9")); err == nil {
		t.Fatal("HEAD of a missing tag succeeded")
	}

	rec := httptest.NewRecorder()
	set.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	got := rec.Body.String()
	for _, want := range []string{
		`incus_beacon_registry_requests_total{host="` + f.host + `",method="HEAD",code="200"} 1`,
		`incus_beacon_registry_requests_total{host="` + f.host + `",method="HEAD",code="404"} 1`,
		`incus_beacon_registry_request_duration_seconds_count{host="` + f.host + `"} 2`,
	} {
		if !strings.Contains(got, want+"\n") {
			t.Errorf("missing %q in\n%s", want, got)
		}
	}
}

func TestPausedRequestsAreNotCounted(t *testing.T) {
	t.Parallel()
	var set metrics.Set
	l := newLimiter(newObserver(roundTripFunc(func(*http.Request) (*http.Response, error) {
		t.Fatal("a paused host reached the network")
		return nil, nil
	}), &set))
	l.pause(l.blocked, "example.com", time.Now().Add(time.Hour))
	req := httptest.NewRequest(http.MethodHead, "https://example.com/v2/", nil)
	resp, err := l.RoundTrip(req)
	if err == nil {
		resp.Body.Close()
		t.Fatal("paused host answered")
	}
	rec := httptest.NewRecorder()
	set.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if strings.Contains(rec.Body.String(), "example.com") {
		t.Errorf("a paused request was counted:\n%s", rec.Body.String())
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
