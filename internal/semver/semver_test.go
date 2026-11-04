package semver

import (
	"regexp"
	"slices"
	"testing"

	mmsemver "github.com/Masterminds/semver/v3"
)

func TestParse(t *testing.T) {
	t.Parallel()
	tests := []struct {
		tag    string
		ok     bool
		parts  []int
		suffix string
	}{
		{"1", true, []int{1}, ""},
		{"1.27", true, []int{1, 27}, ""},
		{"v1.27.3", true, []int{1, 27, 3}, ""},
		{"1.27.3-alpine", true, []int{1, 27, 3}, "-alpine"},
		{"2026.01.15", true, []int{2026, 1, 15}, ""},
		{"latest", false, nil, ""},
		{"1.2.3.4", false, nil, ""},
		{"12abc", false, nil, ""},
	}
	for _, tt := range tests {
		v, ok := Parse(tt.tag)
		if ok != tt.ok {
			t.Errorf("Parse(%q) ok = %v, want %v", tt.tag, ok, tt.ok)
			continue
		}
		if ok && (!slices.Equal(v.Parts, tt.parts) || v.Suffix != tt.suffix) {
			t.Errorf("Parse(%q) = %v %q, want %v %q", tt.tag, v.Parts, v.Suffix, tt.parts, tt.suffix)
		}
	}
}

var tags = []string{
	"1", "1.26", "1.27", "1.28", "2",
	"1.27.3", "1.27.4", "1.28.0", "1.28.1", "2.0.0",
	"1.27.3-alpine", "1.27.5-alpine", "1.28.0-alpine",
	"1.29.0-rc1", "v1.27.9", "latest", "mainline",
}

func TestNewest(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name, current string
		filter        Filter
		want          string
	}{
		{"same precision, patch", "1.27.3", Filter{}, "2.0.0"},
		{"same precision, minor", "1.27", Filter{}, "1.28"},
		{"same precision, major", "1", Filter{}, "2"},
		{"suffix kept", "1.27.3-alpine", Filter{}, "1.28.0-alpine"},
		{"prerelease is another family", "1.28.1", Filter{}, "2.0.0"},
		{"v prefix kept", "v1.27.3", Filter{}, "v1.27.9"},
		{"constraint", "1.27.3", Filter{Constraint: mustConstraint(t, "~1.27")}, "1.27.4"},
		{"constraint major", "1.27.3", Filter{Constraint: mustConstraint(t, "<2")}, "1.28.1"},
		{"include", "1.27.3", Filter{Include: regexp.MustCompile(`^1\.27\.`)}, "1.27.4"},
		{"exclude", "1.27.3", Filter{Exclude: regexp.MustCompile(`^2\.`)}, "1.28.1"},
		{"already newest", "2.0.0", Filter{}, ""},
	}
	for _, tt := range tests {
		got, ok := Newest(tt.current, tags, tt.filter)
		if !ok || got != tt.want {
			t.Errorf("%s: Newest(%q) = %q, %v; want %q", tt.name, tt.current, got, ok, tt.want)
		}
	}
}

func TestNewestNotAVersion(t *testing.T) {
	t.Parallel()
	if _, ok := Newest("latest", tags, Filter{}); ok {
		t.Error("latest accepted as a version")
	}
}

func mustConstraint(t *testing.T, s string) *mmsemver.Constraints {
	t.Helper()
	c, err := ParseConstraint(s)
	if err != nil {
		t.Fatal(err)
	}
	return c
}
