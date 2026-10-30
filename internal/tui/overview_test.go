package tui

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/lipgloss"

	"clawbridge/internal/bus"
)

// overviewFor loads Overview for a profile with these doctor checks and
// these server answers.
func overviewFor(t *testing.T, mode Mode, session bool, checks []Check, results map[string]bus.Result, errs map[string]error, linked bool) Model {
	t.Helper()
	a := &fakeAuthority{results: results, errs: errs}
	cfg := testConfig(a)
	cfg.Mode = mode
	if !session {
		cfg.Session, cfg.Instance = "", ""
	}
	cfg.Doctor = DoctorFunc(func(context.Context) ([]Check, error) { return checks, nil })
	source := &testLinearSource{}
	if linked {
		source.binding = bus.TaskboardBinding{Configured: true, Revision: 1}
	}
	cfg.Linear = source
	m, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	m = m.SetSize(80, 24)
	m, cmd := m.Show(ScreenOverview)
	return quickSettle(m, cmd)
}

var passing = []Check{{ID: "authority.health", Title: "Server reachable", Status: Pass}}

// U19, U23: Overview says what needs the person, each line naming its tab;
// Enter goes there.
func TestOverviewNeedsAttention(t *testing.T) {
	owner := map[string]bus.Result{
		"owner_transfer_get": {Source: "server", Data: map[string]any{"pending": map[string]any{"from": "ada@example.test", "to": "human@example.test"}}},
		"invite_list":        {Source: "server", Data: map[string]any{"invites": []any{map[string]any{"status": "pending"}, map[string]any{"status": "used"}}}},
	}
	checks := append([]Check{{ID: "linear.binding", Title: "Linear board", Status: Fail}}, passing...)
	m := overviewFor(t, ModeCloud, true, checks, owner, nil, false)
	view := Render(m)
	for _, want := range []string{
		"Doctor: 1 check fails", "8 Doctor",
		"This project's Linear board isn't linked (optional)", "5 Linear",
		"Ownership offered to you by ada@example.test", "1 invite is waiting to be used", "7 Account",
		"The dispatcher's model isn't set up (optional)", "6 Dispatcher",
	} {
		if !strings.Contains(view, want) {
			t.Fatalf("no %q in\n%s", want, view)
		}
	}
	golden(t, "overview-owner-80x24", view)
	if got := do(m, "enter"); got.screen != ScreenDoctor {
		t.Fatal("enter on the doctor line:", got.screen)
	}
	if got := do(m, "down", "enter"); got.screen != ScreenLinear {
		t.Fatal("enter on the Linear line:", got.screen)
	}
	// Once linked, the Linear line goes (U23); with nothing due, it says so.
	quiet := overviewFor(t, ModeCloud, true, passing, nil, map[string]error{"dispatcher_model_doctor": errors.New("project owner permission required")}, true)
	view = Render(quiet)
	if !strings.Contains(view, "Nothing needs you.") || strings.Contains(view, "Linear") && strings.Contains(view, "isn't linked") {
		t.Fatalf("a member with nothing due:\n%s", view)
	}
	golden(t, "overview-member-80x24", view)
	// With nothing due, the one row is the project's intake line.
	if after := do(quiet, "enter"); after.screen != ScreenLinear || after.linear.step != "intake" {
		t.Fatal("enter on the intake line:", after.screen, after.linear.step)
	}
}

// A line for a tab this profile lacks is dropped.
func TestAttentionOnlyForShownTabs(t *testing.T) {
	m := overviewFor(t, ModeLocal, false, passing, nil, nil, true)
	m = m.applyAttention(attentionMsg{request: m.request, items: []attentionItem{{Text: "an offer", Screen: ScreenAccount}, {Text: "a check", Screen: ScreenDoctor}}})
	if len(m.attention) != 1 || m.attention[0].Screen != ScreenDoctor {
		t.Fatal(m.attention)
	}
}

// U20, U21: identity in words; without a session, no unread count.
func TestOverviewSaysWhoAndWhere(t *testing.T) {
	m := overviewFor(t, ModeLocal, false, passing, nil, nil, true)
	view := Render(m)
	for _, want := range []string{"Signed in       human@example.test (Google", "Inbox           no session on this profile (agents' inboxes are per session)"} {
		if !strings.Contains(view, want) {
			t.Fatalf("no %q in\n%s", want, view)
		}
	}
	for _, unwanted := range []string{"Unread", "google-id-token", "pinned", "Account"} {
		if strings.Contains(view, unwanted) {
			t.Fatalf("%q in\n%s", unwanted, view)
		}
	}
	golden(t, "overview-local-80x24", view)
}

// U38, U39: Doctor lists failures first, folds the passes, jumps to a
// failing check's screen on Enter, and keeps the last results while it
// runs again.
func TestDoctorFailuresFirstAndJumps(t *testing.T) {
	m := screenFixtures(t)[ScreenDoctor]
	m.checks = append(m.checks, Check{ID: "linear.markers", Title: "Linear markers", Status: Pass})
	if c, ok := m.selectedCheck(); !ok || c.ID != "linear.binding" {
		t.Fatal("not first:", c)
	}
	// The rows are the ones shown: with the passes folded there's one,
	// so down stays on it.
	if got := do(m, "down", "enter"); got.screen != ScreenLinear {
		t.Fatal("enter on a Linear warning:", got.screen)
	}
	m = do(m, "p")
	if rows := doctorRows(m); len(rows) != 3 || rows[1].Status != Pass {
		t.Fatal(rows)
	}
	m = do(m, "down", "down")
	if c, _ := m.selectedCheck(); c.ID != "linear.markers" {
		t.Fatal(c)
	}
	if after := do(m, "enter"); after.screen != ScreenDoctor {
		t.Fatal("enter on a Linear pass moved")
	}
	m = do(m, "up")
	m = do(m, "down")
	if after := do(m, "enter"); after.screen != ScreenDoctor {
		t.Fatal("enter on a pass moved")
	}
	// A check whose screen this profile lacks doesn't jump.
	local := m
	local.cfg.Mode = ModeLocal
	local.checks = []Check{{ID: "auth.device", Title: "Device key", Status: Fail}}
	local.selected = 0
	if after := do(local, "enter"); after.screen != ScreenDoctor {
		t.Fatal("jumped to a screen local mode lacks")
	}
	// Running again keeps the last results in view.
	m.loading = true
	if view := Render(m); !strings.Contains(view, "Running the checks again…") || !strings.Contains(view, "Linear board") {
		t.Fatalf("the last results went while running:\n%s", view)
	}
}

// U60: a check reads as the CLI doctor's line: the answer on it with the
// message muted under it, or the message alone on the line, escaped.
func TestDoctorShowsTheAnswer(t *testing.T) {
	m := screenFixtures(t)[ScreenDoctor]
	m.checks = []Check{
		{ID: "authority.health", Title: "Workspace", Status: Fail, Answer: "Your workspace can't be reached.", Detail: "dial \x1b[2Jtcp: refused"},
		{ID: "auth.clock", Title: "Clock", Status: Warn, Detail: "the clock is 3m off"},
	}
	view := Render(m)
	for _, want := range []string{"FAIL  Workspace: Your workspace can't be reached.", `dial \x1b[2Jtcp: refused`, "WARN  Clock: the clock is 3m off"} {
		if !strings.Contains(view, want) {
			t.Fatalf("no %q in\n%s", want, view)
		}
	}
	if strings.Count(view, "the clock is 3m off") != 1 || strings.Count(view, "refused") != 1 {
		t.Fatalf("a message shown twice:\n%s", view)
	}
}

// #213 L1: a check without an answer puts its message on the line; a long
// one at 80 columns wraps inside the frame, never past it.
func TestDoctorLongMessageStaysInTheFrame(t *testing.T) {
	m := screenFixtures(t)[ScreenDoctor].SetSize(80, 24)
	long := "the clock is 3m off from the server's, so signed requests may be refused until the clock is set right again"
	m.checks = []Check{{ID: "auth.clock", Title: "Clock", Status: Warn, Detail: long}}
	view := Render(m)
	if !strings.Contains(view, "WARN  Clock: the clock is 3m off") || !strings.Contains(view, "set right again") {
		t.Fatalf("the message isn't all there:\n%s", view)
	}
	for _, line := range strings.Split(view, "\n") {
		if w := lipgloss.Width(line); w > 80 {
			t.Fatalf("a line %d cells wide:\n%s", w, view)
		}
	}
}

// The project owner's Overview names the server's PROVEN gates that are
// on and those that are off; a member, whose overview has none, sees no
// row.
// G8: a local workspace has no Linear intake, webhooks, PROVEN gates or
// dispatcher model, so its Overview lists none of them, whatever the
// authority answers; the same answers show in cloud mode.
func TestLocalOverviewHidesHostedOnlyItems(t *testing.T) {
	answers := map[string]bus.Result{
		"project_overview":        {Source: "local-authority", Data: map[string]any{"proven_gates": []any{map[string]any{"name": "project_flow", "on": false}}}},
		"intake_policy_get":       {Source: "local-authority", Data: map[string]any{"mode": "notify"}},
		"intake_trust_list":       {Source: "local-authority", Data: []any{}},
		"intake_list":             {Source: "local-authority", Data: []any{}},
		"webhook_status":          {Source: "local-authority", Data: map[string]any{"teams": []any{map[string]any{"team_id": "t", "state": "live", "configured": true}}}},
		"dispatcher_model_doctor": {Source: "local-authority", Data: map[string]any{"model_on": false}},
	}
	hostedOnly := []string{"Proven gates", "Not proven", "Intake:", "The dispatcher's model isn't set up"}
	view := Render(overviewFor(t, ModeLocal, false, passing, answers, nil, false))
	for _, item := range hostedOnly {
		if strings.Contains(view, item) {
			t.Fatalf("local mode lists %q\n%s", item, view)
		}
	}
	view = Render(overviewFor(t, ModeCloud, false, passing, answers, nil, false))
	for _, item := range []string{"Proven gates", "Intake:", "The dispatcher's model isn't set up"} {
		if !strings.Contains(view, item) {
			t.Fatalf("cloud mode lacks %q\n%s", item, view)
		}
	}
}

func TestOverviewShowsProvenGatesToTheOwner(t *testing.T) {
	gates := []any{
		map[string]any{"name": "project_flow", "on": false},
		map[string]any{"name": "issue_subscribers", "on": true},
		map[string]any{"name": "issue_move", "on": true},
		map[string]any{"name": "issue_family_move", "on": false},
		map[string]any{"name": "project_team_add", "on": false},
	}
	owner := map[string]bus.Result{"project_overview": {Source: "server", Data: map[string]any{"proven_gates": gates}}}
	view := Render(overviewFor(t, ModeCloud, false, passing, owner, nil, true))
	if !strings.Contains(view, "Proven gates    issue_subscribers, issue_move  ") || !strings.Contains(view, "Not proven      project_flow, issue_family_move, project_team_add  ") {
		t.Fatalf("no gates row in\n%s", view)
	}
	member := map[string]bus.Result{"project_overview": {Source: "server", Data: map[string]any{"project": map[string]any{}}}}
	if view = Render(overviewFor(t, ModeCloud, false, passing, member, nil, true)); strings.Contains(view, "Proven gates") {
		t.Fatalf("a member sees the gates row\n%s", view)
	}
}

// D4: one line under the intake line, in three forms; none when no team
// has a secret or the server is older.
func TestOverviewShowsTheWebhookLine(t *testing.T) {
	team := func(state string, configured bool, last string, missed int) map[string]any {
		return map[string]any{"team_id": "t", "state": state, "configured": configured, "last_delivery_at": last, "missed_24h": missed}
	}
	status := func(teams ...map[string]any) map[string]bus.Result {
		list := []any{}
		for _, t := range teams {
			list = append(list, t)
		}
		return map[string]bus.Result{"webhook_status": {Source: "server", Data: map[string]any{"teams": list}}}
	}
	now := time.Now().UTC()
	recent := now.Add(-4*time.Minute - 5*time.Second).Format(time.RFC3339)
	for name, c := range map[string]struct {
		results map[string]bus.Result
		errs    map[string]error
		want    string
	}{
		"live": {status(team("live", true, recent, 0), team("live", true, "", 0), team("waiting_first", true, "", 0)), nil,
			"Webhooks: 2/3 teams live · last 4m ago · 0 missed"},
		"inert": {status(team("inert", true, recent, 0), team("inert", true, "", 0)), nil, "Webhooks: inert (not proven)"},
		"none":  {status(team("unconfigured", false, "", 0)), nil, ""},
		"older": {nil, map[string]error{"webhook_status": errors.New("invalid: unsupported operation")}, ""},
	} {
		view := Render(overviewFor(t, ModeCloud, false, passing, c.results, c.errs, true))
		if c.want == "" && strings.Contains(view, "Webhooks:") || c.want != "" && !strings.Contains(view, c.want) {
			t.Errorf("%s: want %q in\n%s", name, c.want, view)
		}
	}
}

// FR23: the project's newest joins of the last day, under This project,
// one to a row; none when the overview lists none.
func TestOverviewShowsNewestJoins(t *testing.T) {
	now := time.Now().UTC()
	joins := []any{
		map[string]any{"name": "fresh", "display_name": "Grace Hopper", "agent_kind": "claude-code", "created_at": now.Add(-4*time.Minute - 5*time.Second).Format(time.RFC3339Nano)},
		map[string]any{"name": "older", "display_name": "a member", "agent_kind": "codex", "created_at": now.Add(-3*time.Hour - time.Minute).Format(time.RFC3339Nano)},
	}
	with := map[string]bus.Result{"project_overview": {Source: "server", Data: map[string]any{"newest_joins": joins}}}
	view := Render(overviewFor(t, ModeCloud, false, passing, with, nil, true))
	if !strings.Contains(view, "This project") || !strings.Contains(view, "  Newest joins: fresh (Grace Hopper, claude-code, 4m ago)") || !strings.Contains(view, "\n│                 older (a member, codex, 3h ago)") {
		t.Fatalf("no newest joins line in\n%s", view)
	}
	for name, results := range map[string]map[string]bus.Result{
		"none":  {"project_overview": {Source: "server", Data: map[string]any{"newest_joins": []any{}}}},
		"older": {"project_overview": {Source: "server", Data: map[string]any{"project": map[string]any{}}}},
	} {
		if view := Render(overviewFor(t, ModeCloud, false, passing, results, nil, true)); strings.Contains(view, "Newest joins") {
			t.Fatalf("%s: a newest joins line in\n%s", name, view)
		}
	}
}

// A note (info) is counted only when there is one, sorts after the
// warnings and before the passes, and Enter on it goes nowhere.
func TestDoctorShowsInfoAsANote(t *testing.T) {
	m := screenFixtures(t)[ScreenDoctor]
	if strings.Contains(renderDoctor(m), "info") {
		t.Fatal("an info count with no notes")
	}
	m.checks = append(m.checks, Check{ID: "linear.binding.note", Title: "A note", Status: Info})
	if view := renderDoctor(m); !strings.Contains(view, " · 1 info") || !strings.Contains(view, "INFO") {
		t.Fatalf("no info count or mark:\n%s", view)
	}
	rows := doctorRows(m)
	if last := rows[len(rows)-1]; last.Status != Info {
		t.Fatal("the note isn't after the warnings with the passes folded:", rows)
	}
	m.selected = len(rows) - 1
	if after := do(m, "enter"); after.screen != ScreenDoctor {
		t.Fatal("enter on a note moved to", after.screen)
	}
	m = do(m, "p")
	rows = doctorRows(m)
	for i, c := range rows {
		if c.Status == Info && (i == 0 || rows[i-1].Status == Pass) {
			t.Fatal("the note sorts with or after the passes:", rows)
		}
	}
}
