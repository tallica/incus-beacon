package install

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	incusclient "github.com/lxc/incus/v7/client"
	"github.com/lxc/incus/v7/shared/api"
	"github.com/pkg/sftp"
	"github.com/rs/zerolog"
)

// fake implements the calls install makes; any other call panics on the nil
// embedded interface.
type fake struct {
	incusclient.InstanceServer

	httpsAddress string
	projects     []string
	instances    map[string]*api.Instance
	elsewhere    map[string]*api.Instance          // in other projects, until UseProject picks one
	volumes      map[string]api.StorageVolumesPost // pool/name
	certs        []api.Certificate
	tokenOps     map[string]api.CertificatesPost
	files        map[string]incusclient.InstanceFileArgs
	fileData     map[string]string
	states       []string
	failCreate   error
	joins        bool // the instance redeems its token when started
	calls        []string
	volumeFS     sftp.Handlers // every volume's files, in memory
	snapshots    []string
	rebuilds     []api.InstanceSource
	ready        func(rebuilds int) bool // the /ready probe's answer after that many rebuilds
}

func newFake() *fake {
	return &fake{
		httpsAddress: ":8443",
		instances:    map[string]*api.Instance{},
		volumes:      map[string]api.StorageVolumesPost{},
		tokenOps:     map[string]api.CertificatesPost{},
		files:        map[string]incusclient.InstanceFileArgs{},
		fileData:     map[string]string{},
		joins:        true,
		volumeFS:     sftp.InMemHandler(),
	}
}

func (f *fake) GetStoragePoolVolumeFileSFTP(_, _, vol string) (*sftp.Client, error) {
	f.calls = append(f.calls, "sftp "+vol)
	server, client := net.Pipe()
	go func() { _ = sftp.NewRequestServer(server, f.volumeFS).Serve() }()
	return sftp.NewClientPipe(client, client)
}

func (f *fake) readVolumeFile(t *testing.T, path string) string {
	t.Helper()
	c, err := f.GetStoragePoolVolumeFileSFTP("", "", "")
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	r, err := c.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	b, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

var errNotFound = api.StatusErrorf(http.StatusNotFound, "not found")

func (f *fake) UseProject(p string) incusclient.InstanceServer {
	for name, inst := range f.elsewhere {
		if inst.Project == p {
			f.instances[name] = inst
			delete(f.elsewhere, name)
		}
	}
	return f
}

func (f *fake) GetServer() (*api.Server, string, error) {
	return &api.Server{ServerPut: api.ServerPut{Config: map[string]string{"core.https_address": f.httpsAddress}}}, "", nil
}

func (f *fake) GetProfile(string) (*api.Profile, string, error) {
	return &api.Profile{ProfilePut: api.ProfilePut{Devices: map[string]map[string]string{
		"root": {"type": "disk", "path": "/", "pool": "default"},
		"eth0": {"type": "nic", "network": "incusbr0", "name": "eth0"},
	}}}, "", nil
}

func (f *fake) GetNetwork(name string) (*api.Network, string, error) {
	return &api.Network{NetworkPut: api.NetworkPut{Config: map[string]string{"ipv4.address": "192.168.1.10/24"}}}, "", nil
}

func (f *fake) GetProject(name string) (*api.Project, string, error) {
	if !slices.Contains(f.projects, name) {
		return nil, "", errNotFound
	}
	return &api.Project{Name: name}, "", nil
}

func (f *fake) GetInstance(name string) (*api.Instance, string, error) {
	if inst, ok := f.instances[name]; ok {
		return inst, "", nil
	}
	return nil, "", errNotFound
}

func (f *fake) GetInstancesAllProjects(api.InstanceType) ([]api.Instance, error) {
	all := make([]api.Instance, 0, len(f.elsewhere)+len(f.instances))
	for _, inst := range f.elsewhere {
		all = append(all, *inst)
	}
	for _, inst := range f.instances {
		all = append(all, *inst)
	}
	return all, nil
}

func (f *fake) GetCertificates() ([]api.Certificate, error) { return f.certs, nil }

func (f *fake) DeleteCertificate(fp string) error {
	f.calls = append(f.calls, "delete cert "+fp)
	f.certs = slices.DeleteFunc(f.certs, func(c api.Certificate) bool { return c.Fingerprint == fp })
	return nil
}

func (f *fake) CreateStoragePoolVolume(pool string, v api.StorageVolumesPost) error {
	f.calls = append(f.calls, "create volume "+v.Name)
	f.volumes[pool+"/"+v.Name] = v
	return nil
}

func (f *fake) DeleteStoragePoolVolume(pool, _, name string) error {
	if _, ok := f.volumes[pool+"/"+name]; !ok {
		return errNotFound
	}
	f.calls = append(f.calls, "delete volume "+name)
	delete(f.volumes, pool+"/"+name)
	return nil
}

func (f *fake) CreateCertificateToken(req api.CertificatesPost) (incusclient.Operation, error) {
	f.calls = append(f.calls, "create token")
	id := "tok-" + req.Name
	f.tokenOps[id] = req
	tok := api.CertificateAddToken{ClientName: req.Name, Fingerprint: "ab", Addresses: []string{"192.168.1.10:8443"}, Secret: "s"}
	return op{meta: tokenOp(id, tok)}, nil
}

func tokenOp(id string, tok api.CertificateAddToken) api.Operation {
	return api.Operation{ID: id, Class: api.OperationClassToken, StatusCode: api.Running, Metadata: map[string]any{
		"request":     map[string]any{"name": tok.ClientName},
		"fingerprint": tok.Fingerprint,
		"addresses":   []any{tok.Addresses[0]},
		"secret":      tok.Secret,
	}}
}

func (f *fake) GetOperations() ([]api.Operation, error) {
	ops := make([]api.Operation, 0, len(f.tokenOps))
	for id, req := range f.tokenOps {
		ops = append(ops, tokenOp(id, api.CertificateAddToken{ClientName: req.Name, Fingerprint: "ab", Addresses: []string{"a"}, Secret: "s"}))
	}
	return ops, nil
}

func (f *fake) DeleteOperation(id string) error {
	f.calls = append(f.calls, "delete token")
	delete(f.tokenOps, id)
	return nil
}

func (f *fake) CreateInstance(req api.InstancesPost) (incusclient.Operation, error) {
	if f.failCreate != nil {
		return nil, f.failCreate
	}
	f.calls = append(f.calls, "create instance "+req.Name)
	f.instances[req.Name] = &api.Instance{Name: req.Name, StatusCode: api.Stopped, InstancePut: req.InstancePut, ExpandedDevices: req.Devices}
	f.instances[req.Name].Devices = req.Devices
	return op{}, nil
}

func (f *fake) DeleteInstance(name string) (incusclient.Operation, error) {
	f.calls = append(f.calls, "delete instance "+name)
	delete(f.instances, name)
	return op{}, nil
}

func (f *fake) CreateInstanceFile(_, path string, args incusclient.InstanceFileArgs) error {
	f.files[path] = args
	if args.Content != nil {
		b, _ := io.ReadAll(args.Content)
		f.fileData[path] = string(b)
	}
	return nil
}

func (f *fake) DeleteInstanceFile(_, path string) error {
	f.calls = append(f.calls, "delete file "+path)
	return nil
}

func (f *fake) UpdateInstanceState(name string, st api.InstanceStatePut, _ string) (incusclient.Operation, error) {
	f.states = append(f.states, st.Action)
	if st.Action == "start" && f.joins {
		for id, req := range f.tokenOps {
			f.certs = append(f.certs, api.Certificate{Name: req.Name, Fingerprint: "fp-" + req.Name})
			delete(f.tokenOps, id)
		}
	}
	return op{}, nil
}

type op struct {
	incusclient.Operation
	meta api.Operation
}

func (o op) Get() api.Operation                { return o.meta }
func (o op) Wait() error                       { return nil }
func (o op) WaitContext(context.Context) error { return nil }

func opts() Options {
	return Options{
		Name:  "incus-beacon",
		Image: "ghcr.io/tallica/incus-beacon:0.1",
		Watch: "all",
		Wait:  3 * time.Second,
		Log:   zerolog.Nop(),
	}
}

func TestInstall(t *testing.T) {
	t.Parallel()
	f := newFake()
	f.projects = []string{"apps"}
	o := opts()
	o.Projects = []string{"apps"}
	o.Events = true
	if err := Install(context.Background(), f, o); err != nil {
		t.Fatal(err)
	}
	vol, ok := f.volumes["default/incus-beacon-state"]
	if !ok || vol.Config["initial.uid"] != "65534" || vol.Config["initial.gid"] != "65534" {
		t.Errorf("volume %+v, want one owned by 65534", vol)
	}
	inst := f.instances["incus-beacon"]
	if inst == nil {
		t.Fatal("no instance created")
	}
	wantEnv := map[string]string{
		"environment.BEACON_INCUS":      "https://192.168.1.10:8443",
		"environment.BEACON_TOKEN_FILE": tokenFile,
		"environment.BEACON_PROJECT":    "apps",
		"environment.BEACON_EVENTS":     "true",
		"user.beacon.track":             "semver",
	}
	for k, v := range wantEnv {
		if inst.Config[k] != v {
			t.Errorf("%s = %q, want %q", k, inst.Config[k], v)
		}
	}
	if _, ok := inst.Config["environment.BEACON_TOKEN"]; ok {
		t.Error("the token is in the instance config")
	}
	if d := inst.Devices[stateDev]; d["source"] != "incus-beacon-state" || d["path"] != stateDir {
		t.Errorf("state device %v", d)
	}
	if a := f.files[tokenFile]; a.Mode != 0o400 || a.UID != imageUID || f.fileData[tokenFile] == "" {
		t.Errorf("token file %+v", a)
	}
	if !slices.Equal(f.states, []string{"start"}) {
		t.Errorf("states %v, want a start", f.states)
	}
	if !slices.Contains(f.calls, "delete file "+tokenFile) {
		t.Error("spent token file not removed")
	}
}

func TestInstallStoresNotifyURLsInTheVolume(t *testing.T) {
	t.Parallel()
	f := newFake()
	o := opts()
	o.NotifyURLs = []string{"https://example.com/hook", "slack://hooks.example.com/services/T0K3N"}
	o.NotifyErrs = true
	if err := Install(context.Background(), f, o); err != nil {
		t.Fatal(err)
	}
	if got, want := f.readVolumeFile(t, "/"+notifyFile), "https://example.com/hook\nslack://hooks.example.com/services/T0K3N\n"; got != want {
		t.Errorf("volume file %q, want %q", got, want)
	}
	cfg := f.instances["incus-beacon"].Config
	if cfg["environment.BEACON_NOTIFY_URL_FILE"] != stateDir+"/"+notifyFile || cfg["environment.BEACON_NOTIFY_ERRORS"] != "true" {
		t.Errorf("config %v", cfg)
	}
	for k, v := range cfg {
		if strings.Contains(v, "T0K3N") {
			t.Errorf("%s holds a notification URL", k)
		}
	}
	if i, j := slices.Index(f.calls, "sftp incus-beacon-state"), slices.Index(f.calls, "create instance incus-beacon"); i < 0 || i > j {
		t.Errorf("calls %v, want the file written before the instance exists", f.calls)
	}

	f = newFake()
	if err := Install(context.Background(), f, opts()); err != nil {
		t.Fatal(err)
	}
	if slices.ContainsFunc(f.calls, func(c string) bool { return strings.HasPrefix(c, "sftp") }) {
		t.Error("volume opened with no notification URLs")
	}
	if _, ok := f.instances["incus-beacon"].Config["environment.BEACON_NOTIFY_URL_FILE"]; ok {
		t.Error("URL file set with no notification URLs")
	}
}

func TestInstallRestrictsTokenToProjects(t *testing.T) {
	t.Parallel()
	f := newFake()
	f.projects = []string{"apps", "web"}
	f.joins = false
	o := opts()
	o.Projects = []string{"apps", "web"}
	o.Wait = 10 * time.Millisecond
	_ = Install(context.Background(), f, o)
	for _, req := range f.tokenOps {
		if !req.Restricted || !slices.Equal(req.Projects, o.Projects) {
			t.Errorf("token request %+v, want restricted to %v", req, o.Projects)
		}
	}
}

func TestInstallRefusesExisting(t *testing.T) {
	t.Parallel()
	f := newFake()
	f.instances["incus-beacon"] = &api.Instance{Name: "incus-beacon"}
	if err := Install(context.Background(), f, opts()); err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Errorf("existing instance: err %v", err)
	}
	f = newFake()
	f.certs = []api.Certificate{{Name: "incus-beacon"}}
	if err := Install(context.Background(), f, opts()); err == nil || !strings.Contains(err.Error(), "already named") {
		t.Errorf("existing certificate: err %v", err)
	}
	if len(f.calls) > 0 {
		t.Errorf("changed something: %v", f.calls)
	}
}

func TestInstallMissingProjectCreatesNothing(t *testing.T) {
	t.Parallel()
	f := newFake()
	o := opts()
	o.Projects = []string{"nope"}
	if err := Install(context.Background(), f, o); err == nil || !strings.Contains(err.Error(), "project nope") {
		t.Errorf("err %v", err)
	}
	if len(f.calls) > 0 {
		t.Errorf("changed something: %v", f.calls)
	}
}

func TestInstallRollsBack(t *testing.T) {
	t.Parallel()
	f := newFake()
	f.failCreate = errors.New("image not found")
	err := Install(context.Background(), f, opts())
	if err == nil || !strings.Contains(err.Error(), "image not found") {
		t.Fatalf("err %v", err)
	}
	want := []string{"create volume incus-beacon-state", "create token", "delete token", "delete volume incus-beacon-state"}
	if !slices.Equal(f.calls, want) {
		t.Errorf("calls %v, want %v", f.calls, want)
	}
}

func TestInstallKeepsInstanceThatDidNotJoin(t *testing.T) {
	t.Parallel()
	f := newFake()
	f.joins = false
	o := opts()
	o.Wait = 10 * time.Millisecond
	if err := Install(context.Background(), f, o); err == nil || !strings.Contains(err.Error(), "did not join") {
		t.Fatalf("err %v", err)
	}
	if f.instances["incus-beacon"] == nil {
		t.Error("instance removed; its log is what explains the failure")
	}
}

func TestUninstall(t *testing.T) {
	t.Parallel()
	f := newFake()
	f.projects = []string{"apps"}
	f.joins = false
	o := opts()
	o.Wait = 10 * time.Millisecond
	_ = Install(context.Background(), f, o)
	f.instances["incus-beacon"].StatusCode = api.Running
	f.certs = []api.Certificate{{Name: "incus-beacon", Fingerprint: "mine"}, {Name: "someone-else", Fingerprint: "theirs"}}
	f.calls = nil

	if err := Uninstall(context.Background(), f, "incus-beacon", zerolog.Nop()); err != nil {
		t.Fatal(err)
	}
	want := []string{"delete instance incus-beacon", "delete volume incus-beacon-state", "delete token", "delete cert mine"}
	if !slices.Equal(f.calls, want) {
		t.Errorf("calls %v, want %v", f.calls, want)
	}
	if !slices.Contains(f.states, "stop") {
		t.Error("running instance not stopped first")
	}
	if len(f.certs) != 1 || f.certs[0].Name != "someone-else" {
		t.Errorf("certs left %v", f.certs)
	}
}

func TestUninstallNothingThere(t *testing.T) {
	t.Parallel()
	if err := Uninstall(context.Background(), newFake(), "incus-beacon", zerolog.Nop()); err != nil {
		t.Errorf("err %v", err)
	}
}

func TestUninstallFindsInstanceInAnotherProject(t *testing.T) {
	t.Parallel()
	f := newFake()
	f.elsewhere = map[string]*api.Instance{"incus-beacon": {Name: "incus-beacon", Project: "apps", StatusCode: api.Stopped}}
	f.certs = []api.Certificate{{Name: "incus-beacon", Fingerprint: "mine"}}
	if err := Uninstall(context.Background(), f, "incus-beacon", zerolog.Nop()); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(f.calls, "delete instance incus-beacon") || len(f.certs) != 0 {
		t.Errorf("calls %v, certs %v: want the instance and certificate gone", f.calls, f.certs)
	}
}

func TestTrackFor(t *testing.T) {
	t.Parallel()
	tests := []struct{ track, image, want string }{
		{"", "ghcr.io/tallica/incus-beacon:0.4", "semver"},
		{"", "ghcr.io/tallica/incus-beacon:v1.2.3-alpine", "semver"},
		{"", "ghcr.io/tallica/incus-beacon:latest", "digest"},
		{"", "ghcr.io/tallica/incus-beacon", "digest"},
		{"", "ghcr.io/tallica/incus-beacon@sha256:" + strings.Repeat("a", 64), "digest"},
		{"digest", "ghcr.io/tallica/incus-beacon:0.4", "digest"},
		{"semver", "ghcr.io/tallica/incus-beacon:latest", "semver"},
	}
	for _, tt := range tests {
		if got := trackFor(tt.track, tt.image); got != tt.want {
			t.Errorf("trackFor(%q, %q) = %q, want %q", tt.track, tt.image, got, tt.want)
		}
	}
}

func TestDefaultImage(t *testing.T) {
	t.Parallel()
	for v, want := range map[string]string{
		"0.1.2":                  "ghcr.io/tallica/incus-beacon:0.1",
		"v1.4.0":                 "ghcr.io/tallica/incus-beacon:1.4",
		"v0.1.2-3-gabc123-dirty": "ghcr.io/tallica/incus-beacon:latest",
		"dev":                    "ghcr.io/tallica/incus-beacon:latest",
	} {
		got, released := DefaultImage(v)
		if got != want || released == strings.HasSuffix(want, ":latest") {
			t.Errorf("DefaultImage(%q) = %s, %t; want %s", v, got, released, want)
		}
	}
}

func TestSplitImage(t *testing.T) {
	t.Parallel()
	server, alias, err := splitImage("ghcr.io/tallica/incus-beacon:0.1")
	if err != nil || server != "https://ghcr.io" || alias != "tallica/incus-beacon:0.1" {
		t.Errorf("got %s %s %v", server, alias, err)
	}
	if _, _, err := splitImage("tallica/incus-beacon"); err == nil {
		t.Error("a ref without a registry was accepted")
	}
}

func TestDetectURL(t *testing.T) {
	t.Parallel()
	tests := []struct{ addr, want string }{
		{":8443", "https://192.168.1.10:8443"},
		{"[::]:9443", "https://192.168.1.10:9443"},
		{"incus.example.com:8443", "https://incus.example.com:8443"},
		{"incus.example.com", "https://incus.example.com:8443"},
	}
	for _, tt := range tests {
		f := newFake()
		f.httpsAddress = tt.addr
		got, err := detectURL(f, "incusbr0")
		if err != nil || got != tt.want {
			t.Errorf("%q: got %s %v, want %s", tt.addr, got, err, tt.want)
		}
	}
	f := newFake()
	f.httpsAddress = ""
	if _, err := detectURL(f, "incusbr0"); err == nil || !strings.Contains(err.Error(), "--api-url") {
		t.Errorf("unset address: err %v", err)
	}
}
