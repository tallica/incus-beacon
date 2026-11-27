package install

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	incusclient "github.com/lxc/incus/v7/client"
	"github.com/lxc/incus/v7/shared/api"
	"github.com/rs/zerolog"

	"github.com/tallica/incus-beacon/internal/config"
)

func (f *fake) UpdateInstance(name string, put api.InstancePut, _ string) (incusclient.Operation, error) {
	if f.failUpdate != nil {
		return nil, f.failUpdate
	}
	if f.conflicts > 0 {
		f.conflicts--
		f.instances[name].Config["user.incus-beacon.status"] = "current"
		return nil, api.StatusErrorf(http.StatusPreconditionFailed, "ETag mismatch")
	}
	f.calls = append(f.calls, "update instance "+name)
	f.instances[name].InstancePut = put
	f.instances[name].Config = maps.Clone(put.Config)
	return op{}, nil
}

func (f *fake) GetCertificate(fp string) (*api.Certificate, string, error) {
	for _, c := range f.certs {
		if c.Fingerprint == fp {
			return &c, "", nil
		}
	}
	return nil, "", errNotFound
}

func (f *fake) UpdateCertificate(fp string, put api.CertificatePut, _ string) error {
	f.calls = append(f.calls, "update cert "+fp)
	if f.failCert != nil {
		return f.failCert
	}
	for i := range f.certs {
		if f.certs[i].Fingerprint == fp {
			f.certs[i].CertificatePut = put
		}
	}
	return nil
}

func configuredFake() *fake {
	f := installedFake()
	inst := f.instances["incus-beacon"]
	inst.Config["environment.BEACON_INCUS"] = "https://192.168.1.10:8443"
	inst.Devices[stateDev]["pool"] = "default"
	f.certs = []api.Certificate{
		{Fingerprint: "fp-incus-beacon", CertificatePut: api.CertificatePut{Name: "incus-beacon"}},
	}
	f.projects = []string{"default", "apps"}
	return f
}

func configureOpts() ConfigureOptions {
	return ConfigureOptions{Name: "incus-beacon", Wait: 50 * time.Millisecond, Log: zerolog.Nop()}
}

func TestConfigure(t *testing.T) {
	t.Parallel()
	f := configuredFake()
	o := configureOpts()
	o.Interval = new(time.Hour)
	o.Update = new(true)
	o.UpdateWindow = new("02:00-05:00")
	o.Watch = new("opt-in")
	var buf bytes.Buffer
	o.Audit = zerolog.New(&buf)
	if err := Configure(context.Background(), f, o); err != nil {
		t.Fatal(err)
	}
	for _, r := range checkAudit(t, &buf, "configure begin", "configure done") {
		if got := fmt.Sprint(r["settings"]); got != "[watch interval update update-window]" {
			t.Errorf("settings %s, want the four changed", got)
		}
	}
	s := serveFrom(t, f.instances["incus-beacon"])
	if s.Interval != time.Hour || !s.Update || s.UpdateWindow != "02:00-05:00" || s.Watch != "opt-in" {
		t.Errorf("serve after configure: %+v", s)
	}
	if !slices.Equal(f.states, []string{"stop", "start"}) {
		t.Errorf("states %v, want a restart", f.states)
	}
	if i, j := slices.Index(
		f.calls,
		"update instance incus-beacon",
	), slices.Index(
		f.calls,
		"exec "+strings.Join(readyProbe, " "),
	); i < 0 ||
		i > j {
		t.Errorf("calls %v, want the config written before the probe", f.calls)
	}
	if !slices.Contains(f.calls, "exec "+strings.Join(readyProbe, " ")) {
		t.Errorf("calls %v, want the /ready probe", f.calls)
	}
}

func TestConfigureRestoresDefaults(t *testing.T) {
	t.Parallel()
	f := configuredFake()
	cfg := f.instances["incus-beacon"].Config
	cfg["environment.BEACON_COOLDOWN"] = "48h"
	cfg["environment.BEACON_UPDATE"] = "true"
	cfg["environment.BEACON_UPDATE_WINDOW"] = "02:00-05:00"
	cfg["environment.BEACON_WATCH"] = "opt-in"
	o := configureOpts()
	o.Cooldown = new(time.Duration(0))
	o.Update = new(false)
	o.UpdateWindow = new("")
	o.Watch = new("all")
	if err := Configure(context.Background(), f, o); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"COOLDOWN", "UPDATE", "UPDATE_WINDOW", "WATCH"} {
		if v, ok := f.instances["incus-beacon"].Config[envPrefix+k]; ok {
			t.Errorf("BEACON_%s = %q, want it unset", k, v)
		}
	}
}

func TestConfigureNotifyURLs(t *testing.T) {
	t.Parallel()
	f := configuredFake()
	o := configureOpts()
	o.NotifyURLs = &[]string{"https://example.com/hook"}
	o.NotifyErrs = new(true)
	var buf bytes.Buffer
	o.Audit = zerolog.New(&buf)
	if err := Configure(context.Background(), f, o); err != nil {
		t.Fatal(err)
	}
	if r := checkAudit(
		t,
		&buf,
		"configure begin",
		"configure done",
	); fmt.Sprint(
		r[0]["settings"],
	) != "[notify-url notify-errors]" {
		t.Errorf("settings %v, want notify-url and notify-errors", r[0]["settings"])
	}
	if strings.Contains(buf.String(), "example.com") {
		t.Errorf("a notification URL reached the audit log: %s", buf.String())
	}
	if got := f.readVolumeFile(t, "/"+notifyFile); got != "https://example.com/hook\n" {
		t.Errorf("volume file %q", got)
	}
	if s := serveFrom(t, f.instances["incus-beacon"]); s.NotifyURLFile != stateDir+"/"+notifyFile || !s.NotifyErrors {
		t.Errorf("serve after configure: %+v", s)
	}

	o = configureOpts()
	o.NotifyURLs = &[]string{}
	err := Configure(context.Background(), f, o)
	if err == nil || !strings.Contains(err.Error(), "notify-errors needs") {
		t.Fatalf("clearing the URLs with notify-errors on: err %v, want serve's refusal", err)
	}
	o.NotifyErrs = new(false)
	if err := Configure(context.Background(), f, o); err != nil {
		t.Fatal(err)
	}
	if urls, err := readNotifyURLs(f, "default", "incus-beacon-state"); err != nil || urls != nil {
		t.Errorf("URLs %v %v, want the file removed", urls, err)
	}
	if v, ok := f.instances["incus-beacon"].Config[envPrefix+"NOTIFY_URL_FILE"]; ok {
		t.Errorf("BEACON_NOTIFY_URL_FILE = %q, want it unset", v)
	}
}

func TestConfigureRollsBack(t *testing.T) {
	t.Parallel()
	f := configuredFake()
	inst := f.instances["incus-beacon"]
	// Not ready on the new settings; meanwhile someone else sets a key, which the rollback keeps.
	f.ready = func(int) bool {
		f.instances["incus-beacon"].Config["user.other"] = "x"
		return len(f.states) > 2
	}
	if err := setNotifyURLs(f, "default", "incus-beacon-state", []string{"https://example.com/old"}); err != nil {
		t.Fatal(err)
	}
	inst.Config[envPrefix+"NOTIFY_URL_FILE"] = stateDir + "/" + notifyFile
	want := maps.Clone(inst.Config)
	want["user.other"] = "x"
	o := configureOpts()
	o.Interval = new(time.Hour)
	o.NotifyURLs = &[]string{"https://example.com/new"}
	o.Projects = &[]string{"apps"}
	var buf bytes.Buffer
	o.Audit = zerolog.New(&buf)
	err := Configure(context.Background(), f, o)
	if err == nil || !strings.Contains(err.Error(), "not ready within") ||
		!strings.Contains(err.Error(), "rolled back to the old settings, which are ready") {
		t.Fatalf("err %v, want not ready and rolled back", err)
	}
	if r := checkAudit(
		t,
		&buf,
		"configure begin",
		"configure.rollback begin",
		"configure.rollback done",
		"configure failed",
	); fmt.Sprint(
		r[0]["settings"],
	) != "[project notify-url interval]" {
		t.Errorf("settings %v, want project, notify-url and interval", r[0]["settings"])
	}
	if got := f.instances["incus-beacon"].Config; !maps.Equal(got, want) {
		t.Errorf("config %v, want %v", got, want)
	}
	if got := f.readVolumeFile(t, "/"+notifyFile); got != "https://example.com/old\n" {
		t.Errorf("volume file %q, want the old URL", got)
	}
	if c := f.certs[0]; c.Restricted || len(c.Projects) != 0 {
		t.Errorf("certificate %+v, want it unrestricted again", c)
	}
	if !slices.Equal(f.states, []string{"stop", "start", "stop", "start"}) {
		t.Errorf("states %v", f.states)
	}
}

func TestChangedNamesEveryFlagByItsKey(t *testing.T) {
	t.Parallel()
	f := configuredFake()
	inst := f.instances["incus-beacon"]
	o := configureOpts()
	o.APIURL = new("https://incus.example.com:8443")
	o.Projects = &[]string{"apps"}
	o.Watch = new("opt-in")
	o.Events = new(true)
	o.Track = new("digest")
	o.NotifyURLs = &[]string{"https://example.com/hook"}
	o.NotifyErrs = new(true)
	o.Interval = new(time.Hour)
	o.Cooldown = new(48 * time.Hour)
	o.Update = new(true)
	o.UpdateWindow = new("02:00-05:00")
	cfg, err := configured(f, inst, o)
	if err != nil {
		t.Fatal(err)
	}
	got, want := changed(inst.Config, cfg, false, false), config.SettingFlags()
	slices.Sort(got)
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Errorf("changed names %v, want every configure flag %v", got, want)
	}
}

func TestConfigureRollsBackAFailedWrite(t *testing.T) {
	t.Parallel()
	f := configuredFake()
	f.failCert = errors.New("certificate locked")
	o := configureOpts()
	o.Interval = new(time.Hour)
	o.Projects = &[]string{"apps"}
	var buf bytes.Buffer
	o.Audit = zerolog.New(&buf)
	if err := Configure(
		context.Background(),
		f,
		o,
	); err == nil ||
		!strings.Contains(err.Error(), "certificate locked") {
		t.Fatalf("err %v, want the certificate update's", err)
	}
	r := checkAudit(
		t,
		&buf,
		"configure begin",
		"configure.rollback begin",
		"configure.rollback failed",
		"configure failed",
	)
	if !strings.Contains(fmt.Sprint(r[2]["error"]), "certificate locked") {
		t.Errorf("rollback record %v, want the undo's error", r[2])
	}
	if _, ok := f.instances["incus-beacon"].Config[envPrefix+"INTERVAL"]; ok {
		t.Error("interval kept, want the instance keys undone")
	}
	if !slices.Equal(f.states, []string{"stop", "start"}) {
		t.Errorf("states %v, want it started again", f.states)
	}
}

func TestConfigureRollbackNotReady(t *testing.T) {
	t.Parallel()
	f := configuredFake()
	f.ready = func(int) bool { return false }
	o := configureOpts()
	o.Interval = new(time.Hour)
	err := Configure(context.Background(), f, o)
	if err == nil || !strings.Contains(err.Error(), "roll back: not ready within") {
		t.Fatalf("err %v, want the rollback reported not ready", err)
	}
}

func TestConfigureProjectsRestrictTheCertificate(t *testing.T) {
	t.Parallel()
	f := configuredFake()
	o := configureOpts()
	o.Projects = &[]string{"apps"}
	if err := Configure(context.Background(), f, o); err != nil {
		t.Fatal(err)
	}
	if c := f.certs[0]; !c.Restricted || !slices.Equal(c.Projects, []string{"apps"}) {
		t.Errorf("certificate %+v, want it restricted to apps", c)
	}
	if got := f.instances["incus-beacon"].Config[envPrefix+"PROJECT"]; got != "apps" {
		t.Errorf("BEACON_PROJECT = %q", got)
	}

	o.Projects = &[]string{}
	if err := Configure(context.Background(), f, o); err != nil {
		t.Fatal(err)
	}
	if c := f.certs[0]; c.Restricted || len(c.Projects) != 0 {
		t.Errorf("certificate %+v, want it unrestricted", c)
	}
}

func TestConfigureWithoutRestart(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		setup func(*fake, *ConfigureOptions)
		calls []string // must appear
	}{
		{"unchanged", func(f *fake, o *ConfigureOptions) { o.Interval = new(time.Duration(0)) }, nil},
		{
			"track only",
			func(f *fake, o *ConfigureOptions) { o.Track = new("digest") },
			[]string{"update instance incus-beacon"},
		},
		{"stopped", func(f *fake, o *ConfigureOptions) {
			f.instances["incus-beacon"].StatusCode = api.Stopped
			o.Interval = new(time.Hour)
		}, []string{"update instance incus-beacon"}},
	}
	for _, tt := range tests {
		f := configuredFake()
		o := configureOpts()
		tt.setup(f, &o)
		var buf bytes.Buffer
		o.Audit = zerolog.New(&buf)
		if err := Configure(context.Background(), f, o); err != nil {
			t.Errorf("%s: %v", tt.name, err)
			continue
		}
		if tt.calls == nil && buf.Len() > 0 {
			t.Errorf("%s: audited %s, want a no-op unrecorded", tt.name, buf.String())
		}
		if len(f.states) != 0 {
			t.Errorf("%s: states %v, want no restart", tt.name, f.states)
		}
		for _, c := range tt.calls {
			if !slices.Contains(f.calls, c) {
				t.Errorf("%s: calls %v, want %q", tt.name, f.calls, c)
			}
		}
		if tt.calls == nil &&
			slices.ContainsFunc(f.calls, func(c string) bool { return strings.HasPrefix(c, "update") }) {
			t.Errorf("%s: calls %v, want nothing written", tt.name, f.calls)
		}
	}
}

func TestConfigureTrackFollowsTheImage(t *testing.T) {
	t.Parallel()
	f := configuredFake()
	o := configureOpts()
	o.Track = new("")
	if err := Configure(context.Background(), f, o); err != nil {
		t.Fatal(err)
	}
	if got := f.instances["incus-beacon"].Config["user.incus-beacon.track"]; got != "semver" {
		t.Errorf("track %q, want semver for %s", got, oldImage)
	}
}

func TestConfigureRefuses(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		setup func(*fake, *ConfigureOptions)
		want  string
	}{
		{
			"missing",
			func(f *fake, o *ConfigureOptions) { delete(f.instances, "incus-beacon"); o.Interval = new(time.Hour) },
			"install it first",
		},
		{"unknown project", func(f *fake, o *ConfigureOptions) { o.Projects = &[]string{"nope"} }, "project nope"},
		{
			"no certificate",
			func(f *fake, o *ConfigureOptions) { f.certs = nil; o.Projects = &[]string{"apps"} },
			"no trusted certificate",
		},
		{"serve refuses", func(f *fake, o *ConfigureOptions) { o.NotifyErrs = new(true) }, "notify-errors needs"},
	}
	for _, tt := range tests {
		f := configuredFake()
		o := configureOpts()
		tt.setup(f, &o)
		err := Configure(context.Background(), f, o)
		if err == nil || !strings.Contains(err.Error(), tt.want) {
			t.Errorf("%s: err %v, want %q", tt.name, err, tt.want)
		}
		if len(f.states) != 0 ||
			slices.ContainsFunc(f.calls, func(c string) bool { return strings.HasPrefix(c, "update") }) {
			t.Errorf("%s: calls %v, states %v, want the instance untouched", tt.name, f.calls, f.states)
		}
	}
}

func TestConfigureRetriesAConcurrentWrite(t *testing.T) {
	t.Parallel()
	f := configuredFake()
	f.conflicts = 1
	o := configureOpts()
	o.Track = new("digest")
	if err := Configure(context.Background(), f, o); err != nil {
		t.Fatal(err)
	}
	cfg := f.instances["incus-beacon"].Config
	if cfg["user.incus-beacon.track"] != "digest" || cfg["user.incus-beacon.status"] != "current" {
		t.Errorf("config %v, want the new track beside the status written meanwhile", cfg)
	}
}
