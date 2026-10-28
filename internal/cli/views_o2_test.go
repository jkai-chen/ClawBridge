package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"clawbridge/internal/bus"
	"clawbridge/internal/clirender"
	"clawbridge/internal/doctor"
)

// O2's views (live/CLI-OUTPUT-PLAN.md §5): whoami, doctor and a message's
// status run through Run against a local store; the account lists, which
// need a hosted workspace, through their views with fixtures. The human
// goldens are at width 100 with no colour (a buffer is neither a terminal
// nor coloured). Regenerate with CLAWBRIDGE_UPDATE_HUMAN_GOLDEN=1.
func TestO2HumanGolden(t *testing.T) {
	pkg, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	dir := isolated(t)
	now := time.Now()
	old := viewNow
	viewNow = func() time.Time { return now }
	t.Cleanup(func() { viewNow = old })
	normalise := func(s string) string {
		s = strings.ReplaceAll(s, dir, "<DIR>")
		if real, err := filepath.EvalSymlinks(dir); err == nil {
			s = strings.ReplaceAll(s, real, "<DIR>")
		}
		for _, v := range jsonGuardVolatile {
			s = v.re.ReplaceAllString(s, v.with)
		}
		s = regexp.MustCompile(`\(\d{4}-\d{2}-\d{2} \d{2}:\d{2} UTC\)`).ReplaceAllString(s, "(<UTC>)")
		s = regexp.MustCompile(`\b(msg|ses|prj|ws)_[0-9a-f]{32}\b`).ReplaceAllString(s, "<ID>")
		// This OS user's principal, as printed (on Windows a SID and
		// DOMAIN\user, so not a pattern).
		var me string
		_ = json.Unmarshal([]byte(localPrincipalJSON(nil)), &me)
		return strings.ReplaceAll(s, me, "local:<USER>")
	}
	var got strings.Builder
	human := func(args ...string) string {
		t.Helper()
		t.Setenv("CLAWBRIDGE_OUTPUT", "human")
		defer t.Setenv("CLAWBRIDGE_OUTPUT", "")
		var out, errs bytes.Buffer
		// doctor exits non-zero on a required failure; its output is
		// what's under test.
		_ = Run(context.Background(), args, &out, &errs)
		got.WriteString("### " + normalise(strings.Join(args, " ")) + "\n" + normalise(out.String()))
		return out.String()
	}
	invoke(t, "setup", "--local", "--workspace", "w")
	invoke(t, "project", "create", "p")
	t.Setenv("CLAWBRIDGE_PROJECT", "p")
	invoke(t, "register", "a", "--instance", "a-one")
	invoke(t, "register", "b", "--instance", "b-one")
	var sent bytes.Buffer
	if err := Run(context.Background(), []string{"--session", "a", "send", "b", "hello", "--instance", "a-one", "--operation-id", "one"}, &sent, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	id := regexp.MustCompile(`"id":"([^"]+)"`).FindStringSubmatch(sent.String())[1]
	invoke(t, "--session", "b", "--instance", "b-one", "mark-read", id)

	// whoami prints this OS user's principal, whose length (on Windows a
	// SID and DOMAIN\user) moves the layout's wrap; so the golden renders
	// its result with the principal pinned before the layout.
	whoArgs := []string{"whoami", "--session", "a", "--instance", "a-one"}
	var whoJSON bytes.Buffer
	if err := Run(context.Background(), whoArgs, &whoJSON, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	pinned := strings.ReplaceAll(whoJSON.String(), localPrincipalJSON(nil), `"local:<USER>"`)
	if pinned == whoJSON.String() {
		t.Fatal("whoami's JSON doesn't carry this OS user's principal", whoJSON.String())
	}
	var whoOut bytes.Buffer
	if err := clirender.Write(&whoOut, clirender.Human, "whoami", json.RawMessage(pinned)); err != nil {
		t.Fatal(err)
	}
	who := whoOut.String()
	got.WriteString("### " + strings.Join(whoArgs, " ") + "\n" + normalise(who))
	status := human("status", id, "--session=a", "--instance=a-one")
	// Doctor's checks depend on the machine (its installed skills), so
	// its layout is pinned by TestDoctorViewOrder; here only its shape.
	t.Setenv("CLAWBRIDGE_OUTPUT", "human")
	var docOut bytes.Buffer
	_ = Run(context.Background(), []string{"doctor"}, &docOut, &bytes.Buffer{})
	t.Setenv("CLAWBRIDGE_OUTPUT", "")
	doc := docOut.String()

	day := 24 * time.Hour
	stamp := func(d time.Duration) string { return now.Add(d).UTC().Format(time.RFC3339Nano) }
	for _, c := range []struct {
		name string
		data map[string]any
	}{
		{"invite_list", map[string]any{"invites": []any{
			map[string]any{"invite_id": "inv_7Q2M", "email": "ada@example.test", "purpose": "user", "status": "pending", "expires_at": stamp(2 * day), "grants": map[string]any{"core": "member", "api": "owner"}},
			map[string]any{"invite_id": "inv_3KD9", "email": "lin@example.test", "purpose": "device", "status": "used", "expires_at": stamp(-day), "grants": map[string]any{}},
		}}},
		{"invite_list", map[string]any{"invites": []any{}}},
		{"device_list", map[string]any{"devices": []any{
			map[string]any{"device_id": "dev_LAPTOP1", "label": "laptop", "kind": "device", "status": "active", "last_used_at": stamp(-4 * time.Minute), "created_at": stamp(-30 * day)},
			map[string]any{"device_id": "dev_OLDMAC2", "label": "old mac", "kind": "device", "status": "active", "dormant_at": stamp(3 * day), "dormant_soon": true, "last_used_at": stamp(-57 * day), "created_at": stamp(-200 * day)},
			map[string]any{"device_id": "dev_CI00003", "label": "", "kind": "ci", "status": "revoked", "created_at": stamp(-2 * day)},
		}}},
		{"user_list", map[string]any{"users": []any{
			map[string]any{"principal": "owner@example.test", "owner": true, "added_at": stamp(-90 * day), "roles": []any{map[string]any{"slug": "core", "role": "owner"}},
				"devices": []any{map[string]any{"status": "active"}, map[string]any{"status": "revoked"}}},
			map[string]any{"principal": "ada@example.test", "invited_by": "owner@example.test", "added_at": stamp(-3 * time.Hour), "roles": []any{}, "devices": []any{}},
		}}},
		{"owner_transfer_get", map[string]any{"pending": nil}},
		{"owner_transfer_get", map[string]any{"pending": map[string]any{"from": "owner@example.test", "to": "ada@example.test", "created_at": stamp(-time.Hour), "expires_at": stamp(7 * day)}}},
	} {
		var out bytes.Buffer
		if err := clirender.Write(&out, clirender.Human, c.name, bus.Result{Source: "server", Data: c.data}); err != nil {
			t.Fatal(err)
		}
		got.WriteString("### " + c.name + "\n" + normalise(out.String()))
	}

	golden := filepath.Join(pkg, "testdata", "human", "o2.golden")
	if os.Getenv("CLAWBRIDGE_UPDATE_HUMAN_GOLDEN") == "1" {
		if err := os.MkdirAll(filepath.Dir(golden), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(golden, []byte(got.String()), 0o644); err != nil {
			t.Fatal(err)
		}
	} else if want, err := os.ReadFile(golden); err != nil || got.String() != string(want) {
		t.Fatalf("the human output changed:\n--- got\n%s\n--- want\n%s", got.String(), want)
	}

	// What the goldens show, asserted so a regenerated golden can't lose it.
	for _, c := range []struct{ out, want string }{
		{who, "Signed in as"}, {who, "Session"}, {who, "last seen just now"},
		{status, "ID "}, {status, id}, {status, "TO"}, {status, "READ"},
		{doc, "Doctor: "}, {doc, "PASS Trusted configuration"},
	} {
		if !strings.Contains(c.out, c.want) || strings.HasPrefix(c.out, "{") {
			t.Fatalf("no %q in\n%s", c.want, c.out)
		}
	}
}

// Shape D: failures first, the action under its check, passes without one.
func TestDoctorViewOrder(t *testing.T) {
	report := doctor.Report{Status: doctor.Fail, Checks: []doctor.Check{
		{ID: "a", Title: "Config", Status: doctor.Pass, Message: "found", Action: "nothing to do"},
		{ID: "b", Title: "Mirror", Status: doctor.Warn, Message: "2 days old", Action: "run clawbridge sync"},
		{ID: "c", Title: "Server", Status: doctor.Fail, Message: "unreachable", Path: "/etc/x", Action: "check the URL"},
	}}
	var buf bytes.Buffer
	if err := clirender.Write(&buf, clirender.Human, "doctor", report); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	want := "Doctor: 1 pass · 1 warn · 1 fail\n" +
		"FAIL Server: unreachable\n     /etc/x\n     → check the URL\n" +
		"WARN Mirror: 2 days old\n     → run clawbridge sync\n" +
		"PASS Config: found\n\n" +
		"Doctor found failures that need fixing; the actions above say how.\n"
	if out != want {
		t.Fatalf("%q\n%q", out, want)
	}
	if _, ok := doctorView(map[string]any{"status": "pass"}); ok {
		t.Fatal("took a map for a report")
	}
}

// R1: data from outside is escaped in every O2 view.
func TestO2ViewsEscape(t *testing.T) {
	bad := "x\x1b[2J\u202e\a"
	for name, data := range map[string]any{
		"whoami":             map[string]any{"principal": bad, "identity_provenance": bad, "workspace": map[string]any{"name": bad}, "session": map[string]any{"name": bad, "role": bad}},
		"message_status":     map[string]any{"id": bad, "deliveries": []any{map[string]any{"recipient": bad}}},
		"invite_list":        map[string]any{"invites": []any{map[string]any{"invite_id": bad, "email": bad, "grants": map[string]any{bad: bad}}}},
		"device_list":        map[string]any{"devices": []any{map[string]any{"device_id": bad, "label": bad, "status": bad}}},
		"user_list":          map[string]any{"users": []any{map[string]any{"principal": bad, "roles": []any{map[string]any{"slug": bad, "role": bad}}}}},
		"owner_transfer_get": map[string]any{"pending": map[string]any{"from": bad, "to": bad}},
	} {
		var out bytes.Buffer
		if err := clirender.Write(&out, clirender.Human, name, bus.Result{Data: data}); err != nil {
			t.Fatal(err)
		}
		if s := out.String(); strings.HasPrefix(s, "{") || strings.ContainsAny(s, "\x1b\a\u202e") || !strings.Contains(s, `x\x1b[2J`) {
			t.Fatalf("%s:\n%q", name, s)
		}
	}
	var out bytes.Buffer
	_ = clirender.Write(&out, clirender.Human, "doctor", doctor.Report{Checks: []doctor.Check{{Title: bad, Status: doctor.Fail, Message: bad, Action: bad}}})
	if strings.ContainsAny(out.String(), "\x1b\a\u202e") {
		t.Fatalf("%q", out.String())
	}
}

// The coordinator's whoami says where its playbook is, and whether it
// counts yet; no other whoami has the line.
func TestWhoamiShowsTheCoordinatorsPlaybook(t *testing.T) {
	cases := map[string]struct {
		pb   map[string]any
		want string
	}{
		"merged":    {map[string]any{"path": "docs/COORDINATOR.md", "merged": true, "local": true, "ref": "origin/main"}, "your project's playbook is docs/COORDINATOR.md on origin/main"},
		"only here": {map[string]any{"path": "docs/COORDINATOR.md", "merged": false, "local": true, "ref": "origin/main"}, "doesn't count until merged"},
		"none":      {map[string]any{"path": "docs/COORDINATOR.md", "merged": false, "local": false, "ref": "origin/main"}, "has no docs/COORDINATOR.md yet"},
		"no ref":    {map[string]any{"path": "docs/COORDINATOR.md", "merged": false, "local": true, "note": "the checkout has no origin/HEAD"}, "no origin/HEAD"},
	}
	for name, tc := range cases {
		if got := playbookLine(tc.pb); !strings.Contains(got, tc.want) {
			t.Fatalf("%s: %q", name, got)
		}
	}
	base := map[string]any{"principal": "p@example.test", "workspace": map[string]any{"name": "w"}, "online": true}
	d, ok := whoamiView(map[string]any{"data": base})
	if !ok || strings.Contains(fmt.Sprint(d), "Coordinator") {
		t.Fatal("a non-coordinator's whoami showed the playbook")
	}
	base["coordinator_playbook"] = cases["merged"].pb
	if d, ok = whoamiView(map[string]any{"data": base}); !ok || !strings.Contains(fmt.Sprint(d), "on origin/main") {
		t.Fatalf("%+v", d)
	}
}
