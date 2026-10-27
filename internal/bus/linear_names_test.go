package bus

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

func (h *harness) linkNamed(rev int, projects []string, teamNames, projectNames map[string]any) {
	h.t.Helper()
	a := map[string]any{"expected_revision": rev, "mode": "project_set", "linear_team_ids": []string{sfTeam}, "linear_default_team_id": sfTeam, "linear_project_ids": projects}
	if teamNames != nil {
		a["linear_team_names"] = teamNames
	}
	if projectNames != nil {
		a["linear_project_names"] = projectNames
	}
	h.call("taskboard_set_v2", "", a)
}

func (h *harness) overviewNames() (teams, projects map[string]any, at any) {
	h.t.Helper()
	linear := h.overview("")["linear"].(map[string]any)
	teams, projects = map[string]any{}, map[string]any{}
	for _, t := range linear["teams"].([]map[string]any) {
		teams[t["id"].(string)] = t["name"]
	}
	for _, p := range linear["projects"].([]map[string]any) {
		projects[p["id"].(string)] = p["name"]
	}
	return teams, projects, linear["names_updated_at"]
}

func (h *harness) bindingRevision() int64 {
	h.t.Helper()
	return h.call("taskboard_get_v2", "", nil).(map[string]any)["revision"].(int64)
}

// A link stores the names; a refresh replaces the maps it names without
// moving the revision; a re-link without maps keeps the names of the IDs
// still bound; unlinking clears them.
func TestLinearNamesAtLinkRefreshAndUnlink(t *testing.T) {
	h := newHarness(t)
	h.linkNamed(0, []string{cpOne, cpTwo}, map[string]any{sfTeam: "Eng"}, map[string]any{cpOne: " Payments ", cpTwo: "Web"})
	teams, projects, at := h.overviewNames()
	if teams[sfTeam] != "Eng" || projects[cpOne] != "Payments" || projects[cpTwo] != "Web" || at == nil {
		t.Fatalf("after link: %v %v %v", teams, projects, at)
	}

	got := h.call("taskboard_names_set", "", map[string]any{"expected_revision": 1, "linear_project_names": map[string]any{cpOne: "Payments v2"}}).(map[string]any)
	if got["revision"] != int64(1) || h.bindingRevision() != 1 {
		t.Fatalf("a refresh moved the revision: %v, binding %d", got, h.bindingRevision())
	}
	teams, projects, _ = h.overviewNames()
	if teams[sfTeam] != "Eng" || projects[cpOne] != "Payments v2" || projects[cpTwo] != "" {
		t.Fatalf("after refresh: %v %v", teams, projects)
	}
	h.fail("taskboard_names_set", "", map[string]any{"expected_revision": 0, "linear_team_names": map[string]any{sfTeam: "Stale"}})
	if teams, _, _ = h.overviewNames(); teams[sfTeam] != "Eng" {
		t.Fatalf("a stale refresh landed: %v", teams)
	}

	// Re-link without maps: cpOne stays bound and keeps its name; cpThree
	// is new and has none.
	h.link(1, []string{cpOne, cpThree})
	teams, projects, at = h.overviewNames()
	if teams[sfTeam] != "Eng" || projects[cpOne] != "Payments v2" || projects[cpThree] != "" || at == nil {
		t.Fatalf("after re-link: %v %v %v", teams, projects, at)
	}
	var stored string
	if err := h.s.db.QueryRow(`SELECT names FROM taskboard_bindings`).Scan(&stored); err != nil || strings.Contains(stored, cpTwo) || strings.Contains(stored, "Web") || strings.Contains(stored, cpThree) {
		t.Fatalf("an unbound ID's name stayed: %q %v", stored, err)
	}

	h.call("taskboard_clear_v2", "", map[string]any{"expected_revision": 2})
	var cleared any
	if err := h.s.db.QueryRow(`SELECT names FROM taskboard_bindings`).Scan(&cleared); err != nil || cleared != nil {
		t.Fatalf("unlink left names: %v %v", cleared, err)
	}
	h.fail("taskboard_names_set", "", map[string]any{"expected_revision": 3, "linear_team_names": map[string]any{sfTeam: "Eng"}})
	h.link(3, []string{cpOne})
	if teams, projects, at = h.overviewNames(); teams[sfTeam] != "" || projects[cpOne] != "" || at != nil {
		t.Fatalf("names came back after unlink: %v %v %v", teams, projects, at)
	}
	if err := h.s.db.QueryRow(`SELECT names FROM taskboard_bindings`).Scan(&cleared); err != nil || cleared != nil {
		t.Fatalf("a link without names stored %v %v", cleared, err)
	}
}

// The audit records that names were set, never the names.
func TestLinearNamesAuditHasNoNames(t *testing.T) {
	h := newHarness(t)
	h.linkNamed(0, []string{cpOne}, map[string]any{sfTeam: "team-name-sentinel"}, map[string]any{cpOne: "project-name-sentinel"})
	h.call("taskboard_names_set", "", map[string]any{"expected_revision": 1, "linear_team_names": map[string]any{sfTeam: "refresh-name-sentinel"}})
	rows, err := h.s.db.Query(`SELECT action,details FROM history WHERE entity='taskboard'`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	actions := []string{}
	for rows.Next() {
		var action, details string
		if err = rows.Scan(&action, &details); err != nil {
			t.Fatal(err)
		}
		actions = append(actions, action)
		if strings.Contains(details, "sentinel") {
			t.Fatalf("%s audits a name: %s", action, details)
		}
	}
	if !reflect.DeepEqual(actions, []string{"taskboard_set_v2", "taskboard_names_set"}) {
		t.Fatalf("audited %v", actions)
	}
}

func TestLinearNamesRefused(t *testing.T) {
	h := newHarness(t)
	set := func(a map[string]any) error {
		_, err := h.s.Execute(context.Background(), h.request("taskboard_names_set", "", a))
		return err
	}
	if err := set(map[string]any{"expected_revision": 0, "linear_team_names": map[string]any{sfTeam: "Eng"}}); err == nil || !strings.Contains(err.Error(), "link the task board first") {
		t.Fatalf("unlinked: %v", err)
	}
	h.link(0, []string{cpOne})
	many := map[string]any{}
	for i := 0; i <= maxLinearNames; i++ {
		many[fmt.Sprintf("id-%03d", i)] = "x"
	}
	for name, a := range map[string]map[string]any{
		"no maps":          {},
		"not an object":    {"linear_team_names": "Eng"},
		"unbound team":     {"linear_team_names": map[string]any{cpOne: "Eng"}},
		"unbound project":  {"linear_project_names": map[string]any{cpTwo: "Web"}},
		"not a string":     {"linear_team_names": map[string]any{sfTeam: 7}},
		"empty":            {"linear_team_names": map[string]any{sfTeam: "  "}},
		"too long":         {"linear_team_names": map[string]any{sfTeam: strings.Repeat("é", maxLinearNameRune+1)}},
		"control":          {"linear_team_names": map[string]any{sfTeam: "Eng\x1b[31m"}},
		"newline":          {"linear_team_names": map[string]any{sfTeam: "Eng\nOps"}},
		"bidi override":    {"linear_team_names": map[string]any{sfTeam: "Eng\u202e"}},
		"bidi isolate":     {"linear_team_names": map[string]any{sfTeam: "\u2066Eng"}},
		"zero width":       {"linear_team_names": map[string]any{sfTeam: "E\u200bng"}},
		"invalid utf-8":    {"linear_team_names": map[string]any{sfTeam: "Eng\xff"}},
		"too many entries": {"linear_project_names": many},
	} {
		a["expected_revision"] = 1
		if err := set(a); err == nil {
			t.Fatalf("%s: accepted", name)
		}
	}
	// The cap holds before any key is looked at.
	if err := set(map[string]any{"expected_revision": 1, "linear_project_names": many}); err == nil || !strings.Contains(err.Error(), "at most 100") {
		t.Fatalf("too many: %v", err)
	}
	// The same rules hold at link, and a refused link changes nothing.
	if _, err := h.s.Execute(context.Background(), h.request("taskboard_set_v2", "", map[string]any{"expected_revision": 1, "mode": "project_set", "linear_team_ids": []string{sfTeam}, "linear_default_team_id": sfTeam,
		"linear_project_ids": []string{cpOne}, "linear_project_names": map[string]any{cpTwo: "Web"}})); err == nil {
		t.Fatal("a link named an ID outside it")
	}
	if h.bindingRevision() != 1 {
		t.Fatal("a refused link moved the revision")
	}
	// The longest name and the most entries pass.
	if err := set(map[string]any{"expected_revision": 1, "linear_team_names": map[string]any{sfTeam: strings.Repeat("é", maxLinearNameRune)}}); err != nil {
		t.Fatal(err)
	}
}

// M3: names never decide anything. A name equal to another project's ID
// leaves the binding, who closes, and every notice exactly as without
// names, and no message carries a name.
func TestLinearNameEqualToAnIDChangesNothing(t *testing.T) {
	run := func(named bool) (map[string]any, []string, []map[string]any) {
		h := newHarness(t)
		h.call("coordinator_set", "c", map[string]any{"target_session": "c"})
		link := func(rev int, projects []string) {
			if named {
				h.linkNamed(rev, projects, map[string]any{sfTeam: cpOne}, map[string]any{cpOne: cpTwo, cpTwo: "name-sentinel"})
			} else {
				h.link(rev, projects)
			}
		}
		link(0, []string{cpOne, cpTwo})
		h.call("status_policy_set", "", map[string]any{"expected_revision": 0, "status_policy": sfPolicy(true)})
		if _, err := h.closesSet(1, cpTwo, "agent", "cli-tty"); err != nil {
			t.Fatal(err)
		}
		// cpTwo leaves and comes back: a close-policy notice about it.
		h.link(1, []string{cpOne})
		link(2, []string{cpOne, cpTwo})
		binding := h.call("taskboard_get_v2", "", nil).(map[string]any)
		for _, k := range []string{"updated_at", "project_id", "workspace_id"} {
			delete(binding, k)
		}
		var bodies []string
		rows, err := h.s.db.Query(`SELECT body FROM messages ORDER BY sequence`)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		for rows.Next() {
			var body string
			if err = rows.Scan(&body); err != nil {
				t.Fatal(err)
			}
			if strings.Contains(body, "name-sentinel") {
				t.Fatalf("a message carries a name: %s", body)
			}
			bodies = append(bodies, body)
		}
		projects := h.overview("")["linear"].(map[string]any)["projects"].([]map[string]any)
		for _, p := range projects {
			delete(p, "name")
		}
		return binding, bodies, projects
	}
	b1, n1, p1 := run(false)
	b2, n2, p2 := run(true)
	if len(n1) == 0 {
		t.Fatal("no notice to compare")
	}
	if !reflect.DeepEqual(b1, b2) || !reflect.DeepEqual(n1, n2) || !reflect.DeepEqual(p1, p2) {
		t.Fatalf("names changed an outcome:\n%v\n%v\n%v\n%v\n%v\n%v", b1, b2, n1, n2, p1, p2)
	}
}
