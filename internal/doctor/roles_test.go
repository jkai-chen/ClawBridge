package doctor

import (
	"context"
	"errors"
	"strings"
	"testing"

	"clawbridge/internal/linearprovider"
)

// Team roles: none set passes; a team that left the binding and a mapped
// person who is no longer an active, non-guest workspace user warn; a
// listing that fails warns and says nothing was checked.
func TestCheckTeamRoles(t *testing.T) {
	const ada, gus = "aaaaaaaa-0000-4000-8000-00000000000a", "bbbbbbbb-0000-4000-8000-00000000000b"
	mapping := func(bound bool) map[string]any {
		return map[string]any{"revision": 2, "teams": []any{map[string]any{"team_id": "t", "leads": []any{ada}, "team_bound": bound}}, "people": []any{
			map[string]any{"linear_user_id": ada, "role": "lead", "teams": []any{"t"}},
			map[string]any{"linear_user_id": gus, "role": "engineer", "teams": []any{"t"}},
		}}
	}
	users := func(list ...linearprovider.WorkspaceUser) func(context.Context, string, string) ([]linearprovider.WorkspaceUser, error) {
		return func(context.Context, string, string) ([]linearprovider.WorkspaceUser, error) { return list, nil }
	}
	active := func(id string) linearprovider.WorkspaceUser {
		return linearprovider.WorkspaceUser{ID: id, Active: true, ActiveKnown: true, GuestKnown: true}
	}
	guest := active(gus)
	guest.Guest = true
	cases := []struct {
		name   string
		data   map[string]any
		users  func(context.Context, string, string) ([]linearprovider.WorkspaceUser, error)
		status string
		words  string
	}{
		{"none", map[string]any{"revision": 0, "teams": []any{}, "people": []any{}}, nil, Pass, "No team roles"},
		{"all good", mapping(true), users(active(ada), active(gus)), Pass, "2 people"},
		{"no connection", mapping(true), nil, Pass, "2 people"},
		{"unbound team", mapping(false), users(active(ada), active(gus)), Warn, "left the binding"},
		{"a guest", mapping(true), users(active(ada), guest), Warn, gus},
		{"gone", mapping(true), users(active(ada)), Warn, "no longer active"},
		{"listing fails", mapping(true), func(context.Context, string, string) ([]linearprovider.WorkspaceUser, error) {
			return nil, errors.New("x")
		}, Warn, "could not be read"},
	}
	for _, tc := range cases {
		var got []Check
		checkTeamRoles(context.Background(), &queueCounts{data: tc.data}, "w", "p", tc.users, func(c Check) { got = append(got, c) })
		if len(got) != 1 || got[0].ID != "linear.team_roles" || got[0].Status != tc.status || !strings.Contains(got[0].Message, tc.words) {
			t.Fatalf("%s: %+v", tc.name, got)
		}
	}
}
