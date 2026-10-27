package clirender

import (
	"testing"
	"time"
)

// U37, Q6: relative in lists, relative and UTC in details; anything that
// isn't a stored time is left as it is.
func TestWhen(t *testing.T) {
	now := time.Date(2026, 10, 27, 16, 0, 0, 0, time.UTC)
	for stored, want := range map[string]string{
		"2026-10-27T15:59:30Z":           "just now",
		"2026-10-27T15:55:59.123456789Z": "4 min ago",
		"2026-10-27T13:00:00+01:00":      "4 h ago",
		"2026-10-24T16:00:00Z":           "3 days ago",
		"2026-10-29T16:00:01Z":           "in 2 days",
		"2026-10-27 16:30:00":            "in 30 min",
		"never":                          "never",
		"":                               "",
	} {
		if got := When(now, stored); got != want {
			t.Errorf("%q: %q, want %q", stored, got, want)
		}
	}
	if got := WhenExact(now, "2026-10-27T16:31:00+01:00"); got != "29 min ago (2026-10-27 15:31 UTC)" {
		t.Fatal(got)
	}
	if got := WhenExact(now, "-"); got != "-" {
		t.Fatal(got)
	}
}
