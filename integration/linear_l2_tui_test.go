package integration

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"clawbridge/internal/bus"
	"clawbridge/internal/linearconn"
	"clawbridge/internal/linearprovider"
	"clawbridge/internal/tui"
)

func TestL2TUIPagedProjectSetPreviewRetainsOriginalRealStoreCAS(t *testing.T) {
	for _, finish := range []string{"cancel", "stale_confirm"} {
		t.Run(finish, func(t *testing.T) {
			f := newFixture(t)
			f.setup("w", "p")
			p := l2ScopedFixture()
			service := l1ServiceFor(t, f.store, p)
			scope := f.request("", "w", "p", "worker", actor("worker", nil))
			source := &l1TUISource{hook: func(ctx context.Context, r tui.LinearRequest) (tui.LinearData, error) {
				b, err := service.BindingV2(ctx, scope)
				d := tui.LinearData{BindingV2: &b}
				if err != nil {
					return d, err
				}
				switch r.Operation {
				case "profiles":
					d.Profiles = []linearconn.Profile{{ID: l1ProfileID, Name: "Synthetic personal read connection", Access: linearconn.Read}}
				case "teams":
					d.Teams = linearprovider.Page[linearprovider.Team]{Items: []linearprovider.Team{{ID: l1TeamA, Name: "Same team label"}, {ID: l1TeamB, Name: "Same team label"}}, Complete: true}
				case "projects":
					if r.Cursor == "" {
						d.Projects = linearprovider.Page[linearprovider.Project]{Items: []linearprovider.Project{}, NextCursor: "synthetic-projects-next"}
					} else {
						if r.Cursor != "synthetic-projects-next" {
							t.Fatal("project cursor changed")
						}
						d.Projects = linearprovider.Page[linearprovider.Project]{Items: []linearprovider.Project{{ID: l1Project, Name: "Same project label", TeamIDs: []string{l1TeamA}}, {ID: l2ProjectB, Name: "Same project label", TeamIDs: []string{l1TeamB}}}, Complete: true}
					}
				case "preview":
					if r.LinkV2 == nil || r.LinkV2.Confirm {
						t.Fatal("V2 preview lost explicit intent")
					}
					d.Preview, err = service.LinkV2(ctx, scope, *r.LinkV2)
					if err != nil {
						return d, err
					}
					// Another administrator changes the mapping after the exact
					// original preview; the returned metadata must not promote CAS.
					f.call("taskboard_set_v2", "w", "p", "", l2MappingArgs(0))
					fresh, e := service.BindingV2(ctx, scope)
					d.BindingV2 = &fresh
					return d, e
				case "link":
					if r.LinkV2 == nil {
						t.Fatal("confirmation downgraded to V1")
					}
					_, err = service.LinkV2(ctx, scope, *r.LinkV2)
				default:
					t.Error("unexpected project-set workflow operation", r.Operation)
				}
				return d, err
			}}
			m := l1TUIModel(t, source)
			m, cmd := m.Show(tui.Linear)
			m = l1TUIApply(t, m, cmd)
			for _, key := range []string{"enter", " ", "down", " ", "enter", "enter", "]", "down", " ", "down", " ", "enter"} {
				m, cmd = l1TUIKey(m, key)
				m = l1TUIApply(t, m, cmd)
			}
			state := m.LinearState()
			wantProjects := []string{l1Project, l2ProjectB}
			if state.Step != "preview" || state.BindingV2 == nil || state.BindingV2.Revision != 0 || !reflect.DeepEqual(state.SelectedProjectIDs, wantProjects) || state.TeamBoard {
				t.Fatalf("paged project selection or original revision lost: %+v", state)
			}
			before := l2BindingSnapshot(t, localNotificationAuthority(t, f))
			beforeCalls := len(source.recorded())
			m = m.SetSize(8, 3)
			m = m.SetSize(215, 50)
			if !reflect.DeepEqual(state, m.LinearState()) || len(source.recorded()) != beforeCalls {
				t.Fatal("resize changed selection/CAS or performed provider action")
			}
			key := "esc"
			if finish == "stale_confirm" {
				key = "enter"
			}
			m, cmd = l1TUIKey(m, key)
			m = l1TUIApply(t, m, cmd)
			if finish == "stale_confirm" {
				calls := source.recorded()
				last := calls[len(calls)-1]
				if last.Operation != "link" || last.LinkV2 == nil || last.LinkV2.ExpectedRevision != 0 || !last.LinkV2.Confirm || !reflect.DeepEqual(last.LinkV2.ProjectIDs, wantProjects) {
					t.Fatal("confirmation silently promoted stale CAS or changed project set")
				}
				if m.LinearState().Step == "tasks" {
					t.Fatal("failed stale CAS displayed success")
				}
			} else if len(source.recorded()) != beforeCalls {
				t.Fatal("cancel sent a settings mutation")
			}
			if before != l2BindingSnapshot(t, localNotificationAuthority(t, f)) || p.writes.Load() != 0 {
				t.Fatal("stale/canceled TUI overwrote mapping or mutated provider")
			}
		})
	}
}

func TestL2TUIUnknownExtendedContextRemainsUnavailable(t *testing.T) {
	issue := l2Issue(l1IssueID, l1TeamA, l1Project)
	issue.ProjectKnown = false
	issue.ParentID = l1StateID
	issue.AssigneeID = l2LabelA
	issue.LabelIDs = []string{l2LabelB}
	source := &l1TUISource{hook: func(_ context.Context, r tui.LinearRequest) (tui.LinearData, error) {
		b := bus.TaskboardBindingV2{Configured: true, Revision: 1, MappingVersion: 2, Mode: "project_set", LinearTeamIDs: []string{l1TeamA}, LinearProjectIDs: []string{l1Project}}
		d := tui.LinearData{BindingV2: &b}
		switch r.Operation {
		case "profiles":
			d.Profiles = []linearconn.Profile{{ID: l1ProfileID, Access: linearconn.Read}}
		case "issues":
			page := linearprovider.Page[linearprovider.IssueV2]{Items: []linearprovider.IssueV2{issue}, Complete: true}
			d.IssuesV2 = &page
		case "issue":
			d.IssueV2 = &issue
		default:
			t.Error("unexpected extended-context action", r.Operation)
		}
		return d, nil
	}}
	m := l1TUIModel(t, source).SetSize(215, 50)
	m, cmd := m.Show(tui.Linear)
	m = l1TUIApply(t, m, cmd)
	for _, key := range []string{"enter", "enter"} {
		m, cmd = l1TUIKey(m, key)
		m = l1TUIApply(t, m, cmd)
	}
	if m.LinearState().Step != "details" {
		t.Fatal("V2 details not reached")
	}
	view := tui.RenderPlain(m)
	for _, field := range []string{"Project ID: unavailable", "Parent ID: unavailable", "Assignee ID: unavailable", "Label IDs: unavailable"} {
		if !strings.Contains(view, field) {
			t.Fatalf("unknown context inferred absent/known: missing%s", field)
		}
	}
	if !reflect.DeepEqual(m.LinearState().IssueV2, &issue) {
		t.Fatal("display changed stored provider context")
	}
}
