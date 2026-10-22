package install

import (
	"strings"
	"testing"
)

func TestLinearProfilePinInNativeMCPEntry(t *testing.T) {
	o := options(t, "codex")
	o.DryRun = true
	o.LinearProfile = "lin_0123456789abcdef0123456789abcdef"
	result, err := Install(o)
	if err != nil {
		t.Fatal(err)
	}
	args := result.Entry["args"].([]string)
	found := false
	for i, arg := range args {
		if arg == "--linear-profile" {
			if i+1 >= len(args) || args[i+1] != o.LinearProfile {
				t.Fatal(args)
			}
			found = true
		}
	}
	if !found {
		t.Fatal("profile pin missing", args)
	}
	o.LinearProfile = "token\nsecret"
	if _, err := Install(o); err == nil || strings.Contains(err.Error(), "secret") {
		t.Fatal("hostile profile not rejected/redacted", err)
	}
}
