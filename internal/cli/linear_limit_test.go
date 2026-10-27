package cli

import (
	"bytes"
	"strings"
	"testing"
)

// linear projects refuses a page Linear would refuse (above 50) with a
// message saying so; teams and issues still take 100.
func TestLinearProjectsLimitIsAtMostFifty(t *testing.T) {
	if _, _, err := parseLinear("projects", []string{"--profile", "native", "--limit", "51"}, &bytes.Buffer{}); err == nil || !strings.Contains(err.Error(), "at most 50") || !strings.Contains(err.Error(), "--cursor") {
		t.Fatal(err)
	}
	if v, _, err := parseLinear("projects", []string{"--profile", "native", "--limit", "50"}, &bytes.Buffer{}); err != nil || v.limit != 50 {
		t.Fatal(err)
	}
	for _, command := range []string{"teams", "issues"} {
		if v, _, err := parseLinear(command, []string{"--profile", "native", "--limit", "100"}, &bytes.Buffer{}); err != nil || v.limit != 100 {
			t.Fatal(command, err)
		}
	}
	if _, _, err := parseLinear("teams", []string{"--profile", "native", "--limit", "101"}, &bytes.Buffer{}); err == nil {
		t.Fatal("a 101 page")
	}
}
