package server

import (
	"testing"
	"time"
)

func TestFormatDuration(t *testing.T) {
	cases := []struct {
		name string
		d    time.Duration
		want string
	}{
		{"days_and_hours", 400*time.Hour + 32*time.Minute + 55*time.Second, "16d16h32m55s"},
		{"hours", 2*time.Hour + 34*time.Minute + 12*time.Second, "2h34m12s"},
		{"minutes", 95 * time.Second, "1m35s"},
		{"seconds", 45 * time.Second, "45s"},
		{"exact_day", 24 * time.Hour, "1d0h0m0s"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := formatDuration(c.d); got != c.want {
				t.Errorf("formatDuration(%v) = %q, want %q", c.d, got, c.want)
			}
		})
	}
}

func TestPlanName(t *testing.T) {
	cases := []struct {
		plan int
		want string
	}{
		{0, "Free"},
		{1, "Essential"},
		{2, "Pro"},
		{3, "Standard"},
		{4, ""},
		{-1, ""},
	}
	for _, c := range cases {
		if got := planName(c.plan); got != c.want {
			t.Errorf("planName(%d) = %q, want %q", c.plan, got, c.want)
		}
	}
}

func TestFormatTBDate(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"rfc3339_utc", "2026-05-12T04:33:22Z", "12 May 2026"},
		{"rfc3339_offset", "2026-12-25T18:00:00+02:00", "25 Dec 2026"},
		{"unparseable", "not-a-date", "not-a-date"},
		{"empty", "", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := formatTBDate(c.in); got != c.want {
				t.Errorf("formatTBDate(%q) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}
