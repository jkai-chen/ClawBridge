package tui

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"clawbridge/internal/bus"
	"clawbridge/internal/linearboard"
)

// cancelSource answers the board and who may cancel as the server would:
// the project's kinds, the list at a revision, and a change at that
// revision (a stale one is a conflict).
type cancelSource struct {
	testLinearSource
	kinds    []string
	allowed  []string
	revision int64
	setErr   error
	// racer is a change another person makes between the read and the set.
	racer bool
	sets  []CancelKindsChange
}

func (s *cancelSource) view() *linearboard.CancelKindsView {
	allowed := append([]string{}, s.allowed...)
	return &linearboard.CancelKindsView{Revision: s.revision, CancelKinds: allowed, Kinds: append([]string{}, s.kinds...), Line: bus.CancelKindsLine(allowed)}
}

func (s *cancelSource) Do(ctx context.Context, r LinearRequest) (LinearData, error) {
	switch r.Operation {
	case "cancel-kinds":
		s.calls = append(s.calls, r)
		return LinearData{CancelKindsView: s.view()}, nil
	case "cancel-kinds-set":
		s.calls = append(s.calls, r)
		s.sets = append(s.sets, *r.CancelKinds)
		if s.setErr != nil {
			return LinearData{}, s.setErr
		}
		if s.racer {
			s.racer, s.revision = false, s.revision+1
			return LinearData{}, errors.New("status policy changed: current revision is " + string(rune('0'+s.revision)))
		}
		s.allowed, s.revision = append([]string{}, r.CancelKinds.Kinds...), s.revision+1
		return LinearData{CancelKindsView: s.view()}, nil
	}
	d, err := s.testLinearSource.Do(ctx, r)
	d.BindingV2 = &bus.TaskboardBindingV2{MappingVersion: 2, Configured: true, Revision: 4, LinearTeamIDs: []string{linearTestTeam}, LinearDefaultTeamID: linearTestTeam}
	return d, err
}

func (s *cancelSource) reads() int {
	n := 0
	for _, c := range s.calls {
		if c.Operation == "cancel-kinds" {
			n++
		}
	}
	return n
}

func cancelOpen(t *testing.T, source *cancelSource, edit func(*Config)) Model {
	t.Helper()
	cfg := testConfig(&fakeAuthority{})
	cfg.Linear = source
	// A person's TUI: who may cancel is never set with a session.
	cfg.Session = ""
	if edit != nil {
		edit(&cfg)
	}
	m, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	m, cmd := m.SetSize(120, 40).Show(ScreenLinear)
	m = quickSettle(m, cmd)
	if !strings.Contains(footerText(m), "C who may cancel") {
		t.Fatal("no C on the board:", footerText(m))
	}
	return do(m, "C")
}

func newCancelSource() *cancelSource {
	return &cancelSource{kinds: []string{"Engineering", "Research", "Ops\x1b[31m"}, allowed: []string{"Research"}, revision: 3}
}

// The view lists the project's kinds with a switch each, and the current
// line; Space switches the highlighted kind and nothing is sent.
func TestCancelKindsShowsKindsAndToggles(t *testing.T) {
	source := newCancelSource()
	m := cancelOpen(t, source, nil)
	view := Render(m)
	for _, want := range []string{"Who may cancel", "Revision 3", "Agents may cancel or mark Duplicate in projects of kind Research.", "[ ] Engineering", "[x] Research", `Ops\x1b[31m`} {
		if !strings.Contains(view, want) {
			t.Fatalf("view missing %q:\n%s", want, view)
		}
	}
	m = linearKeys(t, m, kSpace)
	if view = Render(m); !strings.Contains(view, "[x] Engineering") || !strings.Contains(view, "[x] Research") {
		t.Fatalf("Space did not switch the highlighted kind:\n%s", view)
	}
	m = linearKeys(t, m, kSpace)
	if view = Render(m); !strings.Contains(view, "[ ] Engineering") {
		t.Fatalf("Space did not switch it back:\n%s", view)
	}
	if len(source.sets) != 0 {
		t.Fatal("a toggle sent a change")
	}
}

// Adding a kind previews the result and asks for the kind names typed,
// exactly as shown; a wrong name changes nothing, the right ones send the
// list at the revision that was read.
func TestCancelKindsAddNeedsTheNamesTyped(t *testing.T) {
	source := newCancelSource()
	m := cancelOpen(t, source, nil)
	m = linearKeys(t, m, kSpace, kEnter)
	view := Render(m)
	for _, want := range []string{
		"Agents will be allowed to cancel or mark Duplicate issues in the Linear projects of kind Engineering, Research,",
		"without a person.",
		"Result: Agents may cancel or mark Duplicate in projects of kind Engineering, Research.",
		`Type "Engineering, Research" to let agents cancel in those kinds`,
	} {
		if !strings.Contains(view, want) {
			t.Fatalf("preview missing %q:\n%s", want, view)
		}
	}
	if strings.Contains(view, "enforcement ships") {
		t.Fatal("the preview still says the enforcement is pending:\n" + view)
	}
	if m.inputMode != inputLinearCancelKinds || len(source.sets) != 0 {
		t.Fatal("added a kind without asking for the names", m.inputMode, source.sets)
	}
	if input := renderInput(m); !strings.Contains(input, `"Engineering, Research"`) {
		t.Fatal(input)
	}
	// Only some of the names, and a wrong spelling, both refuse.
	for _, typed := range []string{"Engineering", "engineering, Research", "Research, Engineering"} {
		m = linearKeys(t, m, kType(typed), kEnter)
		if len(source.sets) != 0 || m.inputMode != inputNone || !strings.Contains(m.State().Error, "nothing was changed") {
			t.Fatalf("%q confirmed or was not refused: %v %q", typed, source.sets, m.State().Error)
		}
		m = linearKeys(t, m, kEnter)
	}
	m = linearKeys(t, m, kType("Engineering, Research"), kEnter)
	if len(source.sets) != 1 || !slices.Equal(source.sets[0].Kinds, []string{"Engineering", "Research"}) || source.sets[0].Revision != 3 {
		t.Fatal(source.sets)
	}
	view = Render(m)
	if !strings.Contains(view, "Revision 4") || !strings.Contains(view, "[x] Engineering") || !strings.Contains(m.State().Error, "Who may cancel saved") {
		t.Fatalf("not shown as saved: %q\n%s", m.State().Error, view)
	}
	// Esc while typing sends nothing.
	source = newCancelSource()
	m = cancelOpen(t, source, nil)
	m = linearKeys(t, m, kSpace, kEnter, kType("Eng"), kEsc)
	if len(source.sets) != 0 || m.inputMode != inputNone {
		t.Fatal("Esc confirmed", source.sets)
	}
}

// A result that only removes kinds needs no typing: y confirms, any other
// key keeps things as they are.
func TestCancelKindsRemovalNeedsNoTyping(t *testing.T) {
	source := newCancelSource()
	source.allowed = []string{"Engineering", "Research"}
	m := cancelOpen(t, source, nil)
	m = linearKeys(t, m, kSpace, kEnter)
	view := Render(m)
	if m.inputMode != inputNone || !strings.Contains(view, "Result: Agents may cancel or mark Duplicate in projects of kind Research.") || strings.Contains(view, "without a person") {
		t.Fatalf("a removal asked for typing or lacked its preview:\n%s", view)
	}
	if !strings.Contains(footerText(m), "y store it") {
		t.Fatal(footerText(m))
	}
	m = do(m, "n")
	if len(source.sets) != 0 || !strings.Contains(m.State().Error, "Nothing was changed") {
		t.Fatal("n confirmed", source.sets)
	}
	m = do(m, "enter", "y")
	if len(source.sets) != 1 || !slices.Equal(source.sets[0].Kinds, []string{"Research"}) || source.sets[0].Revision != 3 {
		t.Fatal(source.sets)
	}
	// Turning everything off sends an empty list.
	m = cancelOpen(t, newCancelSource(), nil)
	empty := m.cfg.Linear.(*cancelSource)
	m = linearKeys(t, m, kDown, kSpace, kEnter)
	m = do(m, "y")
	if len(empty.sets) != 1 || len(empty.sets[0].Kinds) != 0 || !strings.Contains(Render(m), "Agents never cancel issues.") {
		t.Fatal(empty.sets)
	}
}

// Nothing switched is nothing to send.
func TestCancelKindsUnchangedSendsNothing(t *testing.T) {
	source := newCancelSource()
	m := cancelOpen(t, source, nil)
	m = linearKeys(t, m, kEnter)
	if len(source.sets) != 0 || m.inputMode != inputNone || !strings.Contains(m.State().Error, "Nothing to change") {
		t.Fatal(source.sets, m.State().Error)
	}
}

// With no kind setting the view says so and offers no switch.
func TestCancelKindsWithoutKinds(t *testing.T) {
	source := &cancelSource{revision: 2}
	m := cancelOpen(t, source, nil)
	view := Render(m)
	if !strings.Contains(view, "This project has no kinds yet: the project owner sets them with clawbridge linear kind set") || strings.Contains(view, "[ ]") {
		t.Fatalf("no-kinds view:\n%s", view)
	}
	m = linearKeys(t, m, kSpace, kEnter)
	if len(source.sets) != 0 || m.inputMode != inputNone || strings.Contains(footerText(m), "agents may cancel") {
		t.Fatal("offered a switch with no kinds", footerText(m))
	}
}

// Under a session or an agent the control changes nothing and says it is
// a person's decision.
func TestCancelKindsRefusedForSessionsAndAgents(t *testing.T) {
	for name, edit := range map[string]func(*Config){
		"session": func(c *Config) { c.Session = "engineer" },
		"agent":   func(c *Config) { c.AgentMarker = "CLAUDECODE" },
	} {
		source := newCancelSource()
		m := cancelOpen(t, source, edit)
		m = linearKeys(t, m, kSpace)
		if !strings.Contains(m.State().Error, "Nothing was changed") || !strings.Contains(Render(m), "[ ] Engineering") {
			t.Fatalf("%s: a switch worked: %q", name, m.State().Error)
		}
		m = linearKeys(t, m, kEnter)
		want := "never set with a session"
		if name == "agent" {
			want = "a person's decision, and CLAUDECODE says an agent runs this TUI; run the TUI yourself"
		}
		if !strings.Contains(m.State().Error, want) || len(source.sets) != 0 || m.inputMode != inputNone {
			t.Fatalf("%s: %q %v", name, m.State().Error, source.sets)
		}
	}
}

// A change someone else made first is read again and said so; nothing the
// person agreed to is applied to the new revision.
func TestCancelKindsConflictReadsAgain(t *testing.T) {
	source := newCancelSource()
	source.racer = true
	m := cancelOpen(t, source, nil)
	reads := source.reads()
	m = linearKeys(t, m, kSpace, kEnter, kType("Engineering, Research"), kEnter)
	if len(source.sets) != 1 || source.reads() != reads+1 {
		t.Fatal("no re-read after the conflict", source.sets, source.reads())
	}
	if status := m.State().Error; !strings.Contains(status, "status policy changed") || !strings.Contains(status, "read again") || !strings.Contains(status, "nothing was changed") {
		t.Fatalf("conflict not said: %q", status)
	}
	if view := Render(m); !strings.Contains(view, "Revision 4") || !strings.Contains(view, "[ ] Engineering") {
		t.Fatalf("view is not the re-read one:\n%s", view)
	}
}

// Any other server error is shown as it is and nothing is assumed.
func TestCancelKindsShowsServerErrors(t *testing.T) {
	source := newCancelSource()
	source.setErr = errors.New("project owner or lead permission required")
	m := cancelOpen(t, source, nil)
	reads := source.reads()
	m = linearKeys(t, m, kSpace, kEnter, kType("Engineering, Research"), kEnter)
	if got := m.State().Error; !strings.Contains(got, "project owner or lead permission required") || source.reads() != reads {
		t.Fatalf("%q %d/%d", got, reads, source.reads())
	}
	if !strings.Contains(Render(m), "[ ] Engineering") && !strings.Contains(Render(m), "[x] Engineering") {
		t.Fatal("kinds vanished")
	}
}

// The project overview says who may cancel, from the status policy.
func TestProjectOverviewSaysWhoMayCancel(t *testing.T) {
	for kinds, want := range map[string]string{
		"":                      "Agents never cancel issues.",
		"Research":              "Agents may cancel or mark Duplicate in projects of kind Research.",
		"Research,Engineering ": "kind Research, Engineering.",
	} {
		a := overviewAuthority()
		list := []any{}
		for _, k := range strings.Split(kinds, ",") {
			if k = strings.TrimSpace(k); k != "" {
				list = append(list, k)
			}
		}
		linear := a.overviews["core"]["linear"].(map[string]any)
		linear["status_policy"] = map[string]any{"enabled": true, "revision": float64(1), "agent_may_cancel_kinds": list}
		m := projectsView(t, a)
		m = do(m, "enter")
		if view := Render(m); !strings.Contains(view, "Cancelling") || !strings.Contains(view, want) {
			t.Fatalf("overview lacks %q:\n%s", want, view)
		}
	}
	// A server that doesn't report the list says nothing about it.
	m := do(projectsView(t, overviewAuthority()), "enter")
	if view := Render(m); strings.Contains(view, "Cancelling") {
		t.Fatalf("overview invented a line:\n%s", view)
	}
}
