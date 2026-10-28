package tui

import (
	"context"
	"errors"
	"strings"
	"testing"

	"clawbridge/internal/bus"
)

const (
	statusProjectOne = "00000000-0000-4000-8000-0000000000c1"
	statusProjectTwo = "00000000-0000-4000-8000-0000000000c2"
)

// statusSource answers the board and the project status requests: a read
// per project (two fails), a preview, and the set's outcome.
type statusSource struct {
	testLinearSource
	open     int
	complete bool
	refused  string
	state    string
	failTwo  bool
	// teamBoard links the team board alone, with no Linear project.
	teamBoard bool
}

func (s *statusSource) Do(ctx context.Context, r LinearRequest) (LinearData, error) {
	if r.Operation != "project-status" && r.Operation != "project-status-set" {
		d, err := s.testLinearSource.Do(ctx, r)
		d.BindingV2 = &bus.TaskboardBindingV2{MappingVersion: 2, Configured: true, Revision: 4, LinearTeamIDs: []string{linearTestTeam},
			LinearDefaultTeamID: linearTestTeam, LinearProjectIDs: []string{statusProjectOne, statusProjectTwo}}
		if s.teamBoard {
			d.BindingV2.LinearProjectIDs = nil
		}
		return d, err
	}
	s.calls = append(s.calls, r)
	if r.LinearProject == statusProjectTwo && s.failTwo {
		return LinearData{}, errors.New("capability_unavailable: this project's authority is local")
	}
	d := &ProjectStatus{LinearProject: r.LinearProject, Status: ProjectStatusName{ID: "s-started", Name: "In \x1b[2JProgress", Type: "started"}}
	d.Issues.Total, d.Issues.Open, d.Issues.Complete = 4, s.open, s.complete
	switch {
	case r.Operation == "project-status" && r.Suggest:
		d.Suggestion = "completed"
	case r.Operation == "project-status-set" && !r.Confirm:
		d.Preview = true
		if s.refused != "" {
			d.Refused = s.refused
			break
		}
		d.ExpectedStatusID, d.NeedsAllowOpen = "s-started", r.StatusType == "completed" && (s.open > 0 || !s.complete)
		d.To = &ProjectStatusName{ID: "s-done", Name: "Done", Type: r.StatusType}
	case r.Operation == "project-status-set":
		d.State = s.state
		d.To = &ProjectStatusName{ID: "s-done", Name: "Done", Type: r.StatusType}
		if s.state == "refused" {
			d.Error = "rate_limited"
		}
	}
	return LinearData{ProjectStatus: d}, nil
}

func (s *statusSource) statusCalls() []LinearRequest {
	out := []LinearRequest{}
	for _, c := range s.calls {
		if strings.HasPrefix(c.Operation, "project-status") {
			out = append(out, c)
		}
	}
	return out
}

func statusView(t *testing.T, role, session string, source *statusSource) Model {
	t.Helper()
	a := &fakeAuthority{results: map[string]bus.Result{"project_overview": {Source: "server", Data: map[string]any{
		"project": map[string]any{"slug": "core", "role": role},
		"linear":  map[string]any{"configured": true, "projects": []any{map[string]any{"id": statusProjectOne, "name": "Payments"}}},
	}}}}
	cfg := testConfig(a)
	cfg.Linear, cfg.Session = source, session
	m, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	m = m.SetSize(100, 40)
	m, cmd := m.Show(ScreenLinear)
	m = quickSettle(m, cmd)
	if !strings.Contains(footerText(m), "p project status") {
		t.Fatal("no p on the board:", footerText(m))
	}
	return do(m, "p")
}

// U61: the view reads each bound project's status by the server's app,
// by name, escaped; one that can't be read says why on its row.
func TestProjectStatusReadsEachProject(t *testing.T) {
	source := &statusSource{open: 1, complete: true, failTwo: true}
	m := statusView(t, "member", "", source)
	view := Render(m)
	for _, want := range []string{"Project status: each Linear project of the board", "Payments", `In \x1b[2JProgress`, "1 of 4 issues open",
		statusProjectTwo, "not read", "    capability_unavailable: this project's authority is local", "Read-only: a project owner or lead sets a status"} {
		if !strings.Contains(view, want) {
			t.Fatalf("no %q in\n%s", want, view)
		}
	}
	if calls := source.statusCalls(); len(calls) != 2 || calls[0].LinearProject != statusProjectOne || calls[1].LinearProject != statusProjectTwo || calls[0].Suggest {
		t.Fatal(calls)
	}
	// A member gets no set, and Enter does nothing.
	if strings.Contains(footerText(m), "set its status") {
		t.Fatal(footerText(m))
	}
	if m = do(m, "enter"); m.projectStatus.picking {
		t.Fatal("a member picked")
	}
	// g suggests, as a hint on the row.
	m = do(m, "g")
	if calls := source.statusCalls(); !calls[len(calls)-1].Suggest || !strings.Contains(Render(m), "Suggested from its issues: completed (a hint; nothing is set)") {
		t.Fatal(Render(m))
	}
	for _, c := range source.statusCalls() {
		if c.Operation != "project-status" {
			t.Fatal("a member set:", c)
		}
	}
	if back := do(m, "esc"); back.linear.step != "tasks" {
		t.Fatal("esc:", back.linear.step)
	}
	// r reads them again.
	before := len(source.statusCalls())
	if m = do(m, "r"); len(source.statusCalls()) != before+2 {
		t.Fatal("r didn't read again")
	}
}

// The owner picks a type, sees the server's preview, and only y sends the
// confirm, with the operation ID and the status the preview saw.
func TestProjectStatusOwnerSetsThroughThePreview(t *testing.T) {
	source := &statusSource{complete: true, state: "accepted"}
	m := statusView(t, "owner", "", source)
	if !strings.Contains(footerText(m), "Enter set its status") || strings.Contains(Render(m), "Read-only") {
		t.Fatal(footerText(m))
	}
	m = do(m, "enter")
	if !m.projectStatus.picking || m.projectStatus.pick != 2 || !strings.Contains(Render(m), "Set it to which status type?") {
		t.Fatal("the picker doesn't start on the current type:", m.projectStatus.pick)
	}
	m = do(m, "down", "enter")
	calls := source.statusCalls()
	preview := calls[len(calls)-1]
	if preview.Operation != "project-status-set" || preview.Confirm || preview.StatusType != "completed" || !strings.HasPrefix(preview.OperationID, "tui-") || preview.LinearProject != statusProjectOne {
		t.Fatal(preview)
	}
	if !strings.Contains(Render(m), `Set Payments from In \x1b[2JProgress to Done? (0 of 4 issues open)`) || strings.Contains(Render(m), "still open") {
		t.Fatal(Render(m))
	}
	// n keeps it.
	kept := do(m, "n")
	if len(source.statusCalls()) != len(calls) || kept.projectStatus.ask != nil || kept.status.text != "Nothing was changed." {
		t.Fatal("n sent the set:", kept.status.text)
	}
	// The next request (here g) clears that note.
	if again := do(kept, "g"); again.status.text != "" {
		t.Fatal("the note stayed:", again.status.text)
	}
	m = do(m, "y")
	calls = source.statusCalls()
	confirm := calls[len(calls)-2]
	if confirm.Operation != "project-status-set" || !confirm.Confirm || confirm.ExpectedStatus != "s-started" || confirm.OperationID != preview.OperationID || confirm.AllowOpen {
		t.Fatal(confirm)
	}
	// Only the project set is read again, and the outcome stays.
	if last := calls[len(calls)-1]; !strings.Contains(m.status.text, "Payments is now Done.") || last.Operation != "project-status" || last.LinearProject != statusProjectOne || len(calls) != 6 {
		t.Fatal("not read again, or read all:", m.status.text, calls)
	}
	// Each set has its own operation ID.
	m = do(m, "enter", "enter")
	if again := source.statusCalls(); again[len(again)-1].OperationID == preview.OperationID {
		t.Fatal("an operation ID reused")
	}
}

// Closing with issues open takes a second y, then sends allow_open; a
// refused preview and a failed set say so.
func TestProjectStatusClosingWithIssuesOpen(t *testing.T) {
	source := &statusSource{open: 2, complete: true, state: "refused"}
	m := statusView(t, "lead", "", source)
	m = do(m, "enter", "down", "enter")
	if view := Render(m); !strings.Contains(view, "2 issues of Payments are still open; setting it completed anyway leaves them open.") {
		t.Fatal(view)
	}
	n := len(source.statusCalls())
	m = do(m, "y")
	if len(source.statusCalls()) != n || !strings.Contains(Render(m), "Set it completed with issues open?") {
		t.Fatal("one y closed it with issues open")
	}
	m = do(m, "y")
	calls := source.statusCalls()
	if c := calls[n]; !c.Confirm || !c.AllowOpen {
		t.Fatal(c)
	}
	if !strings.Contains(m.status.text, "Linear didn't take the change (refused): rate_limited.") {
		t.Fatal(m.status.text)
	}
	// An uncertain set says to check, not that it failed.
	source.state = "uncertain"
	m = do(m, "enter", "down", "enter", "y", "y")
	if !strings.Contains(m.status.text, "Not sure Linear took the change to Payments") {
		t.Fatal(m.status.text)
	}
	// Not all issues read: the second y says so.
	source.open, source.complete = 0, false
	m = do(m, "enter", "down", "enter")
	if !strings.Contains(Render(m), "Not all of Payments's issues could be read") {
		t.Fatal(Render(m))
	}
	m = do(m, "esc")
	// A refused preview.
	source.refused = "people close this project"
	m = do(m, "enter", "down", "enter")
	if m.projectStatus.ask != nil || !strings.Contains(m.status.text, "Can't set Payments completed: people close this project") {
		t.Fatal(m.status.text)
	}
}

// A TUI with a session reads only: the set is a person's, with none.
func TestProjectStatusWithASessionReadsOnly(t *testing.T) {
	source := &statusSource{complete: true}
	m := statusView(t, "owner", "sess-1", source)
	if !strings.Contains(Render(m), "Read-only") || strings.Contains(footerText(m), "set its status") {
		t.Fatal(Render(m))
	}
	m = do(m, "enter", "y")
	for _, c := range source.statusCalls() {
		if c.Operation != "project-status" {
			t.Fatal("a session set:", c)
		}
	}
}

// When the role can't be read, the view says so and reads only.
func TestProjectStatusRoleUnreadable(t *testing.T) {
	source := &statusSource{complete: true}
	m := statusView(t, "", "", source)
	if !strings.Contains(Render(m), "Read-only: couldn't read your role in this project.") || strings.Contains(footerText(m), "set its status") {
		t.Fatal(Render(m))
	}
}

// A board with no Linear project has no status to show: no p.
func TestProjectStatusNeedsALinearProject(t *testing.T) {
	cfg := testConfig(&fakeAuthority{})
	source := &statusSource{teamBoard: true}
	cfg.Linear = source
	m, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	m, cmd := m.SetSize(100, 40).Show(ScreenLinear)
	m = quickSettle(m, cmd)
	if !m.linearConfigured() || strings.Contains(footerText(m), "project status") {
		t.Fatal(footerText(m))
	}
	if m = do(m, "p"); m.linear.step == "status" || len(source.statusCalls()) != 0 {
		t.Fatal("p opened the view")
	}
}
