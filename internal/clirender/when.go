package clirender

import (
	"fmt"
	"time"
)

// When is a stored time for a list: relative to now ("4 min ago", "in 2
// days"). A value that isn't a time is shown as it is (owner, Q6; TUI-UX-AUDIT U37). The CLI
// and the TUI both show times through it.
func When(now time.Time, stored string) string {
	t, ok := parseStored(stored)
	if !ok {
		return stored
	}
	d := now.Sub(t)
	future := d < 0
	if future {
		d = -d
	}
	var n string
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		n = fmt.Sprintf("%d min", int(d/time.Minute))
	case d < 48*time.Hour:
		n = fmt.Sprintf("%d h", int(d/time.Hour))
	default:
		n = fmt.Sprintf("%d days", int(d/(24*time.Hour)))
	}
	if future {
		return "in " + n
	}
	return n + " ago"
}

// WhenExact is a stored time for details: relative, then absolute UTC.
func WhenExact(now time.Time, stored string) string {
	t, ok := parseStored(stored)
	if !ok {
		return stored
	}
	return When(now, stored) + " (" + t.UTC().Format("2006-01-02 15:04 UTC") + ")"
}

func parseStored(s string) (time.Time, bool) {
	for _, layout := range []string{time.RFC3339Nano, "2006-01-02 15:04:05"} {
		if t, err := time.Parse(layout, s); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}
