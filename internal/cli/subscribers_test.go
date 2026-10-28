package cli

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

// linear subscribers: any member reads it; changes are the owner's or a
// lead's, never a session's, one at a time at the revision read; nothing
// turns on where the server hasn't proven it (B2); a backfill needs a
// hosted server's app.
func TestCLIIssueSubscribers(t *testing.T) {
	isolated(t)
	invoke(t, "setup", "--local", "--workspace", "w")
	invoke(t, "project", "create", "p")
	t.Setenv("CLAWBRIDGE_PROJECT", "p")
	invoke(t, "register", "eng", "--instance", "eng-i")
	run := func(args ...string) (string, error) {
		var out, errs bytes.Buffer
		e := Run(context.Background(), args, &out, &errs)
		return out.String(), e
	}
	out, e := run("linear", "subscribers", "show")
	if e != nil || !strings.Contains(out, `"proven":false`) || !strings.Contains(out, `"on":false`) || !strings.Contains(out, `"owner":true`) {
		t.Fatal(e, out)
	}
	if out, e = run("--session", "eng", "--instance", "eng-i", "linear", "subscribers", "show"); e != nil {
		t.Fatal("a session can't read it:", e)
	}
	const person = "00000000-0000-4000-8000-0000000000d1"
	if out, e = run("linear", "subscribers", "add", strings.ToUpper(person)); e != nil || !strings.Contains(out, person) || !strings.Contains(out, `"revision":1`) {
		t.Fatal(e, out)
	}
	if out, e = run("linear", "subscribers", "add", person); e != nil || strings.Count(out, person) != 1 {
		t.Fatal("added twice:", e, out)
	}
	if out, e = run("linear", "subscribers", "owner", "off"); e != nil || !strings.Contains(out, `"owner":false`) {
		t.Fatal(e, out)
	}
	if out, e = run("linear", "subscribers", "remove", person); e != nil || strings.Contains(out, person) {
		t.Fatal(e, out)
	}
	for want, args := range map[string][]string{
		"not yet proven":          {"linear", "subscribers", "on"},
		"without --session":       {"--session", "eng", "--instance", "eng-i", "linear", "subscribers", "off"},
		"isn't on the list":       {"linear", "subscribers", "remove", person},
		"Linear user UUID":        {"linear", "subscribers", "add", "someone@example.test"},
		"owner takes on or off":   {"linear", "subscribers", "owner", "maybe"},
		"requires --since":        {"linear", "subscribers", "backfill"},
		"YYYY-MM-DD":              {"linear", "subscribers", "backfill", "--since", "last week"},
		"hosted server's":         {"linear", "subscribers", "backfill", "--since", "2026-10-27"},
		"subscribers show|on|off": {"linear", "subscribers", "drop"},
	} {
		if _, e := run(args...); e == nil || !strings.Contains(e.Error(), want) {
			t.Fatal(args, e)
		}
	}
	if out, e = run("linear", "subscribers", "off"); e != nil || !strings.Contains(out, `"enabled":false`) {
		t.Fatal("off refused:", e, out)
	}
}

func TestSubscribersSince(t *testing.T) {
	for in, want := range map[string]string{"2026-10-27": "2026-10-27T00:00:00Z", "2026-10-27T09:30:00+02:00": "2026-10-27T07:30:00Z"} {
		if got, e := subscribersSince(in); e != nil || got != want {
			t.Fatal(in, got, e)
		}
	}
}
