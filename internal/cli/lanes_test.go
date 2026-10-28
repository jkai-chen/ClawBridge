package cli

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

// lanes show reads the pins (none yet); pin and unpin are the owner's,
// refused with a session, with neither or both of a session and a
// principal, with the wrong --lane use, and before the board is linked.
func TestCLILanesShowAndPinRefusals(t *testing.T) {
	isolated(t)
	invoke(t, "setup", "--local", "--workspace", "w")
	invoke(t, "project", "create", "p")
	t.Setenv("CLAWBRIDGE_PROJECT", "p")
	invoke(t, "register", "eng", "--instance", "eng-i")
	var out, errs bytes.Buffer
	if e := Run(context.Background(), []string{"linear", "lanes", "show"}, &out, &errs); e != nil || !strings.Contains(out.String(), `"revision":0`) {
		t.Fatal(e, out.String(), errs.String())
	}
	for want, args := range map[string][]string{
		"isn't linked":           {"linear", "lanes", "pin", "--pin-session", "eng", "--lane", "ai", "--expected-revision", "0"},
		"--expected-revision":    {"linear", "lanes", "pin", "--pin-session", "eng", "--lane", "ai"},
		"without --session":      {"--session", "eng", "--instance", "eng-i", "linear", "lanes", "pin", "--pin-session", "eng", "--lane", "ai", "--expected-revision", "0"},
		"takes one of":           {"linear", "lanes", "pin", "--lane", "ai", "--expected-revision", "0"},
		"takes one of ":          {"linear", "lanes", "pin", "--pin-session", "eng", "--pin-principal", "a@example.com", "--lane", "ai", "--expected-revision", "0"},
		"lanes pin takes --lane": {"linear", "lanes", "pin", "--pin-session", "eng", "--expected-revision", "0"},
		"unpin takes none":       {"linear", "lanes", "unpin", "--pin-session", "eng", "--lane", "ai", "--expected-revision", "0"},
		"lanes show|pin|unpin":   {"linear", "lanes", "drop"},
	} {
		out.Reset()
		errs.Reset()
		if e := Run(context.Background(), args, &out, &errs); e == nil || !strings.Contains(e.Error(), strings.TrimSpace(want)) {
			t.Fatal(args, e)
		}
	}
}

// --lane is NAME=TEAM[,TEAM...], the first team the lane's default.
func TestLaneFlags(t *testing.T) {
	lanes, e := laneFlags([]string{"ai=t1,t2", "platform=t3"})
	if e != nil || len(lanes) != 2 || lanes[0].Name != "ai" || lanes[0].DefaultTeamID != "t1" || len(lanes[0].WriteTeamIDs) != 2 || lanes[1].DefaultTeamID != "t3" {
		t.Fatal(lanes, e)
	}
	for _, bad := range []string{"ai", "=t1", "ai="} {
		if _, e := laneFlags([]string{bad}); e == nil {
			t.Fatal(bad, "parsed")
		}
	}
}
