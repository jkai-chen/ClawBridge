package tui

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"clawbridge/internal/bus"
	"clawbridge/internal/linearconn"
	"clawbridge/internal/linearprovider"
	tea "github.com/charmbracelet/bubbletea"
)

type testLinearSourceV2 struct{ calls []LinearRequest }

func (s *testLinearSourceV2) Do(_ context.Context, r LinearRequest) (LinearData, error) {
	s.calls = append(s.calls, r)
	const a = "11111111-1111-4111-8111-111111111111"
	const b = "22222222-2222-4222-8222-222222222222"
	const p = "33333333-3333-4333-8333-333333333333"
	const q = "44444444-4444-4444-8444-444444444444"
	d := LinearData{BindingV2: &bus.TaskboardBindingV2{MappingVersion: 2}}
	switch r.Operation {
	case "profiles":
		d.Profiles = []linearconn.Profile{{ID: "native", Name: "Native", Access: linearconn.Read}}
	case "teams":
		d.Teams = linearprovider.Page[linearprovider.Team]{Items: []linearprovider.Team{{ID: a, Name: "same"}, {ID: b, Name: "same"}}, Complete: true}
	case "projects":
		// Each project includes only one selected team; pair validation belongs
		// to the service, so the picker must not discard either project.
		d.Projects = linearprovider.Page[linearprovider.Project]{Items: []linearprovider.Project{{ID: p, Name: "same", TeamIDs: []string{a}}, {ID: q, Name: "same", TeamIDs: []string{b}}}, Complete: true}
	case "preview":
		d.BindingV2.Revision = 9 // unrelated later metadata cannot promote CAS
		d.Preview = "Exact selected project set"
	case "link":
		d.BindingV2 = &bus.TaskboardBindingV2{Configured: true, Revision: 1, MappingVersion: 2, Mode: "project_set", LinearTeamIDs: []string{a, b}, LinearDefaultTeamID: a, LinearProjectIDs: []string{p, q}}
	}
	return d, nil
}

func TestLinearV2TUIMultiProjectSelectionAndPreviewCAS(t *testing.T) {
	source := &testLinearSourceV2{}
	cfg := testConfig(&fakeAuthority{})
	cfg.Linear = source
	model, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	model, cmd := model.Show(ScreenLinear)
	model, _ = model.UpdateModel(runCommand(t, cmd))
	enter := tea.KeyMsg{Type: tea.KeyEnter}
	space := tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(" ")}
	down := tea.KeyMsg{Type: tea.KeyDown}
	for _, key := range []tea.KeyMsg{enter, space, down, space, enter, enter, down, space, down, space, enter} {
		model, cmd = model.UpdateModel(key)
		if cmd != nil {
			model, _ = model.UpdateModel(runCommand(t, cmd))
		}
	}
	snapshot := model.LinearState()
	if snapshot.Step != "preview" || snapshot.BindingV2 == nil || snapshot.BindingV2.Revision != 0 || snapshot.TeamBoard || len(snapshot.SelectedProjectIDs) != 2 {
		t.Fatal("selection or revision lost", snapshot)
	}
	preview := source.calls[len(source.calls)-1]
	if preview.LinkV2 == nil || preview.LinkV2.ExpectedRevision != 0 || preview.LinkV2.Confirm || !reflect.DeepEqual(preview.LinkV2.ProjectIDs, snapshot.SelectedProjectIDs) {
		t.Fatal("preview changed intent", preview)
	}
	model, cmd = model.UpdateModel(enter)
	model, _ = model.UpdateModel(runCommand(t, cmd))
	confirm := source.calls[len(source.calls)-1]
	if confirm.Operation != "link" || confirm.LinkV2 == nil || !confirm.LinkV2.Confirm || confirm.LinkV2.ExpectedRevision != 0 || !reflect.DeepEqual(confirm.LinkV2.ProjectIDs, preview.LinkV2.ProjectIDs) {
		t.Fatal("confirm promoted revision or changed set", confirm)
	}
	if model.LinearState().BindingV2.Revision != 1 || model.LinearState().Binding.Configured {
		t.Fatal("V2 collapsed to fabricated legacy board", model.LinearState())
	}
}

func TestLinearV2TUIUnknownContextAndCancel(t *testing.T) {
	source := &testLinearSourceV2{}
	cfg := testConfig(&fakeAuthority{})
	cfg.Linear = source
	model, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	model.screen = ScreenLinear
	model.linear.step = "details"
	issue := linearprovider.IssueV2{Issue: linearprovider.Issue{ID: linearTestTeam, Title: "safe"}}
	model.linear.issueV2, model.linear.issue = &issue, &issue.Issue
	view := renderLinear(model)
	for _, field := range []string{"Project ID: unavailable", "Parent ID: unavailable", "Assignee ID: unavailable", "Label IDs: unavailable"} {
		if !strings.Contains(view, field) {
			t.Fatal("unknown inferred absent", field)
		}
	}
	model.linear.step = "preview"
	model.linear.selectedProjects = []string{linearTestTeam}
	model, cmd := model.UpdateModel(tea.KeyMsg{Type: tea.KeyEsc})
	if cmd != nil || len(source.calls) != 0 || model.LinearState().Step != "profiles" {
		t.Fatal("cancel dispatched or changed screen", source.calls, model.LinearState())
	}
}

func TestLinearV2TUIShowsIdentifierOnlyParent(t *testing.T) {
	model, err := New(testConfig(&fakeAuthority{}))
	if err != nil {
		t.Fatal(err)
	}
	model.screen = ScreenLinear
	model.linear.step = "details"
	issue := linearprovider.IssueV2{Issue: linearprovider.Issue{ID: linearTestTeam, Title: "safe"}, ParentKnown: true, ParentIdentifier: "SYN-4"}
	model.linear.issueV2, model.linear.issue = &issue, &issue.Issue
	if view := renderLinear(model); !strings.Contains(view, "Parent ID: SYN-4") || strings.Contains(view, "Parent ID: none") {
		t.Fatal("identifier-only parent shown as absent", view)
	}
	issue.ParentIdentifier = ""
	if view := renderLinear(model); !strings.Contains(view, "Parent ID: none") {
		t.Fatal("known absent parent not shown as none", view)
	}
}
