package bus

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// taskboardArgsV4 is a project set binding teamOne (the default, carried
// by projectOne) and teamTwo, listed without a project.
func taskboardArgsV4(rev int) map[string]any {
	a := taskboardArgsV2(rev, projectOne)
	a["linear_team_ids"] = []string{teamOne, teamTwo}
	a["teams_without_project"] = []string{teamTwo}
	return a
}

// A binding listing teams without a project goes on the wire as
// mapping_version 4 with every version 3 field and the list, reads back
// the same, and the link's history names the list. The stored column stays
// 2, as for version 3.
func TestTaskboardV4RoundTrip(t *testing.T) {
	h := newHarness(t)
	value := h.call("taskboard_set_v2", "a", taskboardArgsV4(0))
	raw, _ := json.Marshal(value)
	var fields map[string]any
	json.Unmarshal(raw, &fields)
	mapping := fields["binding"].(map[string]any)
	if fields["mapping_version"] != float64(4) || len(mapping) != 11 {
		t.Fatal("a version 4 binding's form:", fields)
	}
	b, e := ParseTaskboardBindingV2(h.call("taskboard_get_v2", "a", nil))
	if e != nil || b.MappingVersion != 4 || len(b.TeamsWithoutProject) != 1 || b.TeamsWithoutProject[0] != teamTwo || b.TeamAccess(teamTwo) != TeamAccessWrite {
		t.Fatal(b, e)
	}
	var version int
	h.s.db.QueryRow(`SELECT mapping_version FROM taskboard_bindings`).Scan(&version)
	if version != 2 {
		t.Fatal("stored version", version)
	}
	var details string
	if err := h.s.db.QueryRow(`SELECT details FROM history WHERE entity='taskboard' AND details LIKE '%"revision":1%'`).Scan(&details); err != nil || !strings.Contains(details, `"teams_without_project":["`+teamTwo+`"]`) {
		t.Fatal("the link's history lacks the list:", details, err)
	}
	// Linking again without the list goes back to version 2.
	b, e = ParseTaskboardBindingV2(h.call("taskboard_set_v2", "a", func() map[string]any {
		a := taskboardArgsV2(1, projectOne)
		a["linear_team_ids"] = []string{teamOne, teamTwo}
		return a
	}()))
	if e != nil || b.MappingVersion != 2 || b.TeamsWithoutProject != nil {
		t.Fatal(b, e)
	}
	// The legacy operations refuse a version 4 binding.
	h.call("taskboard_set_v2", "a", taskboardArgsV4(2))
	if _, e = h.s.Execute(context.Background(), h.request("taskboard_get", "a", map[string]any{})); e == nil {
		t.Fatal("a legacy read served a version 4 binding")
	}
}

// The list names distinct write teams of a project set, never its
// default team, and the wire's version must match it.
func TestTaskboardV4StrictFields(t *testing.T) {
	h := newHarness(t)
	for name, change := range map[string]func(map[string]any){
		"not a write team": func(a map[string]any) { a["teams_without_project"] = []string{teamSub} },
		"twice":            func(a map[string]any) { a["teams_without_project"] = []string{teamTwo, teamTwo} },
		"the default":      func(a map[string]any) { a["teams_without_project"] = []string{teamOne} },
		"not a list":       func(a map[string]any) { a["teams_without_project"] = teamTwo },
		"a team board": func(a map[string]any) {
			a["mode"], a["linear_project_ids"] = "team_board", []string{}
		},
	} {
		a := taskboardArgsV4(0)
		change(a)
		if _, e := h.s.Execute(context.Background(), h.request("taskboard_set_v2", "a", a)); e == nil {
			t.Fatal(name, "accepted")
		}
	}
	value := h.call("taskboard_set_v2", "a", taskboardArgsV4(0))
	raw, _ := json.Marshal(value)
	for name, change := range map[string]func(map[string]any){
		"version 3":    func(m map[string]any) { m["mapping_version"] = 3 },
		"no list":      func(m map[string]any) { delete(m["binding"].(map[string]any), "teams_without_project") },
		"empty list":   func(m map[string]any) { m["binding"].(map[string]any)["teams_without_project"] = []any{} },
		"version 5":    func(m map[string]any) { m["mapping_version"] = 5 },
		"unknown team": func(m map[string]any) { m["binding"].(map[string]any)["teams_without_project"] = []any{teamSub} },
	} {
		var m map[string]any
		json.Unmarshal(raw, &m)
		change(m)
		if _, e := ParseTaskboardBindingV2(m); e == nil {
			t.Fatal(name, "accepted", m)
		}
	}
}

// A client that reads only versions 2 and 3 (v0.3.8) refuses a version 4
// binding outright on its version check, before it reads any field, so it
// neither half uses the binding nor reports an unrepresented team.
func TestTaskboardV4RefusedByAVersion3Reader(t *testing.T) {
	h := newHarness(t)
	raw, _ := json.Marshal(h.call("taskboard_set_v2", "a", taskboardArgsV4(0)))
	var m map[string]any
	json.Unmarshal(raw, &m)
	if n, _ := m["mapping_version"].(float64); n == 2 || n == 3 {
		t.Fatal("a version 3 reader would accept the wire form:", n)
	}
}
