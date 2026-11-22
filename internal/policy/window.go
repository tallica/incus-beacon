package policy

import (
	"fmt"
	"strings"
	"time"
)

// Window is a daily wall-clock span; Start after End wraps midnight, and zero is always.
type Window struct {
	Start, End time.Duration // since midnight
}

// ParseWindow reads HH:MM-HH:MM, such as 02:00-05:00 or 22:00-04:00.
func ParseWindow(s string) (Window, error) {
	from, to, ok := strings.Cut(s, "-")
	start, err := clock(from)
	end, err2 := clock(to)
	if !ok || err != nil || err2 != nil || start == end {
		return Window{}, fmt.Errorf("update-window %q: want HH:MM-HH:MM, such as 02:00-05:00", s)
	}
	return Window{Start: start, End: end}, nil
}

func clock(s string) (time.Duration, error) {
	t, err := time.Parse("15:04", strings.TrimSpace(s))
	if err != nil {
		return 0, err
	}
	return time.Duration(t.Hour())*time.Hour + time.Duration(t.Minute())*time.Minute, nil
}

// Contains reports whether t's wall clock, in t's location, falls inside the window; End is excluded.
func (w Window) Contains(t time.Time) bool {
	at := time.Duration(t.Hour())*time.Hour + time.Duration(t.Minute())*time.Minute
	if w.Start < w.End {
		return at >= w.Start && at < w.End
	}
	return at >= w.Start || at < w.End
}

func (w Window) String() string {
	hm := func(d time.Duration) string { return fmt.Sprintf("%02d:%02d", int(d.Hours()), int(d.Minutes())%60) }
	return hm(w.Start) + "-" + hm(w.End)
}
