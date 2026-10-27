package tui

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"clawbridge/internal/bus"
	"clawbridge/internal/linearprovider"
)

const (
	adaID   = "00000000-0000-4000-8000-0000000000a1"
	graceID = "00000000-0000-4000-8000-0000000000a2"
	goneID  = "00000000-0000-4000-8000-0000000000a3"
	oldID   = "00000000-0000-4000-8000-0000000000a9"
	fixID   = "00000000-0000-4000-8000-0000000000b1"
)

// intakeAuthority keeps a project's intake: its policy, trusted list and
// queue, the viewer's role, and every request made.
type intakeAuthority struct {
	fakeAuthority
	mu       sync.Mutex
	role     string
	mode     string
	signals  []any
	revision int
	trusted  []string
	queue    []map[string]any
	requests []bus.Request
}

func newIntakeAuthority(role string) *intakeAuthority {
	return &intakeAuthority{role: role, mode: "notify", signals: []any{"S1", "S2", "S3"}, revision: 3,
		trusted: []string{adaID, oldID},
		queue: []map[string]any{
			{"issue_uuid": fixID, "issue_identifier": "FIX-7", "state": "queued", "signals": "S1", "reason": ""},
			{"issue_uuid": "00000000-0000-4000-8000-0000000000b2", "issue_identifier": "FIX-8", "state": "closed"},
		}}
}

func (a *intakeAuthority) Execute(ctx context.Context, r bus.Request) (bus.Result, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if !strings.HasPrefix(r.Operation, "intake_") && r.Operation != "list_members" {
		return a.fakeAuthority.Execute(ctx, r)
	}
	a.requests = append(a.requests, r)
	switch r.Operation {
	case "list_members":
		return bus.Result{Source: "server", Data: map[string]any{"members": []any{map[string]any{"principal": "human@example.test", "role": a.role}}}}, nil
	case "intake_policy_get":
		return bus.Result{Source: "server", Data: map[string]any{"mode": a.mode, "signals": a.signals, "revision": float64(a.revision)}}, nil
	case "intake_trust_list":
		rows := []any{}
		for _, id := range a.trusted {
			rows = append(rows, map[string]any{"linear_user_id": id})
		}
		return bus.Result{Source: "server", Data: rows}, nil
	case "intake_list":
		rows := []any{}
		for _, q := range a.queue {
			if s := r.Args["state"]; s == nil || s == q["state"] {
				rows = append(rows, q)
			}
		}
		return bus.Result{Source: "server", Data: rows}, nil
	case "intake_policy_set":
		if v, ok := r.Args["mode"]; ok {
			a.mode = v.(string)
		}
		if v, ok := r.Args["signals"]; ok {
			a.signals = v.([]any)
		}
		a.revision++
	case "intake_trust_add":
		a.trusted = append(a.trusted, r.Args["linear_user_id"].(string))
	case "intake_trust_remove":
		kept := []string{}
		for _, id := range a.trusted {
			if id != r.Args["linear_user_id"] {
				kept = append(kept, id)
			}
		}
		a.trusted = kept
	case "intake_dismiss":
		for _, q := range a.queue {
			if q["issue_uuid"] == r.Args["issue_uuid"] {
				q["state"] = "dismissed"
			}
		}
	}
	return bus.Result{Source: "server", Data: map[string]any{}}, nil
}

// writes are the intake changes made, in order.
func (a *intakeAuthority) writes() []bus.Request {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := []bus.Request{}
	for _, r := range a.requests {
		switch r.Operation {
		case "intake_policy_get", "intake_trust_list", "intake_list", "list_members":
		default:
			out = append(out, r)
		}
	}
	return out
}

// intakeLinear answers the intake read: the board's members and its label
// check; err fails it.
type intakeLinear struct {
	testLinearSource
	err error
}

func (s *intakeLinear) Do(ctx context.Context, r LinearRequest) (LinearData, error) {
	if r.Operation != "intake" {
		return s.testLinearSource.Do(ctx, r)
	}
	s.calls = append(s.calls, r)
	if s.err != nil {
		return LinearData{}, s.err
	}
	return LinearData{
		Members: []linearprovider.Member{
			{ID: adaID, Name: "Ada Lovelace", Active: true},
			{ID: graceID, Name: "Grace \x1b[2JHopper", Active: true},
			{ID: goneID, Name: "Gone Person"},
		},
		IntakeLabels: []IntakeLabel{
			{TeamID: linearTestTeam, Found: 1, Complete: true},
			{TeamID: "team-two", Found: 0, Complete: true},
			{TeamID: "team-three", Found: 2, Complete: true},
		},
	}, nil
}

func intakeView(t *testing.T, a *intakeAuthority, source *intakeLinear) Model {
	t.Helper()
	source.binding = bus.TaskboardBinding{Configured: true, Revision: 4, LinearTeamIDs: []string{linearTestTeam}, LinearDefaultTeamID: linearTestTeam}
	cfg := testConfig(a)
	cfg.Linear = source
	m, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	m = m.SetSize(100, 60)
	m, cmd := m.Show(ScreenLinear)
	m = quickSettle(m, cmd)
	if !strings.Contains(footerText(m), "n Intake") {
		t.Fatal("no n Intake on the board:", footerText(m))
	}
	return do(m, "n")
}

// U59: the owner sees the mode, the signals, the trusted users by name,
// the queue and the label check, and changes each through a y/N preview,
// as themselves (no session).
func TestIntakeViewOwnerChangesThroughPreviews(t *testing.T) {
	a, source := newIntakeAuthority("owner"), &intakeLinear{}
	m := intakeView(t, a, source)
	view := Render(m)
	for _, want := range []string{
		"Intake: which Linear signals ask for an agent", "Mode: notify", "[x] S1 the needs-agent label", "[x] S3",
		"Ada Lovelace", oldID, "FIX-7", "queued", "in Linear itself counts", "never trusted",
		"needs-agent label found in team " + linearTestTeam, "needs-agent label missing in team team-two", "2 labels named needs-agent in team team-three",
	} {
		if !strings.Contains(view, want) {
			t.Fatalf("no %q in\n%s", want, view)
		}
	}
	if strings.Contains(view, "FIX-8") || strings.Contains(view, "Read-only") {
		t.Fatalf("a closed issue, or read-only for the owner:\n%s", view)
	}
	// The mode: a preview, and only y changes it.
	m = do(m, " ")
	if !strings.Contains(Render(m), "Intake mode notify → off: nothing is queued?") || len(a.writes()) != 0 {
		t.Fatal("no preview:", Render(m))
	}
	m = do(m, "n")
	if m.intake.ask != nil || len(a.writes()) != 0 {
		t.Fatal("n changed the mode")
	}
	m = do(m, " ", "y")
	w := a.writes()
	if len(w) != 1 || w[0].Operation != "intake_policy_set" || w[0].Args["mode"] != "off" || w[0].Args["expected_revision"] != 3 || w[0].Session != "" {
		t.Fatal(w)
	}
	if !strings.Contains(Render(m), "Mode: off") {
		t.Fatal("not read again:", Render(m))
	}
	// A signal: S1 off.
	m = do(m, "down", " ", "y")
	if w = a.writes(); len(w) != 2 || len(w[1].Args["signals"].([]any)) != 2 || hasSignal([]string{w[1].Args["signals"].([]any)[0].(string)}, "S1") {
		t.Fatal(w[1].Args)
	}
	// Trusting: the picker offers the active members not yet trusted, by
	// name, escaped.
	m = do(m, "a")
	view = Render(m)
	if !strings.Contains(view, "Trust whom?") || !strings.Contains(view, `Grace \x1b[2JHopper`) || strings.Contains(view, "Gone Person") || strings.Count(view, "Ada Lovelace") != 1 {
		t.Fatalf("the picker:\n%s", view)
	}
	m = do(m, "enter")
	if !strings.Contains(Render(m), "Trust Grace") {
		t.Fatal(Render(m))
	}
	m = do(m, "y")
	if w = a.writes(); w[2].Operation != "intake_trust_add" || w[2].Args["linear_user_id"] != graceID {
		t.Fatal(w[2])
	}
	// Removing: x on Ada, after a preview.
	m.selected = 4
	if kind, id := m.intakeRow(); kind != "trusted" || id != adaID {
		t.Fatal(kind, id)
	}
	m = do(m, "x")
	if !strings.Contains(Render(m), "Stop trusting Ada Lovelace?") {
		t.Fatal(Render(m))
	}
	m = do(m, "y")
	if w = a.writes(); w[3].Operation != "intake_trust_remove" || w[3].Args["linear_user_id"] != adaID {
		t.Fatal(w[3])
	}
	// The queue: dismiss, then rearm.
	m.selected = len(m.intakeRows()) - 1
	m = do(m, "x", "y")
	if w = a.writes(); w[4].Operation != "intake_dismiss" || w[4].Args["issue_uuid"] != fixID {
		t.Fatal(w[4])
	}
	m.selected = len(m.intakeRows()) - 1
	m = do(m, "e")
	if !strings.Contains(Render(m), "Rearm FIX-7") {
		t.Fatal(Render(m))
	}
	m = do(m, "y")
	if w = a.writes(); w[5].Operation != "intake_rearm" {
		t.Fatal(w[5])
	}
	for _, r := range a.requests {
		if r.Session != "" {
			t.Fatal("intake went through a session:", r.Operation)
		}
	}
	if after := do(m, "esc"); after.linear.step != "tasks" {
		t.Fatal("esc:", after.linear.step)
	}
}

// Anyone else reads, with no controls.
func TestIntakeViewReadOnlyForAMember(t *testing.T) {
	a := newIntakeAuthority("member")
	m := intakeView(t, a, &intakeLinear{})
	if !strings.Contains(Render(m), "Read-only: a project owner or lead changes intake.") {
		t.Fatal(Render(m))
	}
	for _, unwanted := range []string{"switch", "trust someone", "stop trusting", "dismiss", "rearm"} {
		if strings.Contains(footerText(m), unwanted) {
			t.Fatalf("%q in the footer: %s", unwanted, footerText(m))
		}
	}
	m = do(m, " ", "y", "a", "x", "e")
	if len(a.writes()) != 0 || m.intake.ask != nil || m.intake.picking {
		t.Fatal("a member changed intake:", a.writes())
	}
}

// Without the viewer's Linear connection the view still works, by ID.
func TestIntakeViewWithoutLinearNames(t *testing.T) {
	m := intakeView(t, newIntakeAuthority("owner"), &intakeLinear{err: errors.New("no connection")})
	view := Render(m)
	if !strings.Contains(view, adaID) || !strings.Contains(view, "Names and the label check need your Linear connection") || strings.Contains(view, "Label check") {
		t.Fatal(view)
	}
	if m = do(m, "a"); m.intake.picking || !strings.Contains(m.status.text, "intake trust add LINEAR_USER_ID") {
		t.Fatal("a picker without names:", m.status.text)
	}
}

// Overview's intake line says what's set and waits, and opens the view.
func TestOverviewIntakeLineOpensIntake(t *testing.T) {
	a := newIntakeAuthority("owner")
	a.mode = "off"
	source := &intakeLinear{}
	source.binding = bus.TaskboardBinding{Configured: true, Revision: 4, LinearTeamIDs: []string{linearTestTeam}}
	cfg := testConfig(a)
	cfg.Linear = source
	cfg.Doctor = DoctorFunc(func(context.Context) ([]Check, error) { return passing, nil })
	m, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	m = m.SetSize(80, 30)
	m, cmd := m.Show(ScreenOverview)
	m = quickSettle(m, cmd)
	if !strings.Contains(Render(m), "Intake: off · 2 trusted · 1 queued") {
		t.Fatal(Render(m))
	}
	m.selected = len(m.attention)
	m = do(m, "enter")
	if m.screen != ScreenLinear || m.linear.step != "intake" || !strings.Contains(Render(m), "Mode: off") {
		t.Fatal("enter on the intake line:", m.screen, m.linear.step)
	}
}
