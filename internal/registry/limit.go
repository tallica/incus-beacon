package registry

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

var ErrRateLimited = errors.New("rate limited")

type RateLimitError struct {
	Host  string
	Until time.Time
}

func (e *RateLimitError) Error() string {
	return fmt.Sprintf("%s: rate limited until %s", e.Host, e.Until.UTC().Format(time.RFC3339))
}

func (e *RateLimitError) Is(target error) bool { return target == ErrRateLimited }

const defaultBackoff = time.Hour

// limiter pauses a host after a 429, and pauses its GETs once
// ratelimit-remaining reaches 0: Docker Hub keeps answering HEADs for free.
type limiter struct {
	base http.RoundTripper
	now  func() time.Time

	mu      sync.Mutex
	blocked map[string]time.Time
	noGets  map[string]time.Time
}

func newLimiter(base http.RoundTripper) *limiter {
	return &limiter{
		base:    base,
		now:     time.Now,
		blocked: map[string]time.Time{},
		noGets:  map[string]time.Time{},
	}
}

func (l *limiter) RoundTrip(req *http.Request) (*http.Response, error) {
	host := req.URL.Host
	if until, ok := l.pausedUntil(host, req.Method); ok {
		return nil, &RateLimitError{Host: host, Until: until}
	}
	resp, err := l.base.RoundTrip(req)
	if err != nil {
		return nil, err
	}
	now := l.now()
	switch {
	case resp.StatusCode == http.StatusTooManyRequests:
		until := now.Add(retryAfter(resp.Header.Get("Retry-After"), now))
		l.pause(l.blocked, host, until)
		resp.Body.Close()
		return nil, &RateLimitError{Host: host, Until: until}
	case remainingZero(resp.Header.Get("Ratelimit-Remaining")):
		l.pause(l.noGets, host, now.Add(window(resp.Header.Get("Ratelimit-Limit"))))
	}
	return resp, nil
}

func (l *limiter) pausedUntil(host, method string) (time.Time, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	if until := l.blocked[host]; now.Before(until) {
		return until, true
	}
	if until := l.noGets[host]; method != http.MethodHead && now.Before(until) {
		return until, true
	}
	return time.Time{}, false
}

func (l *limiter) pause(m map[string]time.Time, host string, until time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if until.After(m[host]) {
		m[host] = until
	}
}

func retryAfter(v string, now time.Time) time.Duration {
	if secs, err := strconv.Atoi(strings.TrimSpace(v)); err == nil && secs > 0 {
		return time.Duration(secs) * time.Second
	}
	if t, err := http.ParseTime(v); err == nil && t.After(now) {
		return t.Sub(now)
	}
	return defaultBackoff
}

// Docker Hub sends "<count>;w=<window seconds>" in both ratelimit headers.
func remainingZero(v string) bool {
	count, _, _ := strings.Cut(v, ";")
	n, err := strconv.Atoi(strings.TrimSpace(count))
	return err == nil && n <= 0
}

func window(limit string) time.Duration {
	for part := range strings.SplitSeq(limit, ";") {
		if w, ok := strings.CutPrefix(strings.TrimSpace(part), "w="); ok {
			if secs, err := strconv.Atoi(w); err == nil && secs > 0 {
				return time.Duration(secs) * time.Second
			}
		}
	}
	return defaultBackoff
}
