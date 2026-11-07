// Package notify tells people when an instance's update status changes.
package notify

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/rs/zerolog"

	"github.com/tallica/incus-beacon/internal/incus"
	"github.com/tallica/incus-beacon/internal/metrics"
)

const (
	statusUpdate = "update-available"
	statusError  = "error"
)

type Transition struct {
	Project  string `json:"project"`
	Instance string `json:"instance"`
	Image    string `json:"image"`
	Status   string `json:"status"`
	Previous string `json:"previous_status,omitempty"`
	Latest   string `json:"latest,omitempty"`
	Reason   string `json:"reason,omitempty"`
}

// Notable follows docs/ARCHITECTURE.md#notifications.
func Notable(prev, next incus.Beacon, errors bool) bool {
	switch next.Status {
	case statusUpdate:
		return prev.Status != statusUpdate || prev.Latest != next.Latest
	case statusError:
		return errors && prev.Status != statusError
	}
	return false
}

func NewTransition(t incus.Target, next incus.Beacon) Transition {
	return Transition{
		Project:  t.Project,
		Instance: t.Instance,
		Image:    next.Current,
		Status:   next.Status,
		Previous: t.Beacon.Status,
		Latest:   next.Latest,
		Reason:   next.Error,
	}
}

// Batch is what one check found worth sending.
type Batch struct {
	Server      string // the Incus server_name; empty when unknown
	Transitions []Transition
}

// style is a chat service's markup; code also keeps refs from becoming links.
type style struct {
	bold, code func(string) string
}

func wrap(mark string) func(string) string {
	return func(s string) string { return mark + s + mark }
}

var (
	plain        = style{bold: wrap(""), code: wrap("")}
	slackStyle   = style{bold: wrap("*"), code: wrap("`")}
	discordStyle = style{bold: wrap("**"), code: wrap("`")}
)

func Message(b Batch) (title, body string) { return message(b, plain) }

func message(b Batch, st style) (title, body string) {
	updates, failing := 0, 0
	var w strings.Builder
	for _, t := range b.Transitions {
		fmt.Fprintf(&w, "• %s (%s): %s", st.bold(t.Instance), t.Project, st.code(shortRef(t.Image)))
		switch {
		case t.Status != statusUpdate:
			failing++
			fmt.Fprintf(&w, " check failed: %s\n", t.Reason)
		case strings.HasPrefix(t.Latest, tagOf(t.Image)+"@"):
			updates++
			w.WriteString(" rebuilt\n")
		default:
			updates++
			fmt.Fprintf(&w, " → %s\n", st.code(t.Latest))
		}
	}
	var parts []string
	if updates > 0 {
		parts = append(parts, plural(updates, "update", "updates")+" available")
	}
	if failing > 0 {
		parts = append(parts, plural(failing, "check", "checks")+" failing")
	}
	title = "incus-beacon"
	if b.Server != "" {
		title += " on " + b.Server
	}
	return title + ": " + strings.Join(parts, ", "), strings.TrimSuffix(w.String(), "\n")
}

// sorted puts updates before failing checks, as the title counts them.
func sorted(ts []Transition) []Transition {
	out := slices.Clone(ts)
	slices.SortStableFunc(out, func(a, b Transition) int {
		return cmp.Or(
			cmp.Compare(rank(a), rank(b)),
			strings.Compare(a.Project, b.Project),
			strings.Compare(a.Instance, b.Instance),
		)
	})
	return out
}

func rank(t Transition) int {
	if t.Status == statusUpdate {
		return 0
	}
	return 1
}

// shortRef drops Docker Hub's implied parts, as docker does: nginx:1.27.
func shortRef(ref string) string {
	if rest, ok := strings.CutPrefix(ref, "docker.io/"); ok {
		return strings.TrimPrefix(rest, "library/")
	}
	return ref
}

// tagOf is the tag after the last colon, unless that colon is a registry port.
func tagOf(ref string) string {
	i := strings.LastIndexByte(ref, ':')
	if i < 0 || strings.Contains(ref[i:], "/") {
		return ""
	}
	return ref[i+1:]
}

func plural(n int, one, many string) string {
	if n == 1 {
		return "1 " + one
	}
	return fmt.Sprintf("%d %s", n, many)
}

type Notifier interface {
	Notify(ctx context.Context, b Batch) error
}

// Factory builds a notifier from a --notify-url whose scheme is already https.
type Factory func(u *url.URL, client *http.Client) (Notifier, error)

// factories maps a --notify-url scheme to its sender; adding one adds a service.
var factories = map[string]Factory{
	"discord": newDiscord,
	"slack":   newSlack,
}

type Options struct {
	URLs    []string
	Command string
	Client  *http.Client  // http.DefaultClient when nil
	Timeout time.Duration // per notifier and message; defaultTimeout when zero
	Metrics *metrics.Set  // nil records nothing
	Log     zerolog.Logger
}

const defaultTimeout = 30 * time.Second

type target struct {
	kind  string
	where string // never the full URL: paths and queries hold tokens
	n     Notifier
}

type Sender struct {
	targets []target
	timeout time.Duration
	log     zerolog.Logger
	sent    *metrics.Counter
}

// New fails on a bad URL, so serve stops at start rather than at an update.
func New(opts Options) (*Sender, error) {
	client := opts.Client
	if client == nil {
		client = http.DefaultClient
	}
	s := &Sender{timeout: cmp.Or(opts.Timeout, defaultTimeout), log: opts.Log}
	var errs []error
	for i, raw := range opts.URLs {
		t, err := parse(raw, client)
		if err != nil {
			errs = append(errs, fmt.Errorf("notify-url #%d: %w", i+1, err))
			continue
		}
		s.targets = append(s.targets, t)
	}
	if opts.Command != "" {
		c, err := newCommand(opts.Command)
		if err != nil {
			errs = append(errs, fmt.Errorf("notify-command: %w", err))
		} else {
			s.targets = append(s.targets, target{kind: "command", where: c.argv[0], n: c})
		}
	}
	if err := errors.Join(errs...); err != nil {
		return nil, err
	}
	if opts.Metrics != nil {
		s.sent = opts.Metrics.Counter("incus_beacon_notifications_total",
			"Notification messages by notifier kind and result: sent, or failed.", "notifier", "result")
		for _, t := range s.targets {
			s.sent.Add(0, t.kind, "sent")
			s.sent.Add(0, t.kind, "failed")
		}
	}
	return s, nil
}

func parse(raw string, client *http.Client) (target, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return target{}, errors.New("not a URL") // url.Error would repeat the URL
	}
	if u.Host == "" {
		return target{}, fmt.Errorf("%s URL has no host", u.Scheme)
	}
	where := u.Scheme + "://" + u.Host
	switch u.Scheme {
	case "http", "https":
		return target{kind: "webhook", where: where, n: &webhook{url: u, client: client}}, nil
	}
	f, ok := factories[u.Scheme]
	if !ok {
		return target{}, fmt.Errorf("unknown scheme %q", u.Scheme)
	}
	https := *u
	https.Scheme = "https"
	n, err := f(&https, client)
	if err != nil {
		return target{}, fmt.Errorf("%s: %w", where, err)
	}
	return target{kind: u.Scheme, where: where, n: n}, nil
}

func (s *Sender) Notify(ctx context.Context, b Batch) {
	ts := b.Transitions
	if len(s.targets) == 0 || len(ts) == 0 {
		return
	}
	b.Transitions = sorted(ts)
	for _, t := range s.targets {
		nctx, cancel := context.WithTimeout(ctx, s.timeout)
		err := t.n.Notify(nctx, b)
		cancel()
		result := "sent"
		if err != nil {
			result = "failed"
			s.log.Error().Err(err).Str("notifier", t.kind).Str("to", t.where).Int("transitions", len(ts)).Msg("notification failed")
		} else {
			s.log.Info().Str("notifier", t.kind).Str("to", t.where).Int("transitions", len(ts)).Msg("notification sent")
		}
		if s.sent != nil {
			s.sent.Add(1, t.kind, result)
		}
	}
}
