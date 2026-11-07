// Package config parses the subcommand, its flags and BEACON_* variables.
package config

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"slices"
	"strings"
	"time"
)

const (
	CmdCheck   = "check"
	CmdStatus  = "status"
	CmdServe   = "serve"
	CmdVersion = "version"
)

// Commands lists the subcommands in the order usage shows them.
var Commands = []string{CmdCheck, CmdStatus, CmdServe, CmdVersion}

var summaries = map[string]string{
	CmdCheck:   "check registries for newer images and print what can be updated",
	CmdStatus:  "show the update status already recorded on instances",
	CmdServe:   "run checks on an interval and record results",
	CmdVersion: "print the version",
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
	Incus             string
	ClientCert        string
	ClientKey         string
	ServerFingerprint string
	Token             string
	StateDir          string

	Projects []string
	Watch    string
}

// Registry holds the settings of commands that query registries.
type Registry struct {
	RegistryAuthFile string
	Concurrency      int
	Timeout          time.Duration
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
}

// Config sets at most one command field; none for version.
type Config struct {
	Command string
	Check   *Check
	Status  *Status
	Serve   *Serve
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
	switch name {
	case "-h", "-help", "--help", "help":
		Usage(out)
		return nil, ErrHelp
	case "-version", "--version":
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
		fs.StringVar(&c.Instance, "instance", "", "check only this instance")
		fs.StringVar(&c.Format, "format", OutputTable, "output format: table or json")
		fs.BoolVar(&c.Write, "write", false, "also record results in user.beacon.* keys")
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
		fs.DurationVar(&c.Interval, "interval", 6*time.Hour, "time between check cycles")
		fs.StringVar(&c.Listen, "listen", ":9153", "address for /health and /ready")
		cfg.Serve = c
		validate = func() error {
			errs := []error{c.Common.validate(), c.Registry.validate()}
			if c.Interval < time.Minute {
				errs = append(errs, fmt.Errorf("interval %s: must be at least 1m", c.Interval))
			}
			if c.Listen == "" {
				errs = append(errs, errors.New("listen: must not be empty"))
			}
			return errors.Join(errs...)
		}
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
	if fs.NArg() > 0 {
		return nil, fmt.Errorf("%w: unexpected argument %q", ErrUsage, fs.Arg(0))
	}
	if err := applyEnv(fs, getenv); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrUsage, err)
	}
	if err := validate(); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrUsage, err)
	}
	return cfg, nil
}

func Usage(out io.Writer) {
	fmt.Fprintln(out, "Usage: incus-beacon <command> [flags]")
	fmt.Fprintln(out)
	fmt.Fprintln(out, "Commands:")
	for _, c := range Commands {
		fmt.Fprintf(out, "  %-8s %s\n", c, summaries[c])
	}
	fmt.Fprintln(out)
	fmt.Fprintln(out, "Run 'incus-beacon <command> -h' for its flags. Every flag can also be set")
	fmt.Fprintln(out, "with an environment variable: --log-level is BEACON_LOG_LEVEL.")
}

// EnvName is the environment variable that sets the flag name.
func EnvName(flagName string) string {
	return "BEACON_" + strings.ToUpper(strings.ReplaceAll(flagName, "-", "_"))
}

func bindCommon(fs *flag.FlagSet, c *Common) {
	fs.StringVar(&c.LogLevel, "log-level", "info", "log level: debug, info, warn or error")
	fs.StringVar(&c.LogFormat, "log-format", FormatConsole, "log format: console or json")
	fs.StringVar(&c.LogColor, "log-color", ColorAuto, "console log colour: auto (on a terminal), never or always")

	fs.StringVar(&c.Remote, "remote", "", "incus CLI remote to use (default: the CLI's default remote)")
	fs.StringVar(&c.Incus, "incus", "", "Incus API URL, e.g. https://192.168.1.10:8443")
	fs.StringVar(&c.ClientCert, "client-cert", "", "client certificate file for --incus")
	fs.StringVar(&c.ClientKey, "client-key", "", "client key file for --incus")
	fs.StringVar(&c.ServerFingerprint, "server-fingerprint", "", "expected server certificate fingerprint")
	fs.StringVar(&c.Token, "token", "", "one-time trust token for --incus; enrolls a certificate into --state-dir")
	fs.StringVar(&c.StateDir, "state-dir", "", "directory keeping the certificate --token enrolled")

	fs.Var((*csvValue)(&c.Projects), "project", "project to watch; comma-separate several (default: all)")
	fs.StringVar(&c.Watch, "watch", WatchAll, "which OCI instances to watch: all or opt-in")
}

func bindRegistry(fs *flag.FlagSet, c *Registry) {
	fs.StringVar(&c.RegistryAuthFile, "registry-auth-file", "", "docker config.json with registry credentials")
	fs.IntVar(&c.Concurrency, "concurrency", 4, "registry lookups run at once")
	fs.DurationVar(&c.Timeout, "timeout", 30*time.Second, "timeout for each registry call")
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
	if c.Token != "" {
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
	return errors.Join(errs...)
}

func oneOf(name, v string, allowed ...string) error {
	if slices.Contains(allowed, v) {
		return nil
	}
	return fmt.Errorf("%s %q: must be one of %s", name, v, strings.Join(allowed, ", "))
}

func applyEnv(fs *flag.FlagSet, getenv func(string) string) error {
	set := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { set[f.Name] = true })
	var errs []error
	fs.VisitAll(func(f *flag.Flag) {
		if set[f.Name] {
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
