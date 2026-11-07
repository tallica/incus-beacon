package metrics

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestExposition(t *testing.T) {
	t.Parallel()
	var s Set
	up := s.Gauge("up", "Always one.")
	up.Set(1)
	req := s.Counter("requests_total", "Requests by host and code.", "host", "code")
	req.Add(1, "ghcr.io", "200")
	req.Add(2, "docker.io", "429")
	req.Add(1, "ghcr.io", "200")
	s.Counter("idle_total", "Never touched.")
	lat := s.Histogram("latency_seconds", "Latency.", []float64{0.1, 1}, "host")
	lat.Observe(0.05, "ghcr.io")
	lat.Observe(0.1, "ghcr.io")
	lat.Observe(3, "ghcr.io")
	s.Gauge("info", "Labels are escaped.", "v").Set(1, "a\"b\\c\nd")

	want := `# HELP up Always one.
# TYPE up gauge
up 1
# HELP requests_total Requests by host and code.
# TYPE requests_total counter
requests_total{host="docker.io",code="429"} 2
requests_total{host="ghcr.io",code="200"} 2
# HELP idle_total Never touched.
# TYPE idle_total counter
idle_total 0
# HELP latency_seconds Latency.
# TYPE latency_seconds histogram
latency_seconds_bucket{host="ghcr.io",le="0.1"} 2
latency_seconds_bucket{host="ghcr.io",le="1"} 2
latency_seconds_bucket{host="ghcr.io",le="+Inf"} 3
latency_seconds_sum{host="ghcr.io"} 3.15
latency_seconds_count{host="ghcr.io"} 3
# HELP info Labels are escaped.
# TYPE info gauge
info{v="a\"b\\c\nd"} 1
`
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if got := rec.Body.String(); got != want {
		t.Errorf("exposition:\n%s\nwant:\n%s", got, want)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/plain; version=0.0.4") {
		t.Errorf("Content-Type %q", ct)
	}
}

func TestWrongLabelCountPanics(t *testing.T) {
	t.Parallel()
	defer func() {
		if recover() == nil {
			t.Error("no panic for a missing label value")
		}
	}()
	var s Set
	s.Counter("c_total", "c", "host").Add(1)
}
