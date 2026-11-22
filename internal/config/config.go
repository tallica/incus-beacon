// Package config parses the subcommand, its flags and BEACON_* variables.
package config

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/tallica/incus-beacon/internal/policy"
)

const (
	CmdCheck     = "check"
	CmdStatus    = "status"
	CmdServe     = "serve"
	CmdInstall   = "install"
	CmdConfigure = "configure"
	CmdUpdate    = "update"
	CmdUpgrade   = "upgrade"
	CmdUninstall = "uninstall"
	CmdVersion   = "version"
)

// DaemonName is the binary's second name: called by it, no command means serve.
const DaemonName = "incus-beacond"

// ForProgram prepends serve for DaemonName given no command, or only flags.
func ForProgram(prog string, args []string) []string {
	if filepath.Base(prog) != DaemonName {
		return args
	}
	if len(args) == 0 || strings.HasPrefix(args[0], "-") && !slices.Contains(helpArgs, args[0]) && !slices.Contains(versionArgs, args[0]) {
		return append([]string{CmdServe}, args...)
	}
	return args
}

// Parse takes these in place of a command.
var (
	helpArgs    = []string{"-h", "-help", "--help", "help"}
	versionArgs = []string{"-version", "--version"}
)

// groups is how usage lists the subcommands; an untitled group follows a blank line.
var groups = []struct {
	title string
	cmds  []string
}{
	{"Watching instances:", []string{CmdCheck, CmdStatus, CmdServe, CmdUpdate}},
	{"Running beacon on Incus:", []string{CmdInstall, CmdConfigure, CmdUpgrade, CmdUninstall}},
	{"", []string{CmdVersion}},
}

// Commands lists the subcommands in the order usage shows them.
var Commands = func() []string {
	var all []string
	for _, g := range groups {
		all = append(all, g.cmds...)
	}
	return all
}()

var summaries = map[string]string{
	CmdCheck:     "check registries for newer images and print what can be updated",
	CmdStatus:    "show the update status already recorded on instances",
	CmdServe:     "run checks on an interval and record results",
	CmdUpdate:    "move one watched instance to its newer image, with rollback",
	CmdInstall:   "create beacon's own serve instance, joined with a trust token",
	CmdConfigure: "change the settings of beacon's own instance, with rollback",
	CmdUpgrade:   "move beacon's own instance to a newer beacon release",
	CmdUninstall: "remove beacon's instance, its volume and its certificate",
	CmdVersion:   "print the version",
}

// ErrUsage marks errors caused by how the command was invoked.
var ErrUsage = errors.New("usage")

// ErrHelp is returned when help was asked for; usage has been printed.
var ErrHelp = flag.ErrHelp

// Common holds the settings every Incus-facing command shares.
type Common struct {
	LogLevel  string
	LogFormat string
	LogColor  string

	Remote            string
	RemoteProject     string
	Incus             string
	ClientCert        string
	ClientKey         string
	ServerFingerprint string
	Token             string
	TokenFile         string
	StateDir          string

	Projects []string
	Watch    string
}

// Registry holds the settings of commands that query registries.
type Registry struct {
	RegistryAuthFile string
	Concurrency      int
	Timeout          time.Duration
	Cooldown         time.Duration
}

// Audit holds where update records go; an empty AuditLog writes none.
type Audit struct {
	AuditLog        string
	AuditLogMaxSize int64
	AuditLogKeep    int
}

type Check struct {
	Common
	Registry
	Instance string
	Format   string
	Write    bool
}

type Status struct {
	Common
	Format      string
	OnlyUpdates bool
}

type Serve struct {
	Common
	Registry
	Interval time.Duration
	Listen   string
	Events   bool
	Notify
	Update       bool
	UpdateWindow string        // HH:MM-HH:MM, or empty for any time
	Window       policy.Window // UpdateWindow parsed; the zero Window is any time
	UpdateFlags
	Audit
}

type Notify struct {
	NotifyURLs    []string
	NotifyURLFile string
	NotifyCommand string
	NotifyErrors  bool
}

// Install binds only the log, remote and watch settings of Common.
type Install struct {
	Common
	Settings
	Name        string
	Image       string
	StoragePool string
	Wait        time.Duration
}

// Settings are what install gives the instance and configure changes; README shows how to set the rest.
type Settings struct {
	APIURL       string
	Events       bool
	Track        string
	NotifyURLs   []string
	NotifyErrs   bool
	Interval     time.Duration
	Cooldown     time.Duration
	Update       bool
	UpdateWindow string
}

// Configure binds the same settings as install; only those Given change.
type Configure struct {
	Common
	Settings
	Name  string
	Wait  time.Duration
	given map[string]bool
}

// Given reports whether the setting flag name was on the command line; it panics on a name bindSettings doesn't bind.
func (c *Configure) Given(name string) bool {
	if !slices.Contains(settingFlags, name) {
		panic("config: " + name + " is not a configure setting")
	}
	return c.given[name]
}

type Update struct {
	Common
	Registry
	Instance string
	DryRun   bool
	UpdateFlags
	Audit
}

type UpdateFlags struct {
	StopTimeout time.Duration
	Wait        time.Duration
	Settle      time.Duration
	Keep        int
}

type Upgrade struct {
	Common
	Name  string
	Image string
	Force bool
	Wait  time.Duration
}

type Uninstall struct {
	Common
	Name string
}

// Config sets at most one command field; none for version.
type Config struct {
	Command   string
	Check     *Check
	Status    *Status
	Serve     *Serve
	Update    *Update
	Install   *Install
	Configure *Configure
	Upgrade   *Upgrade
	Uninstall *Uninstall
}

// AuditOf is the command's Audit; the zero Audit, which writes nothing, for commands without the flags.
func (c *Config) AuditOf() Audit {
	switch {
	case c.Update != nil:
		return c.Update.Audit
	case c.Serve != nil:
		return c.Serve.Audit
	}
	return Audit{}
}

const (
	FormatConsole = "console"
	FormatJSON    = "json"
)

const (
	ColorAuto   = "auto"
	ColorNever  = "never"
	ColorAlways = "always"
)

const (
	OutputTable = "table"
	OutputJSON  = "json"
)

const (
	WatchAll   = "all"
	WatchOptIn = "opt-in"
)

var logLevels = []string{"debug", "info", "warn", "error"}

// Parse reads args after the program name; a flag beats its BEACON_* variable.
func Parse(args []string, getenv func(string) string, out io.Writer) (*Config, error) {
	if len(args) == 0 {
		Usage(out)
		return nil, fmt.Errorf("%w: no command given", ErrUsage)
	}
	name, rest := args[0], args[1:]
	switch {
	case slices.Contains(helpArgs, name):
		Usage(out)
		return nil, ErrHelp
	case slices.Contains(versionArgs, name):
		name = CmdVersion
	}

	fs := flag.NewFlagSet("incus-beacon "+name, flag.ContinueOnError)
	fs.SetOutput(out)
	cfg := &Config{Command: name}
	var validate func() error

	switch name {
	case CmdCheck:
		c := &Check{}
		bindCommon(fs, &c.Common)
		bindRegistry(fs, &c.Registry)
		bindConcurrency(fs, &c.Registry)
		fs.StringVar(&c.Instance, "instance", "", "check only this instance")
		fs.StringVar(&c.Format, "format", OutputTable, "output format: table or json")
		fs.BoolVar(&c.Write, "write", false, "also record results in user.incus-beacon.* keys")
		cfg.Check = c
		validate = func() error {
			return errors.Join(c.Common.validate(), c.Registry.validate(), oneOf("format", c.Format, OutputTable, OutputJSON))
		}
	case CmdStatus:
		c := &Status{}
		bindCommon(fs, &c.Common)
		fs.StringVar(&c.Format, "format", OutputTable, "output format: table or json")
		fs.BoolVar(&c.OnlyUpdates, "only-updates", false, "list only instances with an update available")
		cfg.Status = c
		validate = func() error {
			return errors.Join(c.validate(), oneOf("format", c.Format, OutputTable, OutputJSON))
		}
	case CmdServe:
		c := &Serve{}
		bindCommon(fs, &c.Common)
		bindRegistry(fs, &c.Registry)
		bindConcurrency(fs, &c.Registry)
		fs.DurationVar(&c.Interval, "interval", 6*time.Hour, "time between check cycles")
		fs.StringVar(&c.Listen, "listen", ":9153", "address for /health and /ready")
		fs.BoolVar(&c.Events, "events", false, "also check instances as soon as they are created, updated or rebuilt")
		fs.Var((*urlsValue)(&c.NotifyURLs), "notify-url", "where to send notifications: an http(s) webhook, discord:// or slack://; repeat, or space-separate, for several")
		fs.StringVar(&c.NotifyURLFile, "notify-url-file", "", "file of further --notify-url values, one per line, as install writes it")
		fs.StringVar(&c.NotifyCommand, "notify-command", "", "program run per notification, with the JSON payload on stdin; arguments split on spaces, no shell")
		fs.BoolVar(&c.NotifyErrors, "notify-errors", false, "also notify when an instance's check starts failing")
		fs.BoolVar(&c.Update, "update", false, "apply updates to instances that set user.incus-beacon.update=auto")
		fs.StringVar(&c.UpdateWindow, "update-window", "", "daily HH:MM-HH:MM, in local time, when updates may run, unless an instance sets its own (default: any time)")
		bindUpdate(fs, &c.UpdateFlags)
		bindAudit(fs, &c.Audit)
		cfg.Serve = c
		validate = func() error {
			errs := []error{c.Common.validate(), c.Registry.validate()}
			if c.Interval < time.Minute {
				errs = append(errs, fmt.Errorf("interval %s: must be at least 1m", c.Interval))
			}
			if c.Listen == "" {
				errs = append(errs, errors.New("listen: must not be empty"))
			}
			if c.NotifyErrors && len(c.NotifyURLs) == 0 && c.NotifyURLFile == "" && c.NotifyCommand == "" {
				errs = append(errs, errors.New("notify-errors needs notify-url, notify-url-file or notify-command"))
			}
			if c.UpdateWindow != "" {
				var err error
				if c.Window, err = policy.ParseWindow(c.UpdateWindow); err != nil {
					errs = append(errs, err)
				}
			}
			errs = append(errs, c.UpdateFlags.validate(), c.Audit.validate())
			return errors.Join(errs...)
		}
	case CmdUpdate:
		c := &Update{Registry: Registry{Concurrency: 1}}
		bindCommon(fs, &c.Common)
		bindRegistry(fs, &c.Registry)
		fs.StringVar(&c.Instance, "instance", "", "the instance to update (required)")
		fs.BoolVar(&c.DryRun, "dry-run", false, "print what would be updated, and change nothing")
		bindUpdate(fs, &c.UpdateFlags)
		bindAudit(fs, &c.Audit)
		cfg.Update = c
		validate = func() error {
			errs := []error{c.Common.validate(), c.Registry.validate(), c.UpdateFlags.validate(), c.Audit.validate()}
			if c.Instance == "" {
				errs = append(errs, errors.New("instance: required"))
			}
			return errors.Join(errs...)
		}
	case CmdInstall:
		c := &Install{}
		bindLog(fs, &c.Common)
		bindRemoteProject(fs, &c.Common)
		bindInstance(fs, &c.Name, "incus-beacond, refused beside an incus-beacon from before 0.6")
		fs.StringVar(&c.Image, "image", "", "image to run (default: ghcr.io/tallica/incus-beacon at this version's major.minor)")
		fs.StringVar(&c.StoragePool, "storage-pool", "", "pool for the state volume (default: the default profile's root pool)")
		fs.DurationVar(&c.Wait, "wait", time.Minute, "how long to wait for the instance to join")
		bindSettings(fs, &c.Common, &c.Settings)
		cfg.Install = c
		validate = func() error {
			errs := []error{c.Common.validate(), c.Settings.validate()}
			if c.Wait <= 0 {
				errs = append(errs, fmt.Errorf("wait %s: must be positive", c.Wait))
			}
			if c.NotifyErrs && len(c.NotifyURLs) == 0 {
				errs = append(errs, errors.New("notify-errors needs notify-url"))
			}
			return errors.Join(errs...)
		}
	case CmdConfigure:
		c := &Configure{Common: Common{Watch: WatchAll}}
		bindLog(fs, &c.Common)
		bindRemoteProject(fs, &c.Common)
		bindInstance(fs, &c.Name, "incus-beacond, or an incus-beacon from before 0.6")
		fs.DurationVar(&c.Wait, "wait", 2*time.Minute, "how long to wait for /ready before rolling back")
		bindSettings(fs, &c.Common, &c.Settings)
		cfg.Configure = c
		validate = func() error {
			errs := []error{c.Common.validate(), c.Settings.validate()}
			if c.Wait <= 0 {
				errs = append(errs, fmt.Errorf("wait %s: must be positive", c.Wait))
			}
			// A bare configure is a usage mistake; settings already as given are a no-op install.Configure reports.
			if !slices.ContainsFunc(settingFlags, c.Given) {
				errs = append(errs, errors.New("no setting given; see incus-beacon configure -h"))
			}
			return errors.Join(errs...)
		}
	case CmdUpgrade:
		c := &Upgrade{Common: Common{Watch: WatchAll}}
		bindLog(fs, &c.Common)
		bindRemoteProject(fs, &c.Common)
		bindInstance(fs, &c.Name, "incus-beacond, or an incus-beacon from before 0.6")
		fs.StringVar(&c.Image, "image", "", "image to rebuild from (default: ghcr.io/tallica/incus-beacon at this version's major.minor)")
		fs.BoolVar(&c.Force, "force", false, "allow an image older than the running one")
		fs.DurationVar(&c.Wait, "wait", 2*time.Minute, "how long to wait for /ready before rolling back")
		cfg.Upgrade = c
		validate = func() error {
			errs := []error{c.validate()}
			if c.Wait <= 0 {
				errs = append(errs, fmt.Errorf("wait %s: must be positive", c.Wait))
			}
			return errors.Join(errs...)
		}
	case CmdUninstall:
		c := &Uninstall{Common: Common{Watch: WatchAll}}
		bindLog(fs, &c.Common)
		bindRemoteProject(fs, &c.Common)
		bindInstance(fs, &c.Name, "incus-beacond, or an incus-beacon from before 0.6")
		cfg.Uninstall = c
		validate = func() error { return c.validate() }
	case CmdVersion:
		validate = func() error { return nil }
	default:
		Usage(out)
		return nil, fmt.Errorf("%w: unknown command %q", ErrUsage, name)
	}

	if err := fs.Parse(rest); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil, ErrHelp
		}
		return nil, fmt.Errorf("%w: %w", ErrUsage, err)
	}
	if fs.NArg() > 0 && slices.Contains(Commands, fs.Arg(0)) {
		return nil, fmt.Errorf("%w: unexpected argument %q; give one command, before its flags", ErrUsage, fs.Arg(0))
	}
	if fs.NArg() > 0 {
		return nil, fmt.Errorf("%w: unexpected argument %q", ErrUsage, fs.Arg(0))
	}
	var envSkip []string
	if c := cfg.Configure; c != nil {
		// Only the command line counts: a BEACON_PROJECT exported for check must not narrow the instance.
		c.given = map[string]bool{}
		fs.Visit(func(f *flag.Flag) { c.given[f.Name] = true })
		envSkip = settingFlags
	}
	if err := applyEnv(fs, getenv, envSkip); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrUsage, err)
	}
	if err := validate(); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrUsage, err)
	}
	return cfg, nil
}

func Usage(out io.Writer) {
	fmt.Fprintln(out, "Usage: incus-beacon <command> [flags]")
	width := 0
	for _, c := range Commands {
		width = max(width, len(c))
	}
	for _, g := range groups {
		fmt.Fprintln(out)
		if g.title != "" {
			fmt.Fprintln(out, g.title)
		}
		for _, c := range g.cmds {
			fmt.Fprintf(out, "  %-*s  %s\n", width, c, summaries[c])
		}
	}
	fmt.Fprintln(out)
	fmt.Fprintln(out, "Run 'incus-beacon <command> -h' for its flags. Every flag can also be set")
	fmt.Fprintln(out, "with an environment variable: --log-level is BEACON_LOG_LEVEL.")
}

// EnvName is the environment variable that sets the flag name.
func EnvName(flagName string) string {
	return "BEACON_" + strings.ToUpper(strings.ReplaceAll(flagName, "-", "_"))
}

func bindLog(fs *flag.FlagSet, c *Common) {
	fs.StringVar(&c.LogLevel, "log-level", "info", "log level: debug, info, warn or error")
	fs.StringVar(&c.LogFormat, "log-format", FormatConsole, "log format: console or json")
	fs.StringVar(&c.LogColor, "log-color", ColorAuto, "console log colour: auto (on a terminal), never or always")
}

func bindRemote(fs *flag.FlagSet, c *Common) {
	fs.Var(&remoteValue{c: c}, "remote", "incus CLI `remote` to use (default: the CLI's default remote)")
}

// bindRemoteProject also takes <remote>:<project>, the project the instance lives in.
func bindRemoteProject(fs *flag.FlagSet, c *Common) {
	fs.Var(&remoteValue{c: c, project: true}, "remote", "incus CLI `remote[:project]` to use, the project being where the instance lives (default: the CLI's default remote and its current project)")
}

func bindInstance(fs *flag.FlagSet, name *string, dflt string) {
	fs.StringVar(name, "name", "", "instance name; also names its volume and certificate (default: "+dflt+")")
}

func bindCommon(fs *flag.FlagSet, c *Common) {
	bindLog(fs, c)
	bindRemote(fs, c)
	fs.StringVar(&c.Incus, "incus", "", "Incus API URL, e.g. https://192.168.1.10:8443")
	fs.StringVar(&c.ClientCert, "client-cert", "", "client certificate file for --incus")
	fs.StringVar(&c.ClientKey, "client-key", "", "client key file for --incus")
	fs.StringVar(&c.ServerFingerprint, "server-fingerprint", "", "expected server certificate fingerprint")
	fs.StringVar(&c.Token, "token", "", "one-time trust token for --incus; enrolls a certificate into --state-dir")
	fs.StringVar(&c.TokenFile, "token-file", "", "file holding the --token, read only when enrolling")
	fs.StringVar(&c.StateDir, "state-dir", "", "directory keeping the certificate --token enrolled")

	fs.Var((*csvValue)(&c.Projects), "project", "project to watch; comma-separate several (default: all)")
	fs.StringVar(&c.Watch, "watch", WatchAll, "which OCI instances to watch: all or opt-in")
}

// bindSettings binds install's settings; never with bindCommon, which binds --project and --watch too.
func bindSettings(fs *flag.FlagSet, c *Common, s *Settings) {
	fs.Var((*csvValue)(&c.Projects), "project", "project to watch, and to restrict the certificate to; comma-separate several (default: all)")
	fs.StringVar(&c.Watch, "watch", WatchAll, "which OCI instances to watch: all or opt-in")
	fs.StringVar(&s.APIURL, "api-url", "", "Incus API URL the instance connects to (default: detected)")
	fs.BoolVar(&s.Events, "events", false, "have the instance check instances as soon as they are created, updated or rebuilt")
	fs.StringVar(&s.Track, "track", "", "how the instance follows its own image: digest or semver (default: semver for a version tag, else digest)")
	fs.Var((*urlsValue)(&s.NotifyURLs), "notify-url", "where the instance sends notifications; stored in its state volume, not its config")
	fs.BoolVar(&s.NotifyErrs, "notify-errors", false, "have the instance also notify when a check starts failing")
	fs.DurationVar(&s.Interval, "interval", 0, "time between the instance's check cycles (default: serve's 6h)")
	fs.DurationVar(&s.Cooldown, "cooldown", 0, "have the instance report an image only once it is this old, unless an instance sets user.incus-beacon.cooldown (default: off)")
	fs.BoolVar(&s.Update, "update", false, "have the instance apply updates to instances that set user.incus-beacon.update=auto")
	fs.StringVar(&s.UpdateWindow, "update-window", "", "daily HH:MM-HH:MM, in the instance's local time (UTC unless TZ is set), when its updates may run (default: any time)")
}

// settingFlags names the flags bindSettings binds, for Configure.Given.
var settingFlags = func() []string {
	fs := flag.NewFlagSet("", flag.ContinueOnError)
	bindSettings(fs, &Common{}, &Settings{})
	var names []string
	fs.VisitAll(func(f *flag.Flag) { names = append(names, f.Name) })
	return names
}()

func (s *Settings) validate() error {
	var errs []error
	if s.Track != "" && s.Track != "digest" && s.Track != "semver" {
		errs = append(errs, fmt.Errorf("track %q: must be digest or semver", s.Track))
	}
	if s.Interval != 0 && s.Interval < time.Minute {
		errs = append(errs, fmt.Errorf("interval %s: must be at least 1m", s.Interval))
	}
	if s.Cooldown < 0 {
		errs = append(errs, fmt.Errorf("cooldown %s: must not be negative", s.Cooldown))
	}
	if s.UpdateWindow != "" {
		if _, err := policy.ParseWindow(s.UpdateWindow); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// CheckServeEnv parses serve from env, the BEACON_* variables an instance would run with.
func CheckServeEnv(env map[string]string) error {
	_, err := Parse([]string{CmdServe}, func(k string) string { return env[k] }, io.Discard)
	return err
}

func bindUpdate(fs *flag.FlagSet, c *UpdateFlags) {
	fs.DurationVar(&c.StopTimeout, "update-stop-timeout", 30*time.Second, "how long an instance may take to stop before it is forced")
	fs.DurationVar(&c.Wait, "update-wait", 5*time.Minute, "how long an updated instance has to become healthy before it is rolled back")
	fs.DurationVar(&c.Settle, "update-settle", 30*time.Second, "how long an updated instance must keep running")
	fs.IntVar(&c.Keep, "update-keep", 3, "how many of beacon's snapshots to keep per volume; 0 keeps all")
}

func (c *UpdateFlags) validate() error {
	var errs []error
	for name, d := range map[string]time.Duration{"update-stop-timeout": c.StopTimeout, "update-wait": c.Wait, "update-settle": c.Settle} {
		if d <= 0 {
			errs = append(errs, fmt.Errorf("%s %s: must be positive", name, d))
		}
	}
	if c.Settle >= c.Wait {
		errs = append(errs, fmt.Errorf("update-settle %s: must be shorter than update-wait %s", c.Settle, c.Wait))
	}
	if c.Keep < 0 {
		errs = append(errs, fmt.Errorf("update-keep %d: must not be negative", c.Keep))
	}
	return errors.Join(errs...)
}

func bindAudit(fs *flag.FlagSet, c *Audit) {
	c.AuditLogMaxSize = 10 << 20
	fs.StringVar(&c.AuditLog, "audit-log", "", "file to append a JSON line to per update step, undo, snapshot and outcome (default: none)")
	fs.Var((*sizeValue)(&c.AuditLogMaxSize), "audit-log-max-size", "`size` past which the audit log is rotated: bytes, or a whole number of KiB or MiB")
	fs.IntVar(&c.AuditLogKeep, "audit-log-keep", 5, "how many rotated audit logs to keep, <file>.1 being the newest; 0 keeps none")
}

func (c *Audit) validate() error {
	var errs []error
	if c.AuditLogMaxSize <= 0 {
		errs = append(errs, fmt.Errorf("audit-log-max-size %d: must be positive", c.AuditLogMaxSize))
	}
	if c.AuditLogKeep < 0 {
		errs = append(errs, fmt.Errorf("audit-log-keep %d: must not be negative", c.AuditLogKeep))
	}
	return errors.Join(errs...)
}

func bindConcurrency(fs *flag.FlagSet, c *Registry) {
	fs.IntVar(&c.Concurrency, "concurrency", 4, "registry lookups run at once")
}

func bindRegistry(fs *flag.FlagSet, c *Registry) {
	fs.StringVar(&c.RegistryAuthFile, "registry-auth-file", "", "docker config.json with registry credentials")
	fs.DurationVar(&c.Timeout, "timeout", 30*time.Second, "timeout for each registry call")
	fs.DurationVar(&c.Cooldown, "cooldown", 0, "report an image only once it is this old, unless an instance sets user.incus-beacon.cooldown (default: off)")
}

func (c *Common) validate() error {
	errs := []error{
		oneOf("log-level", c.LogLevel, logLevels...),
		oneOf("log-format", c.LogFormat, FormatConsole, FormatJSON),
		oneOf("log-color", c.LogColor, ColorAuto, ColorNever, ColorAlways),
		oneOf("watch", c.Watch, WatchAll, WatchOptIn),
	}
	if (c.ClientCert == "") != (c.ClientKey == "") {
		errs = append(errs, errors.New("client-cert and client-key must be given together"))
	}
	if c.Remote != "" && c.Incus != "" {
		errs = append(errs, errors.New("remote and incus are mutually exclusive"))
	}
	if c.Token != "" && c.TokenFile != "" {
		errs = append(errs, errors.New("token and token-file are mutually exclusive"))
	}
	if c.Token != "" || c.TokenFile != "" {
		if c.Incus == "" {
			errs = append(errs, errors.New("token needs incus"))
		}
		if c.StateDir == "" {
			errs = append(errs, errors.New("token needs state-dir"))
		}
		if c.ClientCert != "" {
			errs = append(errs, errors.New("token and client-cert are mutually exclusive"))
		}
	}
	return errors.Join(errs...)
}

func (c *Registry) validate() error {
	var errs []error
	if c.Concurrency < 1 {
		errs = append(errs, fmt.Errorf("concurrency %d: must be at least 1", c.Concurrency))
	}
	if c.Timeout <= 0 {
		errs = append(errs, fmt.Errorf("timeout %s: must be positive", c.Timeout))
	}
	if c.Cooldown < 0 {
		errs = append(errs, fmt.Errorf("cooldown %s: must not be negative", c.Cooldown))
	}
	return errors.Join(errs...)
}

func oneOf(name, v string, allowed ...string) error {
	if slices.Contains(allowed, v) {
		return nil
	}
	return fmt.Errorf("%s %q: must be one of %s", name, v, strings.Join(allowed, ", "))
}

func applyEnv(fs *flag.FlagSet, getenv func(string) string, skip []string) error {
	set := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { set[f.Name] = true })
	var errs []error
	fs.VisitAll(func(f *flag.Flag) {
		if set[f.Name] || slices.Contains(skip, f.Name) {
			return
		}
		env := EnvName(f.Name)
		v := getenv(env)
		if v == "" {
			return
		}
		if err := fs.Set(f.Name, v); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", env, err))
		}
	})
	return errors.Join(errs...)
}

type csvValue []string

func (c *csvValue) String() string { return strings.Join(*c, ",") }

func (c *csvValue) Set(v string) error {
	*c = (*c)[:0]
	for p := range strings.SplitSeq(v, ",") {
		if p = strings.TrimSpace(p); p != "" {
			*c = append(*c, p)
		}
	}
	return nil
}

// sizeValue is a byte count, optionally with a KiB or MiB suffix.
type sizeValue int64

var sizeUnits = []struct {
	suffix string
	shift  uint
}{{"MiB", 20}, {"KiB", 10}}

func (v *sizeValue) String() string {
	n := int64(*v)
	for _, u := range sizeUnits {
		if n != 0 && n%(1<<u.shift) == 0 {
			return strconv.FormatInt(n>>u.shift, 10) + u.suffix
		}
	}
	return strconv.FormatInt(n, 10)
}

func (v *sizeValue) Set(s string) error {
	num, shift := s, uint(0)
	for _, u := range sizeUnits {
		if n, ok := strings.CutSuffix(s, u.suffix); ok {
			num, shift = n, u.shift
			break
		}
	}
	n, err := strconv.ParseInt(num, 10, 64)
	if err != nil || n > math.MaxInt64>>shift {
		return fmt.Errorf("%q: want bytes, or a whole number of KiB or MiB", s)
	}
	*v = sizeValue(n << shift)
	return nil
}

// urlsValue appends: a repeated flag adds URLs, and spaces separate them in one value.
type urlsValue []string

func (u *urlsValue) String() string { return strings.Join(*u, " ") }

func (u *urlsValue) Set(v string) error {
	*u = append(*u, strings.Fields(v)...)
	return nil
}

type remoteValue struct {
	c       *Common
	project bool
}

func (r *remoteValue) String() string {
	switch {
	case r.c == nil:
		return ""
	case r.c.RemoteProject == "":
		return r.c.Remote
	}
	return r.c.Remote + ":" + r.c.RemoteProject
}

func (r *remoteValue) Set(v string) error {
	name, project, found := strings.Cut(v, ":")
	switch {
	case !found:
	case !r.project:
		return errors.New("a project is only taken by install and uninstall; --project picks the projects to watch")
	case name == "" || project == "":
		return errors.New("want <remote>:<project>, with both parts")
	}
	r.c.Remote, r.c.RemoteProject = name, project
	return nil
}
