// Package semver picks the newest tag that keeps the current tag's shape.
package semver

import (
	"fmt"
	"regexp"
	"strconv"

	mmsemver "github.com/Masterminds/semver/v3"
)

// Suffix is kept whole ("-alpine", "-rc1"): only tags of one family compare.
type Version struct {
	Tag    string
	Prefix string
	Parts  []int
	Suffix string
	padded *mmsemver.Version
}

var tagShape = regexp.MustCompile(`^(v?)(\d+)(?:\.(\d+))?(?:\.(\d+))?(.*)$`)

// Parse reads tags such as 1, 1.27, v1.27.3 and 1.27.3-alpine.
func Parse(tag string) (Version, bool) {
	m := tagShape.FindStringSubmatch(tag)
	if m == nil {
		return Version{}, false
	}
	v := Version{Tag: tag, Prefix: m[1], Suffix: m[5]}
	for _, p := range m[2:5] {
		if p == "" {
			break
		}
		n, err := strconv.Atoi(p)
		if err != nil {
			return Version{}, false
		}
		v.Parts = append(v.Parts, n)
	}
	if v.Suffix != "" && !validSuffix(v.Suffix) {
		return Version{}, false
	}
	padded := make([]int, 3)
	copy(padded, v.Parts)
	var err error
	v.padded, err = mmsemver.NewVersion(fmt.Sprintf("%d.%d.%d", padded[0], padded[1], padded[2]))
	return v, err == nil
}

// validSuffix keeps "1.2.3.4" and "12abc" from reading as versions.
func validSuffix(s string) bool {
	return s[0] == '-' || s[0] == '_' || s[0] == '+'
}

func (v Version) SameShape(o Version) bool {
	return v.Prefix == o.Prefix && v.Suffix == o.Suffix && len(v.Parts) == len(o.Parts)
}

func (v Version) Less(o Version) bool {
	for i := range v.Parts {
		if v.Parts[i] != o.Parts[i] {
			return v.Parts[i] < o.Parts[i]
		}
	}
	return false
}

type Filter struct {
	Include    *regexp.Regexp
	Exclude    *regexp.Regexp
	Constraint *mmsemver.Constraints
}

// Newest returns "" when current is newest; ok is false if it isn't a version.
func Newest(current string, tags []string, f Filter) (newest string, ok bool) {
	cur, ok := Parse(current)
	if !ok {
		return "", false
	}
	best := cur
	for _, t := range tags {
		v, ok := Parse(t)
		if !ok || !cur.SameShape(v) || !best.Less(v) {
			continue
		}
		if f.Include != nil && !f.Include.MatchString(t) {
			continue
		}
		if f.Exclude != nil && f.Exclude.MatchString(t) {
			continue
		}
		if f.Constraint != nil && !f.Constraint.Check(v.padded) {
			continue
		}
		best = v
	}
	if best.Tag == cur.Tag {
		return "", true
	}
	return best.Tag, true
}

func ParseConstraint(s string) (*mmsemver.Constraints, error) {
	c, err := mmsemver.NewConstraint(s)
	if err != nil {
		return nil, fmt.Errorf("constraint %q: %w", s, err)
	}
	return c, nil
}
