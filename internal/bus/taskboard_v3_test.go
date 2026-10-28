package bus

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
)

const teamSub = "33333333-3333-4333-8333-333333333333"

func taskboardArgsV3(rev int) map[string]any {
	a := taskboardArgsV2(rev)
	a["linear_other_teams"] = map[string]any{teamSub: TeamAccessComment}
	a["linear_parent_team_id"] = teamOne
	return a
}

// A binding using version 3's fields goes on the wire as mapping_version 3
// with both fields, and reads back the same; a version 2 binding keeps its
// exact version 2 form; the legacy operations refuse a version 3 binding.
func TestTaskboardV3RoundTripAndLegacyRefusal(t *testing.T) {
	h := newHarness(t)
	v2 := h.call("taskboard_set_v2", "a", taskboardArgsV2(0))
	raw, _ := json.Marshal(v2)
	var fields map[string]any
	json.Unmarshal(raw, &fields)
	if fields["mapping_version"] != float64(2) || len(fields["binding"].(map[string]any)) != 6 {
		t.Fatal("a version 2 binding changed form:", fields)
	}
	b, e := ParseTaskboardBindingV2(h.call("taskboard_set_v2", "a", taskboardArgsV3(1)))
	if e != nil || b.MappingVersion != 3 || b.LinearOtherTeams[teamSub] != TeamAccessComment || b.LinearParentTeamID != teamOne {
		t.Fatal(b, e)
	}
	if b.TeamAccess(teamOne) != TeamAccessWrite || !b.TeamReadable(teamSub) || b.TeamAccess("44444444-4444-4444-8444-444444444444") != "" {
		t.Fatal("access", b)
	}
	got, e := ParseTaskboardBindingV2(h.call("taskboard_get_v2", "a", nil))
	if e != nil || got.MappingVersion != 3 || got.LinearOtherTeams[teamSub] != TeamAccessComment {
		t.Fatal(got, e)
	}
	// The stored column stays 2: an older server reading the row sees only
	// the write teams, so it can do less, never more.
	var version int
	h.s.db.QueryRow(`SELECT mapping_version FROM taskboard_bindings`).Scan(&version)
	if version != 2 {
		t.Fatal("stored version", version)
	}
	for _, op := range []string{"taskboard_get", "taskboard_set"} {
		a := map[string]any{}
		if op == "taskboard_set" {
			a = taskboardArgs(2)
		}
		_, e = h.s.Execute(context.Background(), h.request(op, "a", a))
		if c, ok := e.(interface{ Code() string }); !ok || c.Code() != "unsupported_mapping" {
			t.Fatal(op, "served a version 3 binding:", e)
		}
	}
}

// Version 3's fields are checked as strictly as version 2's.
func TestTaskboardV3StrictFields(t *testing.T) {
	h := newHarness(t)
	bad := []func(map[string]any){
		func(a map[string]any) { a["linear_other_teams"] = map[string]any{teamOne: TeamAccessRead} },
		func(a map[string]any) { a["linear_other_teams"] = map[string]any{teamSub: "admin"} },
		func(a map[string]any) { a["linear_other_teams"] = map[string]any{"FIX": TeamAccessRead} },
		func(a map[string]any) { a["linear_other_teams"] = "nope" },
		func(a map[string]any) { a["linear_parent_team_id"] = "44444444-4444-4444-8444-444444444444" },
		func(a map[string]any) {
			others := map[string]any{}
			for i := 0; i < 20; i++ {
				others[uuidN(i)] = TeamAccessRead
			}
			a["linear_other_teams"] = others
		},
	}
	for _, change := range bad {
		a := taskboardArgsV3(0)
		change(a)
		h.fail("taskboard_set_v2", "a", a)
	}
	// 19 others plus the write team is the most a binding lists.
	a := taskboardArgsV3(0)
	others := map[string]any{}
	for i := 0; i < 19; i++ {
		others[uuidN(i)] = TeamAccessRead
	}
	a["linear_other_teams"] = others
	h.call("taskboard_set_v2", "a", a)
	// On the wire: mapping_version 3 needs both fields; 2 refuses them.
	value := h.call("taskboard_set_v2", "a", taskboardArgsV3(1))
	raw, _ := json.Marshal(value)
	for _, change := range []func(map[string]any){
		func(m map[string]any) { m["mapping_version"] = 2 },
		func(m map[string]any) { delete(m["binding"].(map[string]any), "linear_parent_team_id") },
		func(m map[string]any) { m["mapping_version"] = 4 },
		// Version 3 whose fields say nothing is a version 2 binding.
		func(m map[string]any) {
			b := m["binding"].(map[string]any)
			b["linear_other_teams"], b["linear_parent_team_id"] = map[string]any{}, nil
		},
	} {
		var m map[string]any
		json.Unmarshal(raw, &m)
		change(m)
		if _, e := ParseTaskboardBindingV2(m); e == nil {
			t.Fatal("accepted", m)
		}
	}
}

func uuidN(i int) string {
	return fmt.Sprintf("55555555-5555-4555-8555-%012x", i)
}
