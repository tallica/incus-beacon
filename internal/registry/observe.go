package registry

import (
	"net/http"
	"strconv"
	"time"

	"github.com/tallica/incus-beacon/internal/metrics"
)

// latencyBounds cover a cached HEAD up to a slow manifest GET, in seconds.
var latencyBounds = []float64{0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30}

// observer sits under the limiter, so requests it pauses are never counted.
type observer struct {
	base     http.RoundTripper
	requests *metrics.Counter
	latency  *metrics.Histogram
}

func newObserver(base http.RoundTripper, set *metrics.Set) *observer {
	return &observer{
		base: base,
		requests: set.Counter("incus_beacon_registry_requests_total",
			"Registry requests by host, method and HTTP status; code is \"error\" when no response came.",
			"host", "method", "code"),
		latency: set.Histogram("incus_beacon_registry_request_duration_seconds",
			"Time from sending a registry request to its response headers.", latencyBounds, "host"),
	}
}

func (o *observer) RoundTrip(req *http.Request) (*http.Response, error) {
	start := time.Now()
	resp, err := o.base.RoundTrip(req)
	code := "error"
	if err == nil {
		code = strconv.Itoa(resp.StatusCode)
	}
	o.requests.Add(1, req.URL.Host, req.Method, code)
	o.latency.Observe(time.Since(start).Seconds(), req.URL.Host)
	return resp, err
}
