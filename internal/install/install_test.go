package install

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
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

	"github.com/tallica/incus-beacon/internal/config"
	"github.com/tallica/incus-beacon/internal/log"
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
	failUpdate   error // every instance update's
	conflicts    int   // instance updates refused with 412, as when serve writes its status keys meanwhile
	failCert     error // every certificate update's
	failList     error // every certificate listing's once the instance has started
	lists        int   // certificate listings once the instance has started
	joins        bool  // the instance redeems its token when started
	calls        []string
	volumeFS     sftp.Handlers // every volume's files, in memory
	snapshots    []string
	rebuilds     []api.InstanceSource
	failRebuild  error                   // the first rebuild's
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
	return &api.Server{
		Config: map[string]string{"core.https_address": f.httpsAddress},
	}, "", nil
}

func (f *fake) GetProfile(string) (*api.Profile, string, error) {
	return &api.Profile{Devices: map[string]map[string]string{
		"root": {"type": "disk", "path": "/", "pool": "default"},
		"eth0": {"type": "nic", "network": "incusbr0", "name": "eth0"},
	}}, "", nil
}

func (f *fake) GetNetwork(name string) (*api.Network, string, error) {
	return &api.Network{
		Config: map[string]string{"ipv4.address": "192.168.1.10/24"},
	}, "", nil
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

func (f *fake) GetCertificates() ([]api.Certificate, error) {
	if slices.Contains(f.states, "start") {
		f.lists++
		return f.certs, f.failList
	}
	return f.certs, nil
}

func (f *fake) DeleteCertificate(fp string) error {
	f.calls = append(f.calls, "delete cert "+fp)
	f.certs = slices.DeleteFunc(f.certs, func(c api.Certificate) bool { return c.Fingerprint == fp })
	return nil
}

func (f *fake) CreateStoragePoolVolume(pool string, v api.StorageVolumesPost) error {
	if _, ok := f.volumes[pool+"/"+v.Name]; ok {
		return api.StatusErrorf(http.StatusConflict, "Volume by that name already exists") // as Incus answers
	}
	f.calls = append(f.calls, "create volume "+v.Name)
	f.volumes[pool+"/"+v.Name] = v
	return nil
}

func (f *fake) GetStoragePoolNames() ([]string, error) {
	pools := map[string]bool{"default": true}
	for key := range f.volumes {
		pools[strings.Split(key, "/")[0]] = true
	}
	return slices.Sorted(maps.Keys(pools)), nil
}

func (f *fake) GetStoragePoolVolume(pool, _, name string) (*api.StorageVolume, string, error) {
	if _, ok := f.volumes[pool+"/"+name]; !ok {
		return nil, "", errNotFound
	}
	return &api.StorageVolume{Name: name, Type: "custom"}, "", nil
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
	tok := api.CertificateAddToken{
		ClientName:  req.Name,
		Fingerprint: "ab",
		Addresses:   []string{"192.168.1.10:8443"},
		Secret:      "s",
	}
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
		ops = append(
			ops,
			tokenOp(
				id,
				api.CertificateAddToken{ClientName: req.Name, Fingerprint: "ab", Addresses: []string{"a"}, Secret: "s"},
			),
		)
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
	f.instances[req.Name] = &api.Instance{
		Name:            req.Name,
		StatusCode:      api.Stopped,
		InstancePut:     req.InstancePut,
		ExpandedDevices: req.Devices,
	}
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
	if inst := f.instances[name]; inst != nil {
		if st.Action == "stop" && inst.StatusCode == api.Stopped {
			return nil, errors.New("The instance is already stopped") // as Incus answers
		}
		inst.StatusCode = map[string]api.StatusCode{"stop": api.Stopped, "start": api.Running}[st.Action]
	}
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

func (o op) Get() api.Operation                    { return o.meta }
func (o op) Wait() error                           { return nil }
func (o op) WaitContext(ctx context.Context) error { return ctx.Err() } // as the client's event-listener wait does

func opts() Options {
	return Options{
		Name:  "incus-beacon",
		Image: "ghcr.io/tallica/incus-beacon:0.1",
		Watch: "all",
		Wait:  3 * time.Second,
		Log:   zerolog.Nop(),
	}
}

// checkAudit stops the test unless buf holds records spelt "event phase", each naming the fakes' incus-beacon and this process.
func checkAudit(t *testing.T, buf *bytes.Buffer, want ...string) []map[string]any {
	t.Helper()
	var records []map[string]any
	var got []string
	for line := range strings.Lines(buf.String()) {
		var r map[string]any
		if err := json.Unmarshal([]byte(line), &r); err != nil {
			t.Fatalf("record %q: %v", line, err)
		}
		if r["instance"] != "incus-beacon" || r["by"] != log.Process() {
			t.Errorf("record %v: want instance and by", r)
		}
		records = append(records, r)
		got = append(got, fmt.Sprint(r["event"], " ", r["phase"]))
	}
	if !slices.Equal(got, want) {
		t.Fatalf("audit records %q, want %q", got, want)
	}
	return records
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
	for _, name := range []string{"incus-beacon-state", "incus-beacon-log"} {
		vol, ok := f.volumes["default/"+name]
		if !ok || vol.Config["initial.uid"] != "65534" || vol.Config["initial.gid"] != "65534" {
			t.Errorf("volume %s %+v, want one owned by 65534", name, vol)
		}
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
		"environment.BEACON_AUDIT_LOG":  "/var/log/incus-beacon/audit.log",
		"user.incus-beacon.track":       "semver",
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
	if d := inst.Devices[logDev]; d["type"] != "disk" || d["pool"] != "default" || d["source"] != "incus-beacon-log" ||
		d["path"] != "/var/log/incus-beacon" {
		t.Errorf("log device %v", d)
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

func TestInstallRefusesAMalformedImageFirst(t *testing.T) {
	t.Parallel()
	f := newFake()
	o := opts()
	o.Image = "image=ghcr.io/tallica/incus-beacon:0.1"
	if err := Install(
		context.Background(),
		f,
		o,
	); err == nil ||
		!strings.Contains(err.Error(), "not a valid reference") {
		t.Fatalf("err %v, want the image refused", err)
	}
	if len(f.calls) != 0 || len(f.volumes) != 0 || len(f.tokenOps) != 0 {
		t.Errorf("calls %v, want nothing created", f.calls)
	}
}

func TestInstallStoresNotifyURLsInTheVolume(t *testing.T) {
	t.Parallel()
	f := newFake()
	o := opts()
	o.NotifyURLs = []string{"https://example.com/hook", "slack://hooks.example.com/services/T0K3N"}
	o.NotifyErrs = true
	var buf bytes.Buffer
	o.Audit = zerolog.New(&buf)
	if err := Install(context.Background(), f, o); err != nil {
		t.Fatal(err)
	}
	for _, r := range checkAudit(t, &buf, "install begin", "install done") {
		if r["image"] != o.Image || r["pool"] != "default" || r["volume"] != "incus-beacon-state" ||
			r["log_volume"] != "incus-beacon-log" {
			t.Errorf("record %v: want image, pool and both volumes", r)
		}
	}
	if strings.Contains(buf.String(), "example.com") {
		t.Errorf("a notification URL reached the audit log: %s", buf.String())
	}
	if got, want := f.readVolumeFile(
		t,
		"/"+notifyFile,
	), "https://example.com/hook\nslack://hooks.example.com/services/T0K3N\n"; got != want {
		t.Errorf("volume file %q, want %q", got, want)
	}
	cfg := f.instances["incus-beacon"].Config
	if cfg["environment.BEACON_NOTIFY_URL_FILE"] != stateDir+"/"+notifyFile ||
		cfg["environment.BEACON_NOTIFY_ERRORS"] != "true" {
		t.Errorf("config %v", cfg)
	}
	for k, v := range cfg {
		if strings.Contains(v, "T0K3N") {
			t.Errorf("%s holds a notification URL", k)
		}
	}
	if i, j := slices.Index(
		f.calls,
		"sftp incus-beacon-state",
	), slices.Index(
		f.calls,
		"create instance incus-beacon",
	); i < 0 ||
		i > j {
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
	o := opts()
	var buf bytes.Buffer
	o.Audit = zerolog.New(&buf)
	err := Install(context.Background(), f, o)
	if err == nil || !strings.Contains(err.Error(), "image not found") {
		t.Fatalf("err %v", err)
	}
	if r := checkAudit(
		t,
		&buf,
		"install begin",
		"install.rollback begin",
		"install.rollback done",
		"install failed",
	); !strings.Contains(
		fmt.Sprint(r[3]["error"]),
		"image not found",
	) {
		t.Errorf("failed record %v, want the error", r[3])
	}
	want := []string{
		"create volume incus-beacon-state",
		"create volume incus-beacon-log",
		"create token",
		"delete token",
		"delete volume incus-beacon-log",
		"delete volume incus-beacon-state",
	}
	if !slices.Equal(f.calls, want) {
		t.Errorf("calls %v, want %v", f.calls, want)
	}
}

func TestInstallReusesALogVolume(t *testing.T) {
	t.Parallel()
	f := newFake()
	f.volumes["default/incus-beacon-log"] = api.StorageVolumesPost{Name: "incus-beacon-log"}
	if err := Install(context.Background(), f, opts()); err != nil {
		t.Fatal(err)
	}
	if slices.Contains(f.calls, "create volume incus-beacon-log") {
		t.Errorf("calls %v, want the old log volume reused", f.calls)
	}
	if d := f.instances["incus-beacon"].Devices[logDev]; d["source"] != "incus-beacon-log" {
		t.Errorf("log device %v, want the reused volume", d)
	}

	f = newFake()
	f.volumes["default/incus-beacon-log"] = api.StorageVolumesPost{Name: "incus-beacon-log"}
	f.failCreate = errors.New("image not found")
	if err := Install(context.Background(), f, opts()); err == nil {
		t.Fatal("want the failed create")
	}
	if _, ok := f.volumes["default/incus-beacon-log"]; !ok ||
		slices.Contains(f.calls, "delete volume incus-beacon-log") {
		t.Errorf("calls %v, want the reused log volume left by the rollback", f.calls)
	}
}

func TestInstallKeepsInstanceThatDidNotJoin(t *testing.T) {
	t.Parallel()
	f := newFake()
	f.joins = false
	o := opts()
	o.Wait = 10 * time.Millisecond
	var buf bytes.Buffer
	o.Audit = zerolog.New(&buf)
	if err := Install(context.Background(), f, o); err == nil || !strings.Contains(err.Error(), "did not join") {
		t.Fatalf("err %v", err)
	}
	if f.instances["incus-beacon"] == nil {
		t.Error("instance removed; its log is what explains the failure")
	}
	checkAudit(t, &buf, "install begin", "install failed")
}

func TestInstallFailsAtOnceWhenCertificatesCannotBeListed(t *testing.T) {
	t.Parallel()
	f := newFake()
	f.failList = errors.New("forbidden")
	o := opts()
	o.Wait = time.Minute
	err := Install(context.Background(), f, o)
	if err == nil || !strings.Contains(err.Error(), "list certificates: forbidden") ||
		strings.Contains(err.Error(), "--show-log") {
		t.Fatalf("err %v, want the listing error without the instance-log hint", err)
	}
	if f.lists != 1 {
		t.Errorf("%d listings once started, want the first failure to end the wait", f.lists)
	}
}

func TestInstallInterruptedDuringWaitDoesNotBlameTheInstance(t *testing.T) {
	t.Parallel()
	f := newFake()
	f.joins = false
	o := opts()
	o.Wait = time.Minute
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(50*time.Millisecond, cancel)
	err := Install(ctx, f, o)
	if !errors.Is(err, context.Canceled) || strings.Contains(err.Error(), "did not join") {
		t.Fatalf("err %v, want the interruption, not a join failure", err)
	}
}

func TestInstallRecordsARollbackOfNothing(t *testing.T) {
	t.Parallel()
	f := newFake()
	f.volumes["default/incus-beacon-state"] = api.StorageVolumesPost{Name: "incus-beacon-state"}
	o := opts()
	var buf bytes.Buffer
	o.Audit = zerolog.New(&buf)
	if err := Install(context.Background(), f, o); err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("err %v, want the state volume's conflict", err)
	}
	checkAudit(t, &buf, "install begin", "install.rollback begin", "install.rollback done", "install failed")
	if _, ok := f.volumes["default/incus-beacon-state"]; !ok {
		t.Error("a volume install did not create was deleted")
	}
}

func TestInstallNamesItIncusBeacond(t *testing.T) {
	t.Parallel()
	f := newFake()
	o := opts()
	o.Name = ""
	if err := Install(context.Background(), f, o); err != nil {
		t.Fatal(err)
	}
	if _, ok := f.instances[DefaultName]; !ok {
		t.Errorf("instances %v, want %s", slices.Collect(maps.Keys(f.instances)), DefaultName)
	}
}

func TestInstallRefusesBesideABeaconFromBefore06(t *testing.T) {
	t.Parallel()
	f := installedFake() // an instance called incus-beacon
	o := opts()
	o.Name = ""
	if err := Install(context.Background(), f, o); err == nil || !strings.Contains(err.Error(), "upgrade it") {
		t.Errorf("err %v, want a pointer to upgrade", err)
	}
	if _, ok := f.instances[DefaultName]; ok {
		t.Error("a second beacon was created")
	}
}

func TestResolveName(t *testing.T) {
	t.Parallel()
	logger := zerolog.Nop()
	if got := ResolveName(newFake(), "", logger); got != DefaultName {
		t.Errorf("nothing installed: %q, want %q", got, DefaultName)
	}
	if got := ResolveName(installedFake(), "", logger); got != "incus-beacon" {
		t.Errorf("an instance from before 0.6: %q, want incus-beacon", got)
	}
	both := installedFake()
	both.instances[DefaultName] = &api.Instance{Name: DefaultName}
	if got := ResolveName(both, "", logger); got != DefaultName {
		t.Errorf("both: %q, want %q", got, DefaultName)
	}
	if got := ResolveName(both, "mine", logger); got != "mine" {
		t.Errorf("--name mine: %q", got)
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
	f.certs = []api.Certificate{
		{Name: "incus-beacon", Fingerprint: "mine"},
		{Name: "someone-else", Fingerprint: "theirs"},
	}
	f.calls = nil

	var buf bytes.Buffer
	if err := Uninstall(
		context.Background(),
		f,
		UninstallOptions{Name: "incus-beacon", Log: zerolog.Nop(), Audit: zerolog.New(&buf)},
	); err != nil {
		t.Fatal(err)
	}
	r := checkAudit(t, &buf, "uninstall begin", "uninstall done")
	if got, want := fmt.Sprint(
		r[1]["removed"],
	), "[instance incus-beacon volume incus-beacon-state trust token tok-incus-beacon certificate mine]"; got != want {
		t.Errorf("removed %s, want %s", got, want)
	}
	want := []string{
		"delete instance incus-beacon",
		"delete volume incus-beacon-state",
		"delete token",
		"delete cert mine",
	}
	if !slices.Equal(f.calls, want) {
		t.Errorf("calls %v, want %v", f.calls, want)
	}
	if !slices.Contains(f.states, "stop") {
		t.Error("running instance not stopped first")
	}
	if len(f.certs) != 1 || f.certs[0].Name != "someone-else" {
		t.Errorf("certs left %v", f.certs)
	}
	if _, ok := f.volumes["default/incus-beacon-log"]; !ok {
		t.Error("log volume deleted without --delete-log")
	}
	if r[1]["kept_volume"] != "incus-beacon-log" {
		t.Errorf("kept_volume %v, want the log volume", r[1]["kept_volume"])
	}
}

func TestUninstallFinishesAnAcceptedDeleteOnceCancelled(t *testing.T) {
	t.Parallel()
	f := newFake()
	f.joins = false
	o := opts()
	o.Wait = 10 * time.Millisecond
	_ = Install(context.Background(), f, o)
	f.instances["incus-beacon"].StatusCode = api.Stopped
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := Uninstall(
		ctx,
		f,
		UninstallOptions{Name: "incus-beacon", Log: zerolog.Nop(), Audit: zerolog.Nop()},
	); err != nil {
		t.Fatalf("interrupted once the delete was accepted: %v", err)
	}
	if _, ok := f.volumes["default/incus-beacon-state"]; ok {
		t.Error("state volume left behind")
	}
}

func TestUninstallDeleteLog(t *testing.T) {
	t.Parallel()
	f := newFake()
	if err := Install(context.Background(), f, opts()); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	o := UninstallOptions{Name: "incus-beacon", DeleteLog: true, Log: zerolog.Nop(), Audit: zerolog.New(&buf)}
	if err := Uninstall(context.Background(), f, o); err != nil {
		t.Fatal(err)
	}
	r := checkAudit(t, &buf, "uninstall begin", "uninstall done")
	if got := fmt.Sprint(r[1]["removed"]); !strings.Contains(got, "volume incus-beacon-log") {
		t.Errorf("removed %s, want the log volume", got)
	}
	if len(f.volumes) != 0 {
		t.Errorf("volumes left %v", slices.Collect(maps.Keys(f.volumes)))
	}
	if err := Uninstall(context.Background(), f, o); err != nil {
		t.Errorf("second uninstall: %v", err)
	}
}

func TestUninstallFindsVolumesOnAnotherPoolOnceTheInstanceIsGone(t *testing.T) {
	t.Parallel()
	for _, del := range []bool{false, true} {
		f := newFake()
		f.volumes["fast/incus-beacon-state"] = api.StorageVolumesPost{Name: "incus-beacon-state"}
		f.volumes["fast/incus-beacon-log"] = api.StorageVolumesPost{Name: "incus-beacon-log"}
		var buf bytes.Buffer
		o := UninstallOptions{Name: "incus-beacon", DeleteLog: del, Log: zerolog.Nop(), Audit: zerolog.New(&buf)}
		if err := Uninstall(context.Background(), f, o); err != nil {
			t.Fatal(err)
		}
		r := checkAudit(t, &buf, "uninstall begin", "uninstall done")
		_, logLeft := f.volumes["fast/incus-beacon-log"]
		if _, ok := f.volumes["fast/incus-beacon-state"]; ok || logLeft == del {
			t.Errorf("--delete-log=%t: volumes left %v", del, slices.Collect(maps.Keys(f.volumes)))
		}
		if want := map[bool]any{false: "incus-beacon-log", true: nil}[del]; r[1]["kept_volume"] != want {
			t.Errorf("--delete-log=%t: kept_volume %v, want %v", del, r[1]["kept_volume"], want)
		}
	}
}

func TestUninstallNothingThere(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	if err := Uninstall(
		context.Background(),
		newFake(),
		UninstallOptions{Name: "incus-beacon", Log: zerolog.Nop(), Audit: zerolog.New(&buf)},
	); err != nil {
		t.Errorf("err %v", err)
	}
	if r := checkAudit(
		t,
		&buf,
		"uninstall begin",
		"uninstall done",
	); r[1]["removed"] != nil ||
		r[1]["kept_volume"] != nil {
		t.Errorf("done record %v, want nothing removed or kept", r[1])
	}
}

func TestUninstallFindsInstanceInAnotherProject(t *testing.T) {
	t.Parallel()
	f := newFake()
	f.elsewhere = map[string]*api.Instance{
		"incus-beacon": {Name: "incus-beacon", Project: "apps", StatusCode: api.Stopped},
	}
	f.certs = []api.Certificate{{Name: "incus-beacon", Fingerprint: "mine"}}
	var buf bytes.Buffer
	if err := Uninstall(
		context.Background(),
		f,
		UninstallOptions{Name: "incus-beacon", Log: zerolog.Nop(), Audit: zerolog.New(&buf)},
	); err != nil {
		t.Fatal(err)
	}
	for _, r := range checkAudit(t, &buf, "uninstall begin", "uninstall done") {
		if r["project"] != "apps" {
			t.Errorf("record %v: want the instance's project", r)
		}
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

// serveFrom parses serve the way the instance does, from the environment install wrote.
func serveFrom(t *testing.T, inst *api.Instance) *config.Serve {
	t.Helper()
	getenv := func(name string) string {
		if name == "BEACON_STATE_DIR" {
			return stateDir // the image's Containerfile sets it, not install
		}
		return inst.Config["environment."+name]
	}
	cfg, err := config.Parse([]string{config.CmdServe}, getenv, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	return cfg.Serve
}

func TestInstallServeSettingsReachServe(t *testing.T) {
	t.Parallel()
	f := newFake()
	o := opts()
	o.Interval = time.Hour
	o.Cooldown = 48 * time.Hour
	o.Update = true
	o.UpdateWindow = "02:00-05:00"
	if err := Install(context.Background(), f, o); err != nil {
		t.Fatal(err)
	}
	s := serveFrom(t, f.instances["incus-beacon"])
	if s.Interval != time.Hour || s.Cooldown != 48*time.Hour || !s.Update || s.UpdateWindow != "02:00-05:00" {
		t.Errorf("serve on the installed instance: %+v", s)
	}
}

func TestInstallLeavesServeDefaults(t *testing.T) {
	t.Parallel()
	f := newFake()
	if err := Install(context.Background(), f, opts()); err != nil {
		t.Fatal(err)
	}
	inst := f.instances["incus-beacon"]
	for _, k := range []string{"INTERVAL", "COOLDOWN", "UPDATE", "UPDATE_WINDOW"} {
		if v, ok := inst.Config["environment.BEACON_"+k]; ok {
			t.Errorf("BEACON_%s = %q with no setting given", k, v)
		}
	}
	if s := serveFrom(t, inst); s.Interval != 6*time.Hour || s.Cooldown != 0 || s.Update || s.UpdateWindow != "" {
		t.Errorf("serve defaults on the installed instance: %+v", s)
	}
}

func TestInstallNotifyErrorsNeedsURLs(t *testing.T) {
	t.Parallel()
	f := newFake()
	o := opts()
	o.NotifyErrs = true
	if err := Install(context.Background(), f, o); err != nil {
		t.Fatal(err)
	}
	if v, ok := f.instances["incus-beacon"].Config["environment.BEACON_NOTIFY_ERRORS"]; ok {
		t.Errorf("BEACON_NOTIFY_ERRORS = %q with no notify URL, want it unset", v)
	}
}
