package incus

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/lxc/incus/v7/shared/api"
	"github.com/rs/zerolog"
)

// Seen remembers each checked instance's Inputs and status keys; one goroutine owns it.
type Seen map[Key]seen

type seen struct {
	inputs string
	beacon Beacon
}

func (s Seen) Changed(t Target) bool {
	v, ok := s[t.Key()]
	return !ok || v.inputs != t.Inputs || v.beacon != t.Beacon
}

func (s Seen) Record(t Target) { s[t.Key()] = seen{inputs: t.Inputs, beacon: t.Beacon} }

func (s Seen) Wrote(k Key, b Beacon) {
	if v, ok := s[k]; ok {
		v.beacon = b
		s[k] = v
	}
}

// Incus sends a rebuild only as an operation, under this description.
const rebuildDescription = "Rebuilding instance"

const (
	followRetry    = time.Second
	followMaxRetry = time.Minute
)

type opener func(ctx context.Context) (events <-chan api.Event, stop func(), err error)

type follower struct {
	open     opener
	projects []string
	out      chan<- Key
	log      zerolog.Logger
	retry    time.Duration
	maxRetry time.Duration
}

// Follow sends created, updated and rebuilt instances to out until ctx is done.
func Follow(ctx context.Context, conn *Conn, projects []string, out chan<- Key, log zerolog.Logger) {
	open := func(ctx context.Context) (<-chan api.Event, func(), error) {
		srv, err := conn.Get(ctx)
		if err != nil {
			return nil, nil, err
		}
		l, err := srv.GetEventsAllProjectsByType([]string{api.EventTypeLifecycle, api.EventTypeOperation})
		if err != nil {
			return nil, nil, err
		}
		return l.AddChannel(nil, 0), l.Disconnect, nil
	}
	f := follower{open: open, projects: projects, out: out, log: log, retry: followRetry, maxRetry: followMaxRetry}
	f.run(ctx)
}

func (f follower) run(ctx context.Context) {
	wait, failing := f.retry, false
	for {
		events, stop, err := f.open(ctx)
		if err == nil {
			f.log.Info().Msg("following events")
			wait, failing = f.retry, false
			f.forward(ctx, events)
			stop()
			err = errors.New("event stream closed")
		}
		if ctx.Err() != nil {
			return
		}
		ev := f.log.Debug()
		if !failing {
			ev = f.log.Warn()
		}
		ev.Err(err).Stringer("retry_in", wait).Msg("not following events; the interval still applies")
		failing = true
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
		wait = min(wait*2, f.maxRetry)
	}
}

func (f follower) forward(ctx context.Context, events <-chan api.Event) {
	for {
		select {
		case <-ctx.Done():
			return
		case ev, ok := <-events:
			if !ok {
				return
			}
			k, ok := instanceOf(ev)
			if !ok || (len(f.projects) > 0 && !slices.Contains(f.projects, k.Project)) {
				continue
			}
			f.log.Debug().Str("project", k.Project).Str("instance", k.Instance).Str("event", ev.Type).Msg("event")
			select {
			case f.out <- k:
			case <-ctx.Done():
				return
			}
		}
	}
}

// instanceOf names the instance an event created, updated or rebuilt.
func instanceOf(ev api.Event) (Key, bool) {
	var name string
	switch ev.Type {
	case api.EventTypeLifecycle:
		var lc api.EventLifecycle
		if json.Unmarshal(ev.Metadata, &lc) != nil {
			return Key{}, false
		}
		if lc.Action != api.EventLifecycleInstanceCreated && lc.Action != api.EventLifecycleInstanceUpdated {
			return Key{}, false
		}
		name = cmp.Or(lc.Name, instanceName(lc.Source))
	case api.EventTypeOperation:
		var op api.Operation
		if json.Unmarshal(ev.Metadata, &op) != nil {
			return Key{}, false
		}
		if op.Description != rebuildDescription || op.StatusCode != api.Success || len(op.Resources["instances"]) != 1 {
			return Key{}, false
		}
		name = instanceName(op.Resources["instances"][0])
	default:
		return Key{}, false
	}
	if name == "" {
		return Key{}, false
	}
	return Key{Project: project(ev.Project), Instance: name}, true
}

// instanceName reads <name> from /1.0/instances/<name>[?project=...].
func instanceName(source string) string {
	u, err := url.Parse(source)
	if err != nil {
		return ""
	}
	name, ok := strings.CutPrefix(u.Path, "/1.0/instances/")
	if !ok || name == "" || strings.Contains(name, "/") {
		return ""
	}
	return name
}
