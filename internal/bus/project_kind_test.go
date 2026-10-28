package bus

import (
	"reflect"
	"strings"
	"testing"
)

func kindSet(rev int, team, group string, values ...string) map[string]any {
	a := map[string]any{"expected_revision": rev, "kinds": values}
	if team != "" {
		a["team_id"] = team
	}
	if group != "" {
		a["group_name"] = group
	}
	return a
}

// The setting: unset, it reads as revision 0 with the default group; the
// owner sets a group and values on a bound team (the default team when
// none is named); any session reads it; history records the scope.
func TestProjectKindSetAndRead(t *testing.T) {
	h := newHarness(t)
	got := h.call("project_kind_get", "", nil).(map[string]any)
	if got["revision"] != 0 || got["group_name"] != "Type" || got["seeded"] != false || len(got["values"].([]ProjectKindValue)) != 0 {
		t.Fatal(got)
	}
	if err := h.viewErr("project_kind_set", "", kindSet(0, "", "", "Engineering")); !strings.Contains(err.Error(), "isn't linked") {
		t.Fatal(err)
	}
	h.link(0, []string{cpOne})
	got = h.call("project_kind_set", "", kindSet(0, "", "", " Engineering ", "Review", "Test")).(map[string]any)
	want := []ProjectKindValue{{Name: "Engineering"}, {Name: "Review"}, {Name: "Test"}}
	if got["revision"] != 1 || got["team_id"] != sfTeam || got["scope"] != "team" || got["group_name"] != "Type" || !reflect.DeepEqual(got["values"], want) || got["seeded"] != false {
		t.Fatal(got)
	}
	h.call("register_session", "kind-s", map[string]any{"instance": "kind-i", "role": "worker"})
	if read := h.call("project_kind_get", "kind-s", map[string]any{"instance": "kind-i"}).(map[string]any); read["revision"] != 1 {
		t.Fatal(read)
	}
	var details string
	if err := h.s.db.QueryRow(`SELECT details FROM history WHERE entity='project_kind_policy' ORDER BY sequence DESC LIMIT 1`).Scan(&details); err != nil || !strings.Contains(details, `"scope":"team:`+sfTeam+`"`) || strings.Contains(details, "Engineering") {
		t.Fatal(details, err)
	}
}

// Refused: a session, a stale revision, a team outside the binding,
// workspace scope (no binding is workspace-wide), duplicate values, a value
// named as the group, no values, too many, an unsafe name.
func TestProjectKindSetRefusals(t *testing.T) {
	h := newHarness(t)
	h.link(0, []string{cpOne})
	h.call("register_session", "kind-s", map[string]any{"instance": "kind-i", "role": "worker"})
	session := kindSet(0, "", "", "Engineering")
	session["instance"] = "kind-i"
	if err := h.viewErr("project_kind_set", "kind-s", session); !strings.Contains(err.Error(), "a session can't") {
		t.Fatal(err)
	}
	many := make([]string, 21)
	for i := range many {
		many[i] = "K" + strings.Repeat("x", i)
	}
	workspace := kindSet(0, "", "", "Engineering")
	workspace["scope"] = "workspace"
	for want, a := range map[string]map[string]any{
		"current revision is 0":      kindSet(1, "", "", "Engineering"),
		"a team of this project's":   kindSet(0, "11111111-1111-4111-8111-111111111119", "", "Engineering"),
		"workspace scope is refused": workspace,
		"names are unique":           kindSet(0, "", "", "Review", "review"),
		"none is the group's name":   kindSet(0, "", "Kind", "Kind"),
		"list of 1 to 20":            kindSet(0, "", "", many...),
		"no control":                 kindSet(0, "", "", "a\u202eb"),
		"group_name must be":         kindSet(0, "", "a\x1bb", "Engineering"),
	} {
		if err := h.viewErr("project_kind_set", "", a); !strings.Contains(err.Error(), want) {
			t.Fatal(want, err)
		}
	}
	if err := h.viewErr("project_kind_set", "", map[string]any{"expected_revision": 0}); !strings.Contains(err.Error(), "list of 1 to 20") {
		t.Fatal(err)
	}
}

// A value keeps its label ID while the group and team stay; renaming the
// group clears every ID, for the new group to prove.
func TestProjectKindKeepsIDsOnlyForTheSameGroup(t *testing.T) {
	h := newHarness(t)
	h.link(0, []string{cpOne})
	h.call("project_kind_set", "", kindSet(0, "", "", "Engineering", "Review"))
	if _, err := h.s.db.Exec(`UPDATE project_kind_policy SET group_label_id='e7e7e7e7-0000-4000-8000-000000000001', kind_values='[{"name":"Engineering","label_id":"e7e7e7e7-0000-4000-8000-000000000002"},{"name":"Review","label_id":"e7e7e7e7-0000-4000-8000-000000000003"}]'`); err != nil {
		t.Fatal(err)
	}
	if got := h.call("project_kind_get", "", nil).(map[string]any); got["seeded"] != true {
		t.Fatal(got)
	}
	got := h.call("project_kind_set", "", kindSet(1, "", "", "engineering", "Test")).(map[string]any)
	want := []ProjectKindValue{{Name: "engineering", LabelID: "e7e7e7e7-0000-4000-8000-000000000002"}, {Name: "Test"}}
	if !reflect.DeepEqual(got["values"], want) || got["group_label_id"] != "e7e7e7e7-0000-4000-8000-000000000001" || got["seeded"] != false {
		t.Fatal(got)
	}
	got = h.call("project_kind_set", "", kindSet(2, "", "Kind", "Engineering")).(map[string]any)
	if !reflect.DeepEqual(got["values"], []ProjectKindValue{{Name: "Engineering"}}) || got["group_label_id"] != nil {
		t.Fatal(got)
	}
}
