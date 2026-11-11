package policy

import (
	"strings"
	"testing"
)

func TestDefaults(t *testing.T) {
	t.Parallel()
	p, err := Parse(map[string]string{}, true)
	if err != nil {
		t.Fatal(err)
	}
	if !p.Watched || p.Track != TrackDigest || p.Image != "" || p.Include != nil || p.Exclude != nil {
		t.Errorf("defaults: %+v", p)
	}
}

func TestUserKeyBeatsLabel(t *testing.T) {
	t.Parallel()
	p, err := Parse(map[string]string{
		"user.incus-beacon.image":       "example.com/a:1",
		"user.label.incus-beacon.image": "example.com/b:1",
		"user.label.incus-beacon.track": "semver",
	}, true)
	if err != nil {
		t.Fatal(err)
	}
	if p.Image != "example.com/a:1" {
		t.Errorf("image %q, want the user.incus-beacon key", p.Image)
	}
	if p.Track != TrackSemver {
		t.Errorf("track %q, want the label's semver", p.Track)
	}
}

func TestLegacyNames(t *testing.T) {
	t.Parallel()
	cfg := map[string]string{
		"user.incus-beacon.image":   "example.com/new:1",
		"user.beacon.image":         "example.com/old:1",
		"user.label.beacon.track":   "semver",
		"user.beacon.status":        "current", // written by beacon, not a policy key
		"user.label.beacon.unknown": "x",
	}
	p, err := Parse(cfg, true)
	if err != nil {
		t.Fatal(err)
	}
	if p.Image != "example.com/new:1" || p.Track != TrackSemver {
		t.Errorf("policy %+v, want the new image name to win and the old label read", p)
	}
	if got, want := strings.Join(Legacy(cfg), " "), "user.beacon.image user.label.beacon.track"; got != want {
		t.Errorf("legacy %q, want %q", got, want)
	}
	if got := Legacy(map[string]string{"user.incus-beacon.track": "semver"}); got != nil {
		t.Errorf("legacy %q with only new names", got)
	}
}

func TestWatch(t *testing.T) {
	t.Parallel()
	tests := []struct {
		enable   string
		watchAll bool
		want     bool
	}{
		{"", true, true},
		{"", false, false},
		{"false", true, false},
		{"true", false, true},
	}
	for _, tt := range tests {
		cfg := map[string]string{}
		if tt.enable != "" {
			cfg["user.incus-beacon.enable"] = tt.enable
		}
		p, err := Parse(cfg, tt.watchAll)
		if err != nil {
			t.Fatal(err)
		}
		if p.Watched != tt.want {
			t.Errorf("enable %q, watchAll %v: watched %v, want %v", tt.enable, tt.watchAll, p.Watched, tt.want)
		}
	}
}

func TestErrors(t *testing.T) {
	t.Parallel()
	tests := []struct {
		key, value, want string
	}{
		{"enable", "maybe", "not a boolean"},
		{"track", "latest", "must be digest or semver"},
		{"include", "(", "include"},
		{"exclude", "[", "exclude"},
		{"constraint", "~>>1", "constraint"},
	}
	for _, tt := range tests {
		p, err := Parse(map[string]string{"user.incus-beacon." + tt.key: tt.value}, true)
		if err == nil || !strings.Contains(err.Error(), tt.want) {
			t.Errorf("%s=%q: err %v, want it to mention %q", tt.key, tt.value, err, tt.want)
		}
		if !p.Watched {
			t.Errorf("%s=%q: a policy error must not unwatch the instance", tt.key, tt.value)
		}
	}
}

func TestRegexes(t *testing.T) {
	t.Parallel()
	p, err := Parse(map[string]string{
		"user.incus-beacon.include": `^1\.`,
		"user.incus-beacon.exclude": `-rc`,
	}, true)
	if err != nil {
		t.Fatal(err)
	}
	if !p.Include.MatchString("1.27") || !p.Exclude.MatchString("1.28-rc1") {
		t.Errorf("regexes not compiled as given: %v %v", p.Include, p.Exclude)
	}
}
