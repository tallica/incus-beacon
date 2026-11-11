// Package policy reads the user-set user.incus-beacon.* and label keys.
package policy

import (
	"errors"
	"fmt"
	"regexp"
	"strconv"

	mmsemver "github.com/Masterminds/semver/v3"

	"github.com/tallica/incus-beacon/internal/semver"
)

const (
	TrackDigest = "digest"
	TrackSemver = "semver"
)

// Key prefixes, newest first; the user.beacon. forms are read until 0.7.
const (
	Prefix            = "user.incus-beacon."
	LabelPrefix       = "user.label.incus-beacon."
	LegacyPrefix      = "user.beacon."
	LegacyLabelPrefix = "user.label.beacon."
)

var names = []string{"enable", "image", "track", "constraint", "include", "exclude"}

type Policy struct {
	Watched    bool
	Image      string
	Track      string
	Constraint *mmsemver.Constraints
	Include    *regexp.Regexp
	Exclude    *regexp.Regexp
}

// Key falls back to the incus-compose label form, then to the old names.
func Key(config map[string]string, name string) string {
	for _, p := range []string{Prefix, LabelPrefix, LegacyPrefix, LegacyLabelPrefix} {
		if v := config[p+name]; v != "" {
			return v
		}
	}
	return ""
}

// Legacy lists the policy keys config sets under their old names.
func Legacy(config map[string]string) []string {
	var keys []string
	for _, p := range []string{LegacyPrefix, LegacyLabelPrefix} {
		for _, n := range names {
			if config[p+n] != "" {
				keys = append(keys, p+n)
			}
		}
	}
	return keys
}

// Parse still sets Watched on error, so a bad policy is reported, not hidden.
func Parse(config map[string]string, watchAll bool) (Policy, error) {
	p := Policy{Watched: watchAll, Image: Key(config, "image"), Track: TrackDigest}
	var errs []error

	if v := Key(config, "enable"); v != "" {
		b, err := strconv.ParseBool(v)
		if err != nil {
			errs = append(errs, fmt.Errorf("enable %q: not a boolean", v))
		} else {
			p.Watched = b
		}
	}
	switch v := Key(config, "track"); v {
	case "", TrackDigest:
	case TrackSemver:
		p.Track = TrackSemver
	default:
		errs = append(errs, fmt.Errorf("track %q: must be %s or %s", v, TrackDigest, TrackSemver))
	}
	if v := Key(config, "constraint"); v != "" {
		c, err := semver.ParseConstraint(v)
		if err != nil {
			errs = append(errs, err)
		} else {
			p.Constraint = c
		}
	}

	var err error
	if p.Include, err = compile(config, "include"); err != nil {
		errs = append(errs, err)
	}
	if p.Exclude, err = compile(config, "exclude"); err != nil {
		errs = append(errs, err)
	}
	return p, errors.Join(errs...)
}

func compile(config map[string]string, name string) (*regexp.Regexp, error) {
	v := Key(config, name)
	if v == "" {
		return nil, nil
	}
	re, err := regexp.Compile(v)
	if err != nil {
		return nil, fmt.Errorf("%s %q: %w", name, v, err)
	}
	return re, nil
}
