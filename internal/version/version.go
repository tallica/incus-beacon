// Package version holds build metadata stamped in by the Makefile and
// GoReleaser via -ldflags.
package version

import "fmt"

var (
	Version = "dev"
	Commit  = ""
	Date    = ""
)

func String() string {
	if Commit == "" {
		return Version
	}
	short := Commit
	if len(short) > 7 {
		short = short[:7]
	}
	return fmt.Sprintf("%s (%s, %s)", Version, short, Date)
}
