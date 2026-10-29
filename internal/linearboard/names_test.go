package linearboard

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"clawbridge/internal/bus"
	"clawbridge/internal/linearprovider"
)

// namesRecorder records the writes that carry names; refuseNames plays a
// server older than the names.
type namesRecorder struct {
	*authorityV2
	sets        []map[string]any
	names       map[string]any
	refuseNames bool
}

func (a *namesRecorder) Execute(ctx context.Context, r bus.Request) (bus.Result, error) {
	if r.Operation == "linear_write_charge" {
		return budgetAnswer(r)
	}
	switch r.Operation {
	case "taskboard_set_v2":
		args := map[string]any{}
		for k, v := range r.Args {
			args[k] = v
		}
		a.sets = append(a.sets, args)
		if _, ok := r.Args["linear_team_names"]; ok && a.refuseNames {
			return bus.Result{}, errors.New(`unsupported argument "linear_team_names"`)
		}
		return bus.Result{Source: "local-authority", Data: map[string]any{"revision": 2}}, nil
	case "taskboard_names_set":
		a.names = r.Args
		return bus.Result{Source: "local-authority", Data: map[string]any{"revision": a.b.Revision}}, nil
	}
	return a.authorityV2.Execute(ctx, r)
}

func namesTest(t *testing.T) (*Service, *namesRecorder, bus.Request, LinkInputV2) {
	t.Helper()
	s, a, _, scope := testV2(t)
	rec := &namesRecorder{authorityV2: a}
	s.cfg.Executor = rec
	return s, rec, scope, LinkInputV2{TeamIDs: []string{otherTeamID, teamID}, DefaultTeamID: teamID, ProjectIDs: []string{projectB, projectA}, ExpectedRevision: 1, Confirm: true}
}

// A link sends the names Linear gave, and an older server gets the same
// link without them.
func TestLinkSendsTheNames(t *testing.T) {
	s, rec, scope, in := namesTest(t)
	if _, e := s.LinkV2(context.Background(), scope, in); e != nil || len(rec.sets) != 1 {
		t.Fatal(e, rec.sets)
	}
	want := map[string]any{teamID: "Team", otherTeamID: "Team"}
	if got := rec.sets[0]["linear_team_names"]; !reflect.DeepEqual(got, want) {
		t.Fatalf("team names %v", got)
	}
	// The fake's projects have no names: none is sent rather than "".
	if got := rec.sets[0]["linear_project_names"]; !reflect.DeepEqual(got, map[string]any{}) {
		t.Fatalf("project names %v", got)
	}
	s, rec, scope, in = namesTest(t)
	rec.refuseNames = true
	if _, e := s.LinkV2(context.Background(), scope, in); e != nil || len(rec.sets) != 2 {
		t.Fatal(e, len(rec.sets))
	}
	if _, ok := rec.sets[1]["linear_team_names"]; ok || rec.sets[1]["linear_project_ids"] == nil {
		t.Fatalf("the retry: %v", rec.sets[1])
	}
}

// A refresh stores the names at the binding's own revision.
func TestRefreshNamesAtTheBindingsRevision(t *testing.T) {
	s, rec, scope, _ := namesTest(t)
	if _, e := s.RefreshNamesV2(context.Background(), scope); e != nil {
		t.Fatal(e)
	}
	if rec.names["expected_revision"] != int64(1) || !reflect.DeepEqual(rec.names["linear_team_names"], map[string]any{teamID: "Team", otherTeamID: "Team"}) || rec.names["instance"] != "secret-instance" {
		t.Fatal(rec.names)
	}
}

// What the server would refuse is cut or left out, never sent.
func TestLinkNamesMeetTheServersRule(t *testing.T) {
	long := strings.Repeat("é", 79) + " x"
	teams, projects := linkNames(verifiedV2{
		Teams:    []linearprovider.Team{{ID: "t1", Name: "  Eng  "}, {ID: "t2", Name: "Ops\u202e"}, {ID: "t3", Name: " "}},
		Projects: []linearprovider.Project{{ID: "p1", Name: long}, {ID: "p2", Name: "Web\nApp"}},
	})
	if !reflect.DeepEqual(teams, map[string]any{"t1": "Eng"}) || !reflect.DeepEqual(projects, map[string]any{"p1": strings.Repeat("é", 79)}) {
		t.Fatal(teams, projects)
	}
	for _, v := range projects {
		if !bus.LinearNameOK(v.(string)) {
			t.Fatal(v)
		}
	}
}
