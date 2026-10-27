package tui

import (
	"context"
	"strings"
	"testing"

	"clawbridge/internal/bus"
	"clawbridge/internal/clirender"
	"clawbridge/internal/linearboard"
	"clawbridge/internal/linearconn"
	"clawbridge/internal/linearprovider"
	tea "github.com/charmbracelet/bubbletea"
)

const (
	closesTeam = "11111111-1111-4111-8111-111111111111"
	closesP    = "33333333-3333-4333-8333-333333333333"
	closesQ    = "44444444-4444-4444-8444-444444444444"
)

// closesLinearSource answers a link preview as the server would: each
// project's rule, what the link sets, and the minimal policy when there
// is no status policy and an agent answer.
type closesLinearSource struct {
	calls    []LinearRequest
	agents   map[string]bool // stored: projects agents close already
	noPolicy bool
	outcomes []linearboard.ClosesOutcome
}

func (s *closesLinearSource) Do(_ context.Context, r LinearRequest) (LinearData, error) {
	s.calls = append(s.calls, r)
	d := LinearData{BindingV2: &bus.TaskboardBindingV2{MappingVersion: 2}}
	switch r.Operation {
	case "profiles":
		d.Profiles = []linearconn.Profile{{ID: "native", Name: "Native", Access: linearconn.Read}}
	case "teams":
		d.Teams = linearprovider.Page[linearprovider.Team]{Items: []linearprovider.Team{{ID: closesTeam, Name: "Eng"}}, Complete: true}
	case "projects":
		d.Projects = linearprovider.Page[linearprovider.Project]{Items: []linearprovider.Project{{ID: closesP, Name: "Payments", TeamIDs: []string{closesTeam}}, {ID: closesQ, Name: "Web\x1b[31m", TeamIDs: []string{closesTeam}}}, Complete: true}
	case "preview":
		d.Preview = "preview"
		names := map[string]string{closesP: "Payments", closesQ: "Web\x1b[31m"}
		for _, id := range []string{closesP, closesQ} {
			line := linearboard.ClosesLine{LinearProject: id, Name: names[id], Closes: "people (default)", Bound: true}
			if s.noPolicy {
				line.Closes = "not enforced (no status policy)"
			} else if s.agents[id] {
				line.Closes = "agents"
			}
			if want := r.LinkV2.Closes[id]; want != "" && (want == linearboard.ClosesAgent) != s.agents[id] {
				line.Set = want
				if want == linearboard.ClosesAgent && s.noPolicy {
					d.MinimalPolicy = linearboard.MinimalPolicyConsequences
				}
			}
			d.Closes = append(d.Closes, line)
		}
	case "link":
		d.BindingV2 = &bus.TaskboardBindingV2{Configured: true, Revision: 1, MappingVersion: 2, Mode: "project_set", LinearTeamIDs: []string{closesTeam}, LinearDefaultTeamID: closesTeam, LinearProjectIDs: []string{closesP, closesQ}}
		d.ClosesOutcomes = s.outcomes
	}
	return d, nil
}

func (s *closesLinearSource) last(op string) *LinearRequest {
	for i := len(s.calls) - 1; i >= 0; i-- {
		if s.calls[i].Operation == op {
			return &s.calls[i]
		}
	}
	return nil
}

func linearKeys(t *testing.T, m Model, keys ...tea.KeyMsg) Model {
	t.Helper()
	for _, k := range keys {
		var cmd tea.Cmd
		m, cmd = m.UpdateModel(k)
		for cmd != nil {
			m, cmd = m.UpdateModel(cmd())
		}
	}
	return m
}

var (
	kEnter = tea.KeyMsg{Type: tea.KeyEnter}
	kSpace = tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(" ")}
	kDown  = tea.KeyMsg{Type: tea.KeyDown}
	kC     = tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("c")}
	kEsc   = tea.KeyMsg{Type: tea.KeyEsc}
)

func kType(s string) tea.KeyMsg { return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(s)} }

// closesPreview walks to the link review with both projects selected.
func closesPreview(t *testing.T, source *closesLinearSource) Model {
	t.Helper()
	cfg := testConfig(&fakeAuthority{})
	cfg.Linear = source
	m, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	m = m.SetSize(160, 60)
	m, cmd := m.Show(ScreenLinear)
	m, _ = m.UpdateModel(cmd())
	// profile → team (space, enter) → default (enter) → projects: both → preview
	m = linearKeys(t, m, kEnter, kSpace, kEnter, kEnter, kDown, kSpace, kDown, kSpace, kEnter)
	if m.LinearState().Step != "preview" || len(m.linear.lines) != 2 {
		t.Fatalf("not at the review: %+v %v", m.LinearState(), m.linear.lines)
	}
	return m
}

// The review lists who closes each project; Enter keeps people, sending
// no closes at all.
func TestLinearReviewEnterKeepsPeople(t *testing.T) {
	source := &closesLinearSource{agents: map[string]bool{}}
	m := closesPreview(t, source)
	view := Render(m)
	for _, want := range []string{"Who closes each project's issues? Enter keeps people", "Payments · " + closesP + " · closes: people (default)", `Web\x1b[31m`} {
		if !strings.Contains(view, want) {
			t.Fatalf("review missing %q:\n%s", want, view)
		}
	}
	m = linearKeys(t, m, kEnter)
	link := source.last("link")
	if link == nil || link.LinkV2.Closes != nil || m.inputMode != inputNone {
		t.Fatal("Enter changed who closes:", link)
	}
}

// c switches the highlighted project to agents and previews again; Enter
// then asks for that project's name, and only the right name confirms,
// as tui.
func TestLinearReviewAgentsNeedsTheName(t *testing.T) {
	source := &closesLinearSource{agents: map[string]bool{}}
	m := closesPreview(t, source)
	m = linearKeys(t, m, kC)
	preview := source.last("preview")
	if preview.LinkV2.Confirm || preview.LinkV2.Closes[closesP] != linearboard.ClosesAgent || !strings.Contains(Render(m), "→ agents (this link sets it)") {
		t.Fatal("c did not preview agents for the highlighted project:", preview.LinkV2, Render(m))
	}
	m = linearKeys(t, m, kEnter)
	if source.last("link") != nil || m.inputMode != inputLinearClosesName || !strings.Contains(Render(m), `Type "Payments"`) {
		t.Fatal("agents were confirmed without the name", Render(m))
	}
	m = linearKeys(t, m, kType("Paymentz"), kEnter)
	if source.last("link") != nil || !strings.Contains(m.State().Error, `is not "Payments"`) {
		t.Fatal("a wrong name confirmed", m.State().Error)
	}
	m = linearKeys(t, m, kEnter, kType("Payments"), kEnter)
	link := source.last("link")
	if link == nil || !link.LinkV2.Confirm || link.LinkV2.Closes[closesP] != linearboard.ClosesAgent || link.LinkV2.Via != linearboard.ClosesViaTUI || link.LinkV2.MinimalPolicy {
		t.Fatal(link)
	}
	// Esc while typing changes nothing.
	source = &closesLinearSource{agents: map[string]bool{}}
	m = closesPreview(t, source)
	m = linearKeys(t, m, kC, kEnter, kType("Pay"), kEsc)
	if source.last("link") != nil {
		t.Fatal("Esc confirmed the link")
	}
}

// c on a project agents already close switches it back to people, which
// needs no name; the other project is untouched.
func TestLinearReviewBackToPeople(t *testing.T) {
	source := &closesLinearSource{agents: map[string]bool{closesQ: true}}
	m := closesPreview(t, source)
	m = linearKeys(t, m, kDown, kC)
	if source.last("preview").LinkV2.Closes[closesQ] != linearboard.ClosesHuman {
		t.Fatal(source.last("preview").LinkV2)
	}
	m = linearKeys(t, m, kEnter)
	link := source.last("link")
	if link == nil || len(link.LinkV2.Closes) != 1 || link.LinkV2.Closes[closesQ] != linearboard.ClosesHuman || m.inputMode != inputNone {
		t.Fatal(link)
	}
	// c twice is back where it started: nothing to set.
	source = &closesLinearSource{agents: map[string]bool{}}
	m = closesPreview(t, source)
	m = linearKeys(t, m, kC, kC, kEnter)
	if link = source.last("link"); link == nil || link.LinkV2.Closes[closesP] != linearboard.ClosesHuman || m.inputMode != inputNone {
		t.Fatal(link)
	}
}

// With no status policy, an agents answer shows the minimal policy's
// consequences, and typing the name agrees to create it.
func TestLinearReviewMinimalPolicy(t *testing.T) {
	source := &closesLinearSource{agents: map[string]bool{}, noPolicy: true, outcomes: []linearboard.ClosesOutcome{{LinearProject: closesP, Closes: linearboard.ClosesAgent, Outcome: "failed", Error: "only the project owner can create the status policy", Rerun: "clawbridge linear closes set …"}}}
	m := closesPreview(t, source)
	if !strings.Contains(Render(m), "not enforced (no status policy)") {
		t.Fatal(Render(m))
	}
	m = linearKeys(t, m, kC)
	if !strings.Contains(Render(m), "close protection turns on") || !strings.Contains(Render(m), "also creates that policy") {
		t.Fatal(Render(m))
	}
	m = linearKeys(t, m, kEnter, kType("Payments"), kEnter)
	link := source.last("link")
	if link == nil || !link.LinkV2.MinimalPolicy {
		t.Fatal(link)
	}
	// A failed closes set after the binding says so, with the re-run.
	if e := m.State().Error; !strings.Contains(e, "Linear board linked") || m.State().Status != clirender.Error || !strings.Contains(e, "who closes was not set (only the project owner") || !strings.Contains(e, "closes set") {
		t.Fatal(e)
	}
}

// R114-M1: Enter right after c, before the new preview lands, sends
// nothing; and an agents answer the shown preview doesn't reflect never
// goes without its name.
func TestLinearReviewEnterWaitsForThePreview(t *testing.T) {
	source := &closesLinearSource{agents: map[string]bool{}}
	m := closesPreview(t, source)
	m, pending := m.UpdateModel(kC)
	if pending == nil {
		t.Fatal("c sent no preview")
	}
	m, cmd := m.UpdateModel(kEnter)
	if cmd != nil || source.last("link") != nil || m.inputMode != inputNone || !strings.Contains(m.State().Error, "then press Enter") {
		t.Fatal("Enter went through while the preview loaded:", source.last("link"), m.State().Error)
	}
	m, again := m.UpdateModel(kC)
	if again != nil || m.linear.closes[closesP] != "agent" {
		t.Fatal("c went through while the preview loaded:", m.linear.closes)
	}
	// Once the preview lands, Enter asks for the name as usual.
	m, _ = m.UpdateModel(pending())
	m = linearKeys(t, m, kEnter)
	if source.last("link") != nil || m.inputMode != inputLinearClosesName {
		t.Fatal("agents confirmed without the name")
	}
	// An answer the preview doesn't show is refused, not sent.
	source = &closesLinearSource{agents: map[string]bool{}}
	m = closesPreview(t, source)
	m.linear.closes = map[string]string{closesP: "agent"}
	m = linearKeys(t, m, kEnter)
	if source.last("link") != nil || m.inputMode != inputNone || !strings.Contains(m.State().Error, "out of date") {
		t.Fatal("a stale preview confirmed agents:", source.last("link"), m.State().Error)
	}
}
