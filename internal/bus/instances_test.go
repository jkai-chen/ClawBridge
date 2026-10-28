package bus

import "testing"

// Servers are counted by their distinct names; anything that may hide one
// reads unknown, never a guessed 1.
func TestCountServerInstances(t *testing.T) {
	self := ServerInstancePrefix + "0123456789abcdef"
	other := ServerInstancePrefix + "fedcba9876543210"
	for name, c := range map[string]struct {
		names []string
		want  ServerInstances
	}{
		"alone":            {[]string{self, self, self}, ServerInstances{Live: 1, Known: true}},
		"two":              {[]string{self, other, other}, ServerInstances{Live: 2, Known: true}},
		"self unseen":      {[]string{other}, ServerInstances{Live: 1}},
		"none":             {nil, ServerInstances{}},
		"an unnamed one":   {[]string{self, ""}, ServerInstances{Live: 1}},
		"a pooler":         {[]string{self, "pgbouncer"}, ServerInstances{Live: 1, Pooler: true}},
		"short suffix":     {[]string{self, ServerInstancePrefix + "abc"}, ServerInstances{Live: 1, Known: true}},
		"another app name": {[]string{self, "psql"}, ServerInstances{Live: 1, Known: true}},
	} {
		if got := countServerInstances(c.names, self); got != c.want {
			t.Errorf("%s: %+v, want %+v", name, got, c.want)
		}
	}
}
