package linearboard

import (
	"context"
	"encoding/json"
	"net/http"
	"reflect"
	"sort"
	"strings"
	"testing"

	"clawbridge/internal/linearprovider"
)

const exampleView = "a1a1a1a1-a1a1-4a1a-8a1a-a1a1a1a1a1a1"

// viewingV2 adds the views read to the V2 fake: pages answer in order, and
// asked records each call's team, kind and cursor.
type viewingV2 struct {
	*providerV2
	pages *[]linearprovider.ViewPage
	asked *[]string
	// during runs inside the read, where a binding change can land.
	during func()
}

func (v viewingV2) Views(_ context.Context, team, kind string, o linearprovider.PageOptions) (linearprovider.ViewPage, error) {
	*v.asked = append(*v.asked, team+"|"+kind+"|"+o.Cursor)
	if v.during != nil {
		v.during()
	}
	if len(*v.pages) == 0 {
		return linearprovider.ViewPage{Page: linearprovider.Page[linearprovider.View]{Complete: true}}, nil
	}
	out := (*v.pages)[0]
	*v.pages = (*v.pages)[1:]
	return out, nil
}

func viewsTest(t *testing.T, pages ...linearprovider.ViewPage) (*Service, *authorityV2, *[]string, func(team, kind, cursor string) (linearprovider.ViewPage, error)) {
	t.Helper()
	s, a, p, scope := testV2(t)
	asked := &[]string{}
	v := viewingV2{p, &pages, asked, nil}
	s.cfg.Open = func(context.Context, *http.Client, bool) (Provider, error) { return v, nil }
	return s, a, asked, func(team, kind, cursor string) (linearprovider.ViewPage, error) {
		return s.ViewsV2(context.Background(), scope, team, kind, 50, cursor)
	}
}

func view(team, kind string) linearprovider.View {
	return linearprovider.View{ID: exampleView, Name: "Open bugs", Kind: kind, TeamID: team}
}

// A bound team's views pass through, with the provider's counts; the next
// page's cursor is bound to the team, kind and binding.
func TestViewsV2ListsABoundTeam(t *testing.T) {
	first := linearprovider.ViewPage{Page: linearprovider.Page[linearprovider.View]{Items: []linearprovider.View{view(teamID, "issue")}, NextCursor: "p2"}, WorkspaceViews: 2, OtherKinds: 1}
	_, _, asked, list := viewsTest(t, first, linearprovider.ViewPage{Page: linearprovider.Page[linearprovider.View]{Complete: true}})
	page, e := list(teamID, "issue", "")
	if e != nil || len(page.Items) != 1 || page.WorkspaceViews != 2 || page.OtherKinds != 1 || page.Complete || page.NextCursor == "" || page.NextCursor == "p2" {
		t.Fatal(page, e)
	}
	if _, e = list(teamID, "project", page.NextCursor); e == nil {
		t.Fatal("a cursor continued another kind")
	}
	if last, e := list(teamID, "issue", page.NextCursor); e != nil || !last.Complete {
		t.Fatal(last, e)
	}
	if !reflect.DeepEqual(*asked, []string{teamID + "|issue|", teamID + "|issue|p2"}) {
		t.Fatal(*asked)
	}
}

// Nothing is asked for a team outside the binding or a bad kind, and a page
// that escapes its team or kind is refused.
func TestViewsV2RefusesWhatTheBindingDoesNot(t *testing.T) {
	_, _, asked, list := viewsTest(t)
	if _, e := list("ffffffff-ffff-4fff-8fff-ffffffffffff", "", ""); !IsOutOfScope(e) {
		t.Fatal("an unbound team:", e)
	}
	for _, kind := range []string{"initiative", "Issue"} {
		if _, e := list(teamID, kind, ""); e == nil {
			t.Fatal("kind", kind)
		}
	}
	if len(*asked) != 0 {
		t.Fatal("a refused read asked Linear:", *asked)
	}
	for name, page := range map[string]linearprovider.ViewPage{
		"another team": {Page: linearprovider.Page[linearprovider.View]{Items: []linearprovider.View{view(otherTeamID, "issue")}, Complete: true}},
		"another kind": {Page: linearprovider.Page[linearprovider.View]{Items: []linearprovider.View{view(teamID, "project")}, Complete: true}},
		"no cursor":    {Page: linearprovider.Page[linearprovider.View]{Complete: false}},
	} {
		_, _, _, list := viewsTest(t, page)
		if _, e := list(teamID, "issue", ""); e == nil {
			t.Fatal(name, "accepted")
		}
	}
}

// A provider without the views read says so before anything is sent.
func TestViewsV2NeedsTheCapability(t *testing.T) {
	s, _, _, scope := testV2(t)
	_, e := s.ViewsV2(context.Background(), scope, teamID, "", 50, "")
	if e == nil || !strings.Contains(e.Error(), "capability_unavailable") {
		t.Fatal(e)
	}
}

// A binding changed while the page was read refuses it.
func TestViewsV2BindingChangedDuringTheRead(t *testing.T) {
	s, a, p, scope := testV2(t)
	asked := &[]string{}
	pages := []linearprovider.ViewPage{{Page: linearprovider.Page[linearprovider.View]{Items: []linearprovider.View{view(teamID, "issue")}, Complete: true}}}
	v := viewingV2{p, &pages, asked, func() { a.b.Revision++ }}
	s.cfg.Open = func(context.Context, *http.Client, bool) (Provider, error) { return v, nil }
	if _, e := s.ViewsV2(context.Background(), scope, teamID, "", 50, ""); e == nil {
		t.Fatal("a page read across a binding change was accepted")
	}
}

// The agents' route (ExecuteV2 "views", behind taskboard_views_v2) returns
// exactly these row fields: no description or other free text but the name.
func TestViewsV2AgentRowFields(t *testing.T) {
	s, _, p, scope := testV2(t)
	asked := &[]string{}
	pages := []linearprovider.ViewPage{{Page: linearprovider.Page[linearprovider.View]{Items: []linearprovider.View{{ID: exampleView, Name: "Open bugs", URL: "https://linear.example/v", Kind: "issue", TeamID: teamID, TeamKey: "ENG", TeamName: "Engineering"}}, Complete: true}}}
	s.cfg.Open = func(context.Context, *http.Client, bool) (Provider, error) {
		return viewingV2{p, &pages, asked, nil}, nil
	}
	out, e := s.ExecuteV2(context.Background(), scope, "views", map[string]any{"team_id": teamID, "kind": "issue"})
	if e != nil {
		t.Fatal(e)
	}
	raw, _ := json.Marshal(out)
	var decoded struct {
		Items []map[string]any `json:"items"`
	}
	if json.Unmarshal(raw, &decoded) != nil || len(decoded.Items) != 1 {
		t.Fatal(string(raw))
	}
	keys := []string{}
	for k := range decoded.Items[0] {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	if !reflect.DeepEqual(keys, []string{"id", "kind", "name", "team_id", "team_key", "team_name", "url"}) {
		t.Fatal(keys)
	}
}
