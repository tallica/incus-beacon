package policy

import (
	"testing"
	"time"
)

func TestParseWindow(t *testing.T) {
	t.Parallel()
	for _, bad := range []string{"", "02:00", "02:00-02:00", "2-5", "25:00-03:00", "02:00-03:60", "02:00-03:00-04:00"} {
		if _, err := ParseWindow(bad); err == nil {
			t.Errorf("ParseWindow(%q) accepted", bad)
		}
	}
	w, err := ParseWindow(" 02:30 - 05:00 ")
	if err != nil || w.String() != "02:30-05:00" {
		t.Errorf("got %v, %v", w, err)
	}
}

func TestWindowContains(t *testing.T) {
	t.Parallel()
	at := func(hm string) time.Time {
		tm, err := time.ParseInLocation("15:04", hm, time.Local)
		if err != nil {
			t.Fatal(err)
		}
		return tm
	}
	tests := []struct {
		window, at string
		want       bool
	}{
		{"02:00-05:00", "02:00", true},
		{"02:00-05:00", "04:59", true},
		{"02:00-05:00", "05:00", false},
		{"02:00-05:00", "01:59", false},
		{"22:00-04:00", "23:30", true},
		{"22:00-04:00", "00:00", true},
		{"22:00-04:00", "03:59", true},
		{"22:00-04:00", "04:00", false},
		{"22:00-04:00", "12:00", false},
	}
	for _, tt := range tests {
		w, err := ParseWindow(tt.window)
		if err != nil {
			t.Fatal(err)
		}
		if got := w.Contains(at(tt.at)); got != tt.want {
			t.Errorf("%s contains %s = %t, want %t", tt.window, tt.at, got, tt.want)
		}
	}
}

func TestWindowContainsUsesTheTimesOwnClock(t *testing.T) {
	t.Parallel()
	w, _ := ParseWindow("02:00-05:00")
	zone := time.FixedZone("UTC+3", 3*60*60)
	if !w.Contains(time.Date(2026, 1, 1, 4, 59, 59, 0, time.UTC)) ||
		w.Contains(time.Date(2026, 1, 1, 5, 0, 30, 0, time.UTC)) {
		t.Error("seconds should not move a time across the window's minute edges")
	}
	if !w.Contains(time.Date(2026, 1, 1, 0, 30, 0, 0, time.UTC).In(zone)) {
		t.Error("00:30 UTC is 03:30 in UTC+3, inside the window")
	}
	if !(Window{}).Contains(time.Date(2026, 1, 1, 13, 0, 0, 0, time.UTC)) {
		t.Error("the zero window should contain every time")
	}
}
