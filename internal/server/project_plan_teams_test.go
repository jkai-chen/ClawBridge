package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"clawbridge/internal/bus"
)

const planTeamsProject = "a1a1a1a1-a1a1-4a1a-8a1a-a1a1a1a1a1a1"

// A project plan's project teams are only ever the server's own read: a
// client's are dropped from every reserve, and only a session's confirm
// is given the app's read; a failed read gives none.
func TestPlanReserveTeamsAreTheServersRead(t *testing.T) {
	reads := 0
	writer := &AppWriter{PlanTeams: func(_ context.Context, lp string) ([]string, bool, error) {
		reads++
		if lp != planTeamsProject {
			t.Fatal("read another project:", lp)
		}
		return []string{"t1"}, false, nil
	}}
	req := func(session string, confirm bool) bus.Request {
		return bus.Request{Operation: "project_plan_reserve", Session: session, Args: map[string]any{"linear_project": planTeamsProject, "confirm": confirm, "team_ids": []string{"client"}, "project_teams": []string{"forged"}, "project_teams_more": false}}
	}
	got := planReserveRequest(context.Background(), writer, req("c", true))
	if !reflect.DeepEqual(got.Args["project_teams"], []string{"t1"}) || got.Args["project_teams_more"] != false || reads != 1 {
		t.Fatal("the confirm wasn't given the server's read:", got.Args)
	}
	for name, r := range map[string]bus.Request{"preview": req("c", false), "no session": req("", true)} {
		got = planReserveRequest(context.Background(), writer, r)
		if _, ok := got.Args["project_teams"]; ok || reads != 1 {
			t.Fatal(name, ": the client's teams survived, or the app was read:", got.Args)
		}
	}
	writer.PlanTeams = func(context.Context, string) ([]string, bool, error) { return nil, false, errors.New("unreadable") }
	if got = planReserveRequest(context.Background(), writer, req("c", true)); got.Args["project_teams"] != nil || got.Args["project_teams_more"] != nil {
		t.Fatal("a failed read left teams:", got.Args)
	}
	if got = planReserveRequest(context.Background(), nil, req("c", true)); got.Args["project_teams"] != nil {
		t.Fatal("a server without an app kept the client's teams:", got.Args)
	}
	other := bus.Request{Operation: "project_team_add_reserve", Args: map[string]any{"project_teams": []string{"x"}}}
	if got = planReserveRequest(context.Background(), writer, other); got.Args["project_teams"] == nil {
		t.Fatal("another operation's args were touched")
	}
}

// argsStore answers every request and records the last one's args.
type argsStore struct{ args map[string]any }

func (s *argsStore) ExecuteAs(_ context.Context, _ string, r bus.Request) (bus.Result, error) {
	s.args = r.Args
	return bus.Result{Data: map[string]any{}}, nil
}

// Through the handler: a client's forged project_teams never reach the
// bus; a session's confirm carries the server's own read instead.
func TestThePlanReserveRouteSetsTheServersTeams(t *testing.T) {
	store := &argsStore{}
	writer := &AppWriter{PlanTeams: func(context.Context, string) ([]string, bool, error) { return []string{"server-read"}, false, nil }}
	host := httptest.NewServer(NewHandlerWithOptions(store, drainVerifier, HandlerOptions{Writer: writer}))
	defer host.Close()
	post := func(confirm bool) map[string]any {
		body := fmt.Sprintf(`{"operation":"project_plan_reserve","workspace":"w","project":"p","session":"c","args":{"instance":"i","linear_project":%q,"confirm":%v,"project_teams":["forged"],"project_teams_more":false}}`, planTeamsProject, confirm)
		res := drainPost(t, host.URL+"/v1/execute", body)
		res.Body.Close()
		if res.StatusCode != http.StatusOK {
			t.Fatal(res.Status)
		}
		return store.args
	}
	if got := post(true); !reflect.DeepEqual(got["project_teams"], []any{"server-read"}) && !reflect.DeepEqual(got["project_teams"], []string{"server-read"}) {
		t.Fatal("the confirm reached the bus without the server's read:", got)
	}
	if got := post(false); got["project_teams"] != nil {
		t.Fatal("a preview carried the client's teams:", got)
	}
}
