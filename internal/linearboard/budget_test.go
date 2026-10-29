package linearboard

import (
	"context"
	"errors"
	"io"
	"net/http"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"clawbridge/internal/bus"
	"clawbridge/internal/linearpace"
	"clawbridge/internal/runner"
)

// budgetRecorder answers linear_write_charge as the server would, records
// each, and stops every other request: an admit is recorded before any
// other read a write makes.
type budgetRecorder struct {
	charges []map[string]any
	older   bool
	// spend makes one paced call under each stopped request's context, as
	// the write's first Linear read would: counted if the write is
	// budgeted.
	spend *testing.T
}

func (b *budgetRecorder) Execute(ctx context.Context, r bus.Request) (bus.Result, error) {
	if r.Operation != "linear_write_charge" {
		if b.spend != nil {
			paced(b.spend, ctx, 1)
		}
		return bus.Result{}, errors.New("stop: " + r.Operation)
	}
	if b.older {
		return bus.Result{}, errors.New(`unsupported operation "linear_write_charge"`)
	}
	b.charges = append(b.charges, map[string]any{"session": r.Session, "operation_id": r.Args["operation_id"], "kind": r.Args["kind"], "calls": r.Args["calls"], "admit": r.Args["admit"] != nil})
	return budgetAnswer(r)
}

func budgetService(t *testing.T, b *budgetRecorder) *Service {
	t.Helper()
	return New(Config{Authority: "local-budget", Executor: b, Connections: fakeConnections{}, ProfileID: "profile-1", LedgerDirectory: filepath.Join(t.TempDir(), "ledger"), Open: func(context.Context, *http.Client, bool) (Provider, error) { return &fakeProvider{}, nil }})
}

// paced sends n requests to Linear through the pacing transport under
// ctx, as a provider does: each is counted for the write's budget.
func paced(t *testing.T, ctx context.Context, n int) {
	t.Helper()
	client := &http.Client{Transport: linearpace.Transport{Base: roundTrip(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader("{}")), Header: http.Header{}}, nil
	}), Pacer: linearpace.For("budget-test:"+t.Name(), linearpace.OAuthPerHour)}}
	for i := 0; i < n; i++ {
		req, _ := http.NewRequestWithContext(ctx, http.MethodPost, "https://api.linear.app/graphql", strings.NewReader("{}"))
		res, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
	}
}

type roundTrip func(*http.Request) (*http.Response, error)

func (f roundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// FR8 V4 (r3): every MCP tool that writes to Linear is counted in the
// project's hour after its calls (an agent session on a personal profile),
// never asked for room first, or is charged elsewhere: by the server (an
// app write) or by its reservation (a move). A new write tool fails here
// until it is placed.
func TestEveryAgentWriteToolIsBudgeted(t *testing.T) {
	scope := bus.Request{Workspace: "ws", Project: "prj", Session: "ses", Args: map[string]any{"instance": "i"}}
	title := "t"
	counted := map[string]struct {
		kind string
		run  func(*Service) error
	}{
		"taskboard_create": {"create", func(s *Service) error {
			_, e := s.Execute(context.Background(), scope, "create", map[string]any{"title": "t", "operation_id": "op-taskboard_create"})
			return e
		}},
		"taskboard_update": {"update", func(s *Service) error {
			_, e := s.Execute(context.Background(), scope, "update", map[string]any{"id": issueID, "title": "t", "operation_id": "op-taskboard_update"})
			return e
		}},
		"taskboard_comment": {"comment", func(s *Service) error {
			_, e := s.Execute(context.Background(), scope, "comment", map[string]any{"id": issueID, "body": "b", "operation_id": "op-taskboard_comment"})
			return e
		}},
		"taskboard_create_v2": {"create", func(s *Service) error {
			_, e := s.ExecuteV2(context.Background(), scope, "create", map[string]any{"title": "t", "operation_id": "op-taskboard_create_v2"})
			return e
		}},
		"taskboard_update_v2": {"update", func(s *Service) error {
			_, e := s.WriteV2(context.Background(), scope, WriteInputV2{WriteInput: WriteInput{Operation: "update", OperationID: "op-taskboard_update_v2", IssueID: issueID, Title: &title}})
			return e
		}},
		"taskboard_comment_v2": {"comment", func(s *Service) error {
			_, e := s.ExecuteV2(context.Background(), scope, "comment", map[string]any{"id": issueID, "body": "b", "operation_id": "op-taskboard_comment_v2"})
			return e
		}},
		"taskboard_relation_add_v2": {"relation", func(s *Service) error {
			_, e := s.ExecuteV2(context.Background(), scope, "relation_add", map[string]any{"from_id": issueID, "to_id": otherIssue, "type": "blocks", "operation_id": "op-taskboard_relation_add_v2"})
			return e
		}},
		"taskboard_relation_remove_v2": {"relation", func(s *Service) error {
			_, e := s.ExecuteV2(context.Background(), scope, "relation_remove", map[string]any{"from_id": issueID, "to_id": otherIssue, "type": "blocks", "operation_id": "op-taskboard_relation_remove_v2"})
			return e
		}},
		"taskboard_project_add_team_v2": {"project_team_add", func(s *Service) error {
			_, e := s.ExecuteV2(context.Background(), scope, "project_add_team", map[string]any{"linear_project": projectA, "team_id": teamID, "operation_id": "op-taskboard_project_add_team_v2", "confirm": true, "expected_plan_sha256": strings.Repeat("a", 64)})
			return e
		}},
	}
	elsewhere := map[string]func(*Service) error{
		// The server's app sets it, charged there.
		"taskboard_project_status_set_v2": func(s *Service) error {
			_, e := s.ExecuteV2(context.Background(), scope, "project_status_set", map[string]any{"linear_project": projectA, "type": "started", "operation_id": "op-status", "confirm": true})
			return e
		},
		// Charged with its reservation (#265).
		"taskboard_issue_move_v2": func(s *Service) error {
			_, e := s.ExecuteV2(context.Background(), scope, "issue_move", map[string]any{"id": issueID, "team_id": teamID, "operation_id": "op-move", "confirm": true, "expected_plan_sha256": strings.Repeat("a", 64)})
			return e
		},
	}
	for _, tool := range runner.AgentLinearWriteTools {
		b := &budgetRecorder{spend: t}
		s := budgetService(t, b)
		if c, ok := counted[tool]; ok {
			_ = c.run(s)
			if len(b.charges) != 1 || b.charges[0]["admit"] != false || b.charges[0]["kind"] != c.kind || b.charges[0]["operation_id"] != "op-"+tool || b.charges[0]["calls"] == 0 || b.charges[0]["session"] != "ses" {
				t.Errorf("%s isn't counted after its calls: %v", tool, b.charges)
			}
			continue
		}
		if run, ok := elsewhere[tool]; ok {
			_ = run(s)
			if len(b.charges) != 0 {
				t.Errorf("%s is charged elsewhere, but asked the budget too: %v", tool, b.charges)
			}
			continue
		}
		t.Errorf("write tool %s isn't placed: budget it (budgeted) or say where it is charged", tool)
	}
	for tool := range counted {
		if !slices.Contains(runner.AgentLinearWriteTools, tool) {
			t.Errorf("%s is no longer an agent write tool", tool)
		}
	}
}

// A write is counted after its calls, with or without a session, never
// asked for room and never refused; one that made no call (a replay from
// the ledger) charges nothing; the server's own services never; an older
// server's refusal of the charge leaves the write as it ended.
func TestBudgetedCountsAfter(t *testing.T) {
	session := bus.Request{Workspace: "ws", Project: "prj", Session: "ses", Args: map[string]any{"instance": "i"}}
	b := &budgetRecorder{}
	s := budgetService(t, b)
	failed := errors.New("linear said no")
	if e := s.budgeted(context.Background(), session, "update", "op-2", func(ctx context.Context) error { paced(t, ctx, 5); return failed }); !errors.Is(e, failed) {
		t.Fatal(e)
	}
	if len(b.charges) != 1 || b.charges[0]["admit"] != false || b.charges[0]["calls"] != 5 || b.charges[0]["session"] != "ses" {
		t.Fatal("counted after:", b.charges)
	}
	person := bus.Request{Workspace: "ws", Project: "prj"}
	b.charges = nil
	_ = s.budgeted(context.Background(), person, "comment", "op-3", func(context.Context) error { return nil })
	if len(b.charges) != 0 {
		t.Fatal("a replay that made no call was charged:", b.charges)
	}
	_ = s.budgeted(context.Background(), person, "comment", "", func(ctx context.Context) error { paced(t, ctx, 3); return nil })
	if len(b.charges) != 1 || b.charges[0]["calls"] != 3 || !strings.HasPrefix(b.charges[0]["operation_id"].(string), "budget-") {
		t.Fatal("a person's write:", b.charges)
	}
	app := &budgetRecorder{}
	served := budgetService(t, app)
	served.cfg.ServerCharged = true
	_ = served.budgeted(context.Background(), session, "create", "op-5", func(ctx context.Context) error { paced(t, ctx, 2); return nil })
	if len(app.charges) != 0 {
		t.Fatal("an app write was counted again:", app.charges)
	}
	older := budgetService(t, &budgetRecorder{older: true})
	if e := older.budgeted(context.Background(), session, "create", "op-4", func(ctx context.Context) error { paced(t, ctx, 1); return nil }); e != nil {
		t.Fatal("an older server:", e)
	}
}

// FR8 V4, the sessionless side: each write a person makes in their own
// terminal through their profile is charged after its calls, never
// admitted first.
func TestEveryPersonalWriteIsChargedAfter(t *testing.T) {
	person := bus.Request{Workspace: "ws", Project: "prj"}
	ctx := context.Background()
	title := "t"
	writes := map[string]func(*Service){
		"linear create": func(s *Service) {
			_, _ = s.ExecuteV2(ctx, person, "create", map[string]any{"title": "t", "operation_id": "op-c"})
		},
		"linear update": func(s *Service) {
			_, _ = s.WriteV2(ctx, person, WriteInputV2{WriteInput: WriteInput{Operation: "update", OperationID: "op-u", IssueID: issueID, Title: &title}})
		},
		"linear comment": func(s *Service) {
			_, _ = s.ExecuteV2(ctx, person, "comment", map[string]any{"id": issueID, "body": "b", "operation_id": "op-m"})
		},
		"linear relation add": func(s *Service) {
			_, _ = s.ExecuteV2(ctx, person, "relation_add", map[string]any{"from_id": issueID, "to_id": otherIssue, "type": "blocks", "operation_id": "op-r"})
		},
		"linear project create": func(s *Service) {
			_, _ = s.Admin(ctx, person, AdminInput{Kind: "project_create", OperationID: "op-p", TeamID: teamID, Name: "n"})
		},
		"linear label create": func(s *Service) {
			_, _ = s.Admin(ctx, person, AdminInput{Kind: "label_create", OperationID: "op-l", TeamID: teamID, Name: "n"})
		},
		"linear project update --add-team": func(s *Service) {
			_, _ = s.AddProjectTeam(ctx, person, ProjectTeamAddInput{ProjectID: projectA, TeamID: teamID, OperationID: "op-t", Confirm: true, ExpectedPlanSHA256: strings.Repeat("a", 64)})
		},
		"linear roles apply-leads": func(s *Service) { _, _ = s.ApplyLeads(ctx, person, ApplyLeadsInput{Revision: 1, Confirm: true}) },
		"linear project create --lead": func(s *Service) {
			_, _ = s.SetCreatedProjectLead(ctx, person, projectA, CreateLead{Lead: "lead", Revision: 1})
		},
		"linear roles publish": func(s *Service) {
			_, _ = s.PublishTeamRoles(ctx, person, RolesPublishInput{ParentKind: "team", ParentID: teamID, Confirm: true})
		},
		"linear seed-docs": func(s *Service) { _, _ = s.SeedDocs(ctx, person, SeedInput{Confirm: true}) },
		"linear kinds seed": func(s *Service) {
			_, _ = s.KindSeed(ctx, person, KindSeedInput{OperationID: "op-k", Confirm: true, ExpectedPlanSHA: strings.Repeat("a", 64)})
		},
		"linear view delete": func(s *Service) {
			_, _ = s.ViewDelete(ctx, person, ViewDeleteInput{ViewID: projectA, OperationID: "op-v", Confirm: true}, nil)
		},
	}
	for name, write := range writes {
		b := &budgetRecorder{spend: t}
		write(budgetService(t, b))
		if len(b.charges) != 1 || b.charges[0]["admit"] != false || b.charges[0]["calls"] == 0 {
			t.Errorf("%s isn't charged after its calls: %v", name, b.charges)
		}
	}
}
