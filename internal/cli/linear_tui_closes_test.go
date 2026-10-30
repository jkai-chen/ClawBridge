package cli

import (
	"encoding/json"
	"strings"
	"testing"

	"clawbridge/internal/bus"
	"clawbridge/internal/linearboard"
	"clawbridge/internal/linearprovider"
)

const tuiProject = "33333333-3333-4333-8333-333333333333"

// A3 in the TUI: a confirmed link that changes who closes is refused
// before anything is sent from a session-bound TUI, and letting agents
// close is refused under an agent's environment.
func TestTUIClosesNeedsAPersonWithoutASession(t *testing.T) {
	old := closesEnv
	t.Cleanup(func() { closesEnv = old })
	closesEnv = func() []string { return nil }
	agent := linearboard.LinkInputV2{Confirm: true, Closes: map[string]string{tuiProject: linearboard.ClosesAgent}}
	human := linearboard.LinkInputV2{Confirm: true, Closes: map[string]string{tuiProject: linearboard.ClosesHuman}}
	if err := tuiClosesAllowed(bus.Request{}, agent); err != nil {
		t.Fatal(err)
	}
	for _, in := range []linearboard.LinkInputV2{agent, human} {
		if err := tuiClosesAllowed(bus.Request{Session: "eng"}, in); err == nil || !strings.Contains(err.Error(), "never set with a session") {
			t.Fatal(err)
		}
	}
	// A preview, or a link changing nothing, is never refused.
	preview := agent
	preview.Confirm = false
	if err := tuiClosesAllowed(bus.Request{Session: "eng"}, preview); err != nil {
		t.Fatal(err)
	}
	for _, marker := range []string{"CLAUDECODE=1", "CODEX_HOME=/x", "CLAWBRIDGE_SESSION=eng"} {
		closesEnv = func() []string { return []string{"PATH=/bin", marker} }
		if err := tuiClosesAllowed(bus.Request{}, agent); err == nil || !strings.Contains(err.Error(), "agent runs this TUI") {
			t.Fatal(marker, err)
		}
		// Tightening back to people is never an agent's escalation.
		if err := tuiClosesAllowed(bus.Request{}, human); err != nil {
			t.Fatal(marker, err)
		}
	}
}

// A link that set who closes answers the binding plus its outcomes; the
// TUI reads both, where the binding alone has a fixed set of fields.
func TestTUILinkResultCarriesClosesOutcomes(t *testing.T) {
	raw, _ := json.Marshal(bus.TaskboardBindingV2{WorkspaceID: "ws_1", ProjectID: "prj_1", UpdatedAt: "2026-10-27T00:00:00Z", Configured: true, Revision: 2, MappingVersion: 2, Provider: "linear", Mode: "project_set", LinearTeamIDs: []string{"11111111-1111-4111-8111-111111111111"}, LinearDefaultTeamID: "11111111-1111-4111-8111-111111111111", LinearProjectIDs: []string{tuiProject}})
	result := map[string]any{}
	if err := json.Unmarshal(raw, &result); err != nil {
		t.Fatal(err)
	}
	if _, err := bus.ParseTaskboardBindingV2(result); err != nil {
		t.Fatal("fixture:", err)
	}
	result["closes"] = []linearboard.ClosesOutcome{{LinearProject: tuiProject, Closes: linearboard.ClosesAgent, Outcome: "set"}}
	b, outcomes, err := linkResult(result)
	if err != nil || b.Revision != 2 || len(outcomes) != 1 || outcomes[0].Outcome != "set" {
		t.Fatal(b, outcomes, err)
	}
	if _, err = bus.ParseTaskboardBindingV2(result); err == nil {
		t.Fatal("the bare parser took the outcomes as a binding field; the test proves nothing")
	}
}

// The Intake view's label check counts what S1 takes: the exact name,
// live, not a group.
func TestNeedsAgentLabelsByTheWatchersRule(t *testing.T) {
	labels := []linearprovider.Label{{Name: "needs-agent"}, {Name: "Needs-Agent"}, {Name: "needs-agent", Archived: true}, {Name: "needs-agent", Group: true}, {Name: "needs-agent "}}
	if n := needsAgentLabels(labels); n != 1 {
		t.Fatal(n)
	}
	if n := needsAgentLabels(append(labels, linearprovider.Label{Name: "needs-agent"})); n != 2 {
		t.Fatal("a duplicate:", n)
	}
}

// Who may cancel in the TUI: a session never sets it, an agent's TUI may
// only take kinds away, and a person's TUI sets what it likes.
func TestTUICancelKindsNeedsAPersonWithoutASession(t *testing.T) {
	old := closesEnv
	t.Cleanup(func() { closesEnv = old })
	closesEnv = func() []string { return nil }
	current := []string{"Research", "Engineering"}
	if err := tuiCancelKindsAllowed(bus.Request{}, current, []string{"Research", "ops"}); err != nil {
		t.Fatal(err)
	}
	for _, next := range [][]string{{"Research"}, nil} {
		if err := tuiCancelKindsAllowed(bus.Request{Session: "eng"}, current, next); err == nil || !strings.Contains(err.Error(), "never set with a session") {
			t.Fatal(next, err)
		}
	}
	for _, marker := range []string{"CLAUDECODE=1", "CODEX_HOME=/x", "CLAWBRIDGE_SESSION=eng"} {
		closesEnv = func() []string { return []string{"PATH=/bin", marker} }
		if err := tuiCancelKindsAllowed(bus.Request{}, current, []string{"Research", "Ops"}); err == nil || !strings.Contains(err.Error(), "agent runs this TUI") || !strings.Contains(err.Error(), "run the TUI yourself") {
			t.Fatal(marker, err)
		}
		// Taking kinds away, even renamed in case, is never an escalation.
		for _, next := range [][]string{{"research"}, {}, nil} {
			if err := tuiCancelKindsAllowed(bus.Request{}, current, next); err != nil {
				t.Fatal(marker, next, err)
			}
		}
	}
}
