package notify

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/tallica/incus-beacon/internal/incus"
	"github.com/tallica/incus-beacon/internal/metrics"
)

func TestNotable(t *testing.T) {
	t.Parallel()
	upd := func(latest string) incus.Beacon { return incus.Beacon{Status: "update-available", Latest: latest} }
	cur := incus.Beacon{Status: "current"}
	bad := incus.Beacon{Status: "error", Error: "rate limited"}
	tests := []struct {
		name       string
		prev, next incus.Beacon
		errs, want bool
	}{
		{"first update", cur, upd("2"), false, true},
		{"never checked", incus.Beacon{}, upd("2"), false, true},
		{"same update again", upd("2"), upd("2"), false, false},
		{"newer latest", upd("2"), upd("3"), false, true},
		{"update applied", upd("2"), cur, false, false},
		{"error, not asked", cur, bad, false, false},
		{"error, asked", cur, bad, true, true},
		{"still failing", bad, bad, true, false},
		{"update after error", bad, upd("2"), false, true},
	}
	for _, tt := range tests {
		if got := Notable(tt.prev, tt.next, tt.errs); got != tt.want {
			t.Errorf("%s: Notable = %v, want %v", tt.name, got, tt.want)
		}
	}
}

var sample = []Transition{
	{Project: "default", Instance: "web", Image: "docker.io/library/nginx:1.27", Status: "update-available", Previous: "current", Latest: "1.31"},
	{Project: "apps", Instance: "api", Image: "ghcr.io/example/api:2", Status: "update-available", Latest: "2@sha256:bbbbbbbbbbbb"},
	{Project: "apps", Instance: "db", Image: "docker.io/library/postgres:17", Status: "error", Previous: "current", Reason: "registry auth failed"},
}

var batch = Batch{Server: "node1", Transitions: sample}

const sampleTitle = "incus-beacon on node1: 2 updates available, 1 check failing"

func TestMessage(t *testing.T) {
	t.Parallel()
	title, body := Message(batch)
	if title != sampleTitle {
		t.Errorf("title %q", title)
	}
	want := "• web (default): nginx:1.27 → 1.31\n" +
		"• api (apps): ghcr.io/example/api:2 rebuilt\n" +
		"• db (apps): postgres:17 check failed: registry auth failed"
	if body != want {
		t.Errorf("body:\n%s\nwant:\n%s", body, want)
	}
	if title, _ := Message(Batch{Transitions: sample[:1]}); title != "incus-beacon: 1 update available" {
		t.Errorf("no server: title %q", title)
	}
	for _, tt := range []struct {
		st   style
		want string
	}{
		{slackStyle, "• *web* (default): `nginx:1.27` → `1.31`"},
		{discordStyle, "• **web** (default): `nginx:1.27` → `1.31`"},
	} {
		if _, body := message(Batch{Transitions: sample[:1]}, tt.st); body != tt.want {
			t.Errorf("chat body %q, want %q", body, tt.want)
		}
	}
}

func TestShortRefAndTag(t *testing.T) {
	t.Parallel()
	for ref, want := range map[string][2]string{
		"docker.io/library/nginx:1.27":     {"nginx:1.27", "1.27"},
		"docker.io/grafana/grafana:11":     {"grafana/grafana:11", "11"},
		"ghcr.io/example/api:2":            {"ghcr.io/example/api:2", "2"},
		"registry.example.com:5000/app":    {"registry.example.com:5000/app", ""},
		"registry.example.com:5000/app:v3": {"registry.example.com:5000/app:v3", "v3"},
	} {
		if got := [2]string{shortRef(ref), tagOf(ref)}; got != want {
			t.Errorf("%s: short, tag = %q, want %q", ref, got, want)
		}
	}
}

type request struct {
	path, contentType string
	body              []byte
}

type capture struct {
	mu       sync.Mutex
	requests []request
	status   int
}

// server answers every POST, recording it; a Discord or Slack URL points here.
func (c *capture) server(t *testing.T) *httptest.Server {
	t.Helper()
	ts := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		c.mu.Lock()
		c.requests = append(c.requests, request{r.URL.Path, r.Header.Get("Content-Type"), b})
		status := c.status
		c.mu.Unlock()
		if status != 0 {
			w.WriteHeader(status)
		}
	}))
	t.Cleanup(ts.Close)
	return ts
}

func (c *capture) last(t *testing.T) request {
	t.Helper()
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.requests) == 0 {
		t.Fatal("no request")
	}
	return c.requests[len(c.requests)-1]
}

func sender(t *testing.T, ts *httptest.Server, set *metrics.Set, log io.Writer, urls ...string) *Sender {
	t.Helper()
	if log == nil {
		log = io.Discard
	}
	s, err := New(Options{URLs: urls, Client: ts.Client(), Timeout: 5 * time.Second, Metrics: set, Log: zerolog.New(log)})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestWebhook(t *testing.T) {
	t.Parallel()
	var c capture
	ts := c.server(t)
	sender(t, ts, nil, nil, ts.URL+"/hook").Notify(t.Context(), batch)
	r := c.last(t)
	if r.path != "/hook" || r.contentType != "application/json" {
		t.Errorf("request %s %s", r.path, r.contentType)
	}
	var got Payload
	if err := json.Unmarshal(r.body, &got); err != nil {
		t.Fatal(err)
	}
	if got.Title != sampleTitle || got.Server != "node1" || len(got.Transitions) != 3 || got.Time.IsZero() {
		t.Errorf("payload %+v", got)
	}
	if got.Transitions[2].Reason != "registry auth failed" || got.Transitions[1].Previous != "current" {
		t.Errorf("transition fields lost: %+v", got.Transitions)
	}
	order := make([]string, 0, len(got.Transitions))
	for _, tr := range got.Transitions {
		order = append(order, tr.Instance)
	}
	if want := "api web db"; strings.Join(order, " ") != want {
		t.Errorf("order %q, want updates by project and instance, then failures: %q", order, want)
	}
	if !strings.HasPrefix(got.Message, "• api (apps):") {
		t.Errorf("message not in the same order:\n%s", got.Message)
	}
}

func TestDiscordAndSlack(t *testing.T) {
	t.Parallel()
	var c capture
	ts := c.server(t)
	host := strings.TrimPrefix(ts.URL, "https://")
	tests := []struct {
		scheme, field, prefix string
	}{
		{"discord", "content", "**" + sampleTitle + "**\n• **api** (apps):"},
		{"slack", "text", "*" + sampleTitle + "*\n• *api* (apps):"},
	}
	for _, tt := range tests {
		sender(t, ts, nil, nil, tt.scheme+"://"+host+"/api/webhooks/1/secret").Notify(t.Context(), batch)
		r := c.last(t)
		var got map[string]string
		if err := json.Unmarshal(r.body, &got); err != nil {
			t.Fatal(err)
		}
		if r.path != "/api/webhooks/1/secret" || !strings.HasPrefix(got[tt.field], tt.prefix) {
			t.Errorf("%s: %s %q", tt.scheme, r.path, got)
		}
	}
}

func TestDiscordTruncates(t *testing.T) {
	t.Parallel()
	var c capture
	ts := c.server(t)
	many := make([]Transition, 200)
	for i := range many {
		many[i] = sample[0]
	}
	sender(t, ts, nil, nil, "discord://"+strings.TrimPrefix(ts.URL, "https://")+"/x").Notify(t.Context(), Batch{Transitions: many})
	var got map[string]string
	if err := json.Unmarshal(c.last(t).body, &got); err != nil {
		t.Fatal(err)
	}
	if n := len(got["content"]); n > discordLimit || !strings.HasSuffix(got["content"], "…") {
		t.Errorf("content %d bytes, ends %q", n, got["content"][len(got["content"])-10:])
	}
}

func TestFailureIsLoggedWithoutTheURL(t *testing.T) {
	t.Parallel()
	var c capture
	c.status = http.StatusBadGateway
	ts := c.server(t)
	var set metrics.Set
	var log bytes.Buffer
	host := strings.TrimPrefix(ts.URL, "https://")
	s := sender(t, ts, &set, &log, "discord://"+host+"/api/webhooks/1/s3cret-token")
	s.Notify(t.Context(), batch)
	if !strings.Contains(log.String(), "502") || strings.Contains(log.String(), "s3cret-token") {
		t.Errorf("log %s", log.String())
	}

	ts.Close()
	s.Notify(t.Context(), batch)
	if strings.Contains(log.String(), "s3cret-token") {
		t.Errorf("transport error leaked the URL: %s", log.String())
	}

	rec := httptest.NewRecorder()
	set.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	for _, want := range []string{
		`incus_beacon_notifications_total{notifier="discord",result="failed"} 2`,
		`incus_beacon_notifications_total{notifier="discord",result="sent"} 0`,
	} {
		if !strings.Contains(rec.Body.String(), want+"\n") {
			t.Errorf("missing %q in\n%s", want, rec.Body.String())
		}
	}
}

func TestBadURLsFailAtStart(t *testing.T) {
	t.Parallel()
	_, err := New(Options{URLs: []string{
		"https://example.com/ok",
		"ntfy://example.com/topic-secret",
		"discord:///no-host",
		"%zz://x",
	}})
	if err == nil {
		t.Fatal("bad URLs accepted")
	}
	msg := err.Error()
	for _, want := range []string{`#2: unknown scheme "ntfy"`, "#3: discord URL has no host", "#4: not a URL"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error %q lacks %q", msg, want)
		}
	}
	if strings.Contains(msg, "topic-secret") {
		t.Errorf("error repeats a URL: %q", msg)
	}
	if s, err := New(Options{}); err != nil || len(s.targets) != 0 {
		t.Errorf("no notifiers: %v, %d targets", err, len(s.targets))
	}
}

func TestCommand(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("uses a shell script")
	}
	dir := t.TempDir()
	out := filepath.Join(dir, "got.json")
	script := filepath.Join(dir, "notify.sh")
	body := "#!/bin/sh\ncat > \"$1\"\n[ \"$2\" = fail ] && { echo 'no route to host' >&2; exit 3; }\nexit 0\n"
	if err := os.WriteFile(script, []byte(body), 0o700); err != nil {
		t.Fatal(err)
	}

	var log bytes.Buffer
	s, err := New(Options{Command: script + " " + out, Timeout: 5 * time.Second, Log: zerolog.New(&log)})
	if err != nil {
		t.Fatal(err)
	}
	s.Notify(t.Context(), batch)
	b, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	var got Payload
	if err := json.Unmarshal(b, &got); err != nil || got.Title != sampleTitle || len(got.Transitions) != 3 {
		t.Errorf("stdin %s (%v)", b, err)
	}

	s, err = New(Options{Command: script + " " + out + " fail", Timeout: 5 * time.Second, Log: zerolog.New(&log)})
	if err != nil {
		t.Fatal(err)
	}
	s.Notify(t.Context(), batch)
	if !strings.Contains(log.String(), "exit status 3: no route to host") {
		t.Errorf("log %s", log.String())
	}
}

func TestTruncate(t *testing.T) {
	t.Parallel()
	if got := truncate("short", 10); got != "short" {
		t.Errorf("short: %q", got)
	}
	got := truncate(strings.Repeat("→", 10), 12)
	if len(got) > 12 || !strings.HasSuffix(got, "\n…") || !strings.HasPrefix(got, "→") {
		t.Errorf("multibyte cut: %q (%d bytes)", got, len(got))
	}
}
