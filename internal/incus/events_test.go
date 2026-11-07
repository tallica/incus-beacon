package incus

import (
	"context"
	"encoding/json"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/lxc/incus/v7/shared/api"
	"github.com/rs/zerolog"
)

func lifecycle(t *testing.T, projectName string, lc api.EventLifecycle) api.Event {
	t.Helper()
	meta, err := json.Marshal(lc)
	if err != nil {
		t.Fatal(err)
	}
	return api.Event{Type: api.EventTypeLifecycle, Project: projectName, Metadata: meta}
}

func operation(t *testing.T, projectName string, op api.Operation) api.Event {
	t.Helper()
	meta, err := json.Marshal(op)
	if err != nil {
		t.Fatal(err)
	}
	return api.Event{Type: api.EventTypeOperation, Project: projectName, Metadata: meta}
}

func rebuild(status api.StatusCode, resources ...string) api.Operation {
	return api.Operation{Description: rebuildDescription, StatusCode: status, Resources: map[string][]string{"instances": resources}}
}

func TestInstanceOf(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		ev   api.Event
		want Key
		ok   bool
	}{
		{"created", lifecycle(t, "apps", api.EventLifecycle{Action: api.EventLifecycleInstanceCreated, Name: "web"}), Key{"apps", "web"}, true},
		{"updated, named from the source", lifecycle(t, "", api.EventLifecycle{
			Action: api.EventLifecycleInstanceUpdated, Source: "/1.0/instances/web?project=default",
		}), Key{"default", "web"}, true},
		{"started", lifecycle(t, "apps", api.EventLifecycle{Action: api.EventLifecycleInstanceStarted, Name: "web"}), Key{}, false},
		{"snapshot source", lifecycle(t, "apps", api.EventLifecycle{
			Action: api.EventLifecycleInstanceUpdated, Source: "/1.0/instances/web/snapshots/s1",
		}), Key{}, false},
		{"rebuilt", operation(t, "apps", rebuild(api.Success, "/1.0/instances/web")), Key{"apps", "web"}, true},
		{"rebuilt in the default project", operation(t, "", rebuild(api.Success, "/1.0/instances/db")), Key{"default", "db"}, true},
		{"rebuild still running", operation(t, "apps", rebuild(api.Running, "/1.0/instances/web")), Key{}, false},
		{"other operation", operation(t, "apps", api.Operation{
			Description: "Restarting instance", StatusCode: api.Success,
			Resources: map[string][]string{"instances": {"/1.0/instances/web"}},
		}), Key{}, false},
		{"bad metadata", api.Event{Type: api.EventTypeLifecycle, Project: "apps", Metadata: []byte("{")}, Key{}, false},
		{"logging", api.Event{Type: api.EventTypeLogging, Metadata: []byte("{}")}, Key{}, false},
	}
	for _, c := range cases {
		got, ok := instanceOf(c.ev)
		if got != c.want || ok != c.ok {
			t.Errorf("%s: %+v, %t; want %+v, %t", c.name, got, ok, c.want, c.ok)
		}
	}
}

func TestFollowReconnects(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	streams := make(chan chan api.Event, 2)
	var opens atomic.Int32
	open := func(context.Context) (<-chan api.Event, func(), error) {
		if opens.Add(1) == 1 {
			return nil, nil, errors.New("unreachable")
		}
		ch := make(chan api.Event, 4)
		streams <- ch
		return ch, func() {}, nil
	}
	out := make(chan Key, 4)
	f := follower{open: open, projects: []string{"apps"}, out: out, log: zerolog.Nop(), retry: time.Millisecond, maxRetry: 4 * time.Millisecond}
	go f.run(ctx)

	first := <-streams
	first <- lifecycle(t, "default", api.EventLifecycle{Action: api.EventLifecycleInstanceCreated, Name: "skipped"})
	first <- lifecycle(t, "apps", api.EventLifecycle{Action: api.EventLifecycleInstanceCreated, Name: "web"})
	if k := receive(t, out); k != (Key{"apps", "web"}) {
		t.Fatalf("first stream sent %+v; the unwatched project should be left out", k)
	}
	close(first)

	second := <-streams
	second <- operation(t, "apps", rebuild(api.Success, "/1.0/instances/api"))
	if k := receive(t, out); k != (Key{"apps", "api"}) {
		t.Fatalf("after reconnecting: %+v", k)
	}
	if n := opens.Load(); n != 3 {
		t.Errorf("%d opens, want 3: a failure, a closed stream, the live one", n)
	}
}

func receive(t *testing.T, out <-chan Key) Key {
	t.Helper()
	select {
	case k := <-out:
		return k
	case <-time.After(5 * time.Second):
		t.Fatal("no key in 5s")
		return Key{}
	}
}

func TestTargetOfAndSeen(t *testing.T) {
	t.Parallel()
	f := fixture()
	srv := f.connect(t)
	db := Key{"default", "db"}

	t1, ok, err := TargetOf(t.Context(), srv, db, true)
	if err != nil || !ok {
		t.Fatalf("TargetOf(db): %t, %v", ok, err)
	}
	if t1.Ref != "docker.io/library/postgres:17" {
		t.Errorf("ref %q", t1.Ref)
	}
	seen := Seen{}
	if !seen.Changed(t1) {
		t.Error("an instance never seen should count as changed")
	}
	seen.Record(t1)

	set := func(k, v string) {
		for i := range f.instances {
			if f.instances[i].Name == db.Instance {
				f.instances[i].Config[k] = v
				f.instances[i].ExpandedConfig[k] = v
			}
		}
	}
	set("user.beacon.status", "current")
	set("user.beacon.since", "2026-11-07T10:00:00Z")
	t2, _, _ := TargetOf(t.Context(), srv, db, true)
	if seen.Changed(t2) {
		t.Error("a status write counted as a change")
	}
	set("volatile.base_image", fpMissing)
	t3, _, _ := TargetOf(t.Context(), srv, db, true)
	if !seen.Changed(t3) {
		t.Error("a rebuild onto another image did not count as a change")
	}

	if _, ok, err := TargetOf(t.Context(), srv, Key{"default", "sys"}, true); err != nil || ok {
		t.Errorf("non-OCI instance: %t, %v; want not a target", ok, err)
	}
	if _, _, err := TargetOf(t.Context(), srv, Key{"default", "gone"}, true); err == nil {
		t.Error("deleted instance: want an error")
	}
}
