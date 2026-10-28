package bus

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"reflect"
	"strings"
	"testing"
)

const (
	lvView  = "7777aaaa-7777-4777-8777-777777777771"
	lvView2 = "7777aaaa-7777-4777-8777-777777777772"
	lvOther = "7777aaaa-7777-4777-8777-777777777779"
)

const lvFilter = `{"priority":{"eq":1}}`

func lvRecord(action, view, name, filter, asOf, expected string, rev int) map[string]any {
	sum := sha256.Sum256([]byte(filter))
	a := map[string]any{"action": action, "view_id": view, "team_id": sfTeam, "name": name, "filter": filter, "filter_sha256": hex.EncodeToString(sum[:]), "filter_as_of": asOf, "agent_filter": "available", "expected_binding_revision": rev}
	if expected != "" {
		a["expected_filter_as_of"] = expected
	}
	return a
}

func (h *harness) viewErr(op, session string, a map[string]any) error {
	h.t.Helper()
	_, err := h.s.Execute(context.Background(), h.request(op, session, a))
	if err == nil {
		h.t.Fatalf("%s unexpectedly succeeded: %v", op, a)
	}
	return err
}

// The owner records a view after its read-back; members and sessions list
// and read it; an update follows the stored row and keeps its team; a
// replay answers the stored row; history carries the scope and the digest,
// never the filter or the name.
func TestLinearViewRecordAndRead(t *testing.T) {
	h := newHarness(t)
	h.link(0, []string{cpOne})
	got := h.call("linear_view_record", "", lvRecord("create", lvView, "Urgent bugs", lvFilter, "2026-10-27T21:00:00.000Z", "", 1)).(map[string]any)
	view := got["view"].(map[string]any)
	if got["replayed"] != false || view["scope"] != "team" || view["team_id"] != sfTeam || view["kind"] != "issue" || view["agent_filter"] != "available" || view["team_bound"] != true || view["adopted"] != false || view["filter"] != nil {
		t.Fatal(got)
	}
	if again := h.call("linear_view_record", "", lvRecord("create", lvView, "Urgent bugs", lvFilter, "2026-10-27T21:00:00.000Z", "", 1)).(map[string]any); again["replayed"] != true {
		t.Fatal("a replay wrote again:", again)
	}
	// A session reads one with its filter, and the list without.
	one := h.call("linear_view_get", "a", map[string]any{"view_id": lvView}).(map[string]any)
	if one["filter"] != lvFilter || one["name"] != "Urgent bugs" || !reflect.DeepEqual(one["uses"], []string{}) {
		t.Fatal(one)
	}
	list := h.call("linear_views_list", "", nil).(map[string]any)["views"].([]any)
	if len(list) != 1 || list[0].(map[string]any)["filter"] != nil {
		t.Fatal(list)
	}
	if err := h.viewErr("linear_view_get", "a", map[string]any{"view_id": lvOther}); !strings.Contains(err.Error(), "ClawBridge filters only by views it wrote") {
		t.Fatal(err)
	}

	wide := `{"or":[{"priority":{"eq":1}}]}`
	up := lvRecord("update", lvView, "Urgent", wide, "2026-10-27T21:05:00.000Z", "2026-10-27T21:00:00.000Z", 1)
	up["agent_filter"], up["uses"] = "unavailable", []any{"or"}
	view = h.call("linear_view_record", "", up).(map[string]any)["view"].(map[string]any)
	if view["name"] != "Urgent" || view["agent_filter"] != "unavailable" || !reflect.DeepEqual(view["uses"], []string{"or"}) || view["filter_as_of"] != "2026-10-27T21:05:00.000Z" {
		t.Fatal(view)
	}
	// A stale update, a moved team, and an update of a view never recorded
	// are refused.
	stale := lvRecord("update", lvView, "Other", lvFilter, "2026-10-27T21:09:00.000Z", "2026-10-27T21:00:00.000Z", 1)
	if err := h.viewErr("linear_view_record", "", stale); !strings.Contains(err.Error(), "changed since the preview") {
		t.Fatal(err)
	}
	moved := lvRecord("update", lvView, "Other", lvFilter, "2026-10-27T21:09:00.000Z", "2026-10-27T21:05:00.000Z", 1)
	moved["team_id"] = lvOther
	if err := h.viewErr("linear_view_record", "", moved); !strings.Contains(err.Error(), "isn't bound") {
		t.Fatal(err)
	}
	// Another bound team is still another team.
	other := "11111111-1111-4111-8111-111111111112"
	h.call("taskboard_set_v2", "", map[string]any{"expected_revision": 1, "mode": "project_set", "linear_team_ids": []string{sfTeam, other}, "linear_default_team_id": sfTeam, "linear_project_ids": []string{cpOne}})
	moved = lvRecord("update", lvView, "Other", lvFilter, "2026-10-27T21:09:00.000Z", "2026-10-27T21:05:00.000Z", 2)
	moved["team_id"] = other
	if err := h.viewErr("linear_view_record", "", moved); !strings.Contains(err.Error(), "keeps the view's team") {
		t.Fatal(err)
	}
	if err := h.viewErr("linear_view_record", "", lvRecord("update", lvView2, "X", lvFilter, "2026-10-27T21:09:00.000Z", "2026-10-27T21:05:00.000Z", 2)); !strings.Contains(err.Error(), "updates only views it wrote") {
		t.Fatal(err)
	}
	if err := h.viewErr("linear_view_record", "", lvRecord("create", lvView, "Again", lvFilter, "2026-10-27T21:09:00.000Z", "", 2)); !strings.Contains(err.Error(), "already recorded") {
		t.Fatal(err)
	}

	var details string
	rows, err := h.s.db.Query(`SELECT action,details FROM history WHERE entity='linear_view' ORDER BY sequence`)
	if err != nil {
		t.Fatal(err)
	}
	var actions []string
	for rows.Next() {
		var action string
		if err = rows.Scan(&action, &details); err != nil {
			t.Fatal(err)
		}
		actions = append(actions, action)
		if !strings.Contains(details, `"scope":"team:`+sfTeam+`"`) || !strings.Contains(details, `"shared":true`) || !strings.Contains(details, `"filter_sha256"`) || strings.Contains(details, "Urgent") || strings.Contains(details, "priority") {
			t.Fatal(details)
		}
	}
	rows.Close()
	if !reflect.DeepEqual(actions, []string{"view_created", "view_updated"}) {
		t.Fatal(actions)
	}
}

// Only the owner, without a session, records; the binding must be the
// one the preview read and hold the team.
func TestLinearViewRecordRefusals(t *testing.T) {
	h := newHarness(t)
	ok := func() map[string]any {
		return lvRecord("create", lvView, "Urgent bugs", lvFilter, "2026-10-27T21:00:00.000Z", "", 1)
	}
	if err := h.viewErr("linear_view_record", "", ok()); !strings.Contains(err.Error(), "link the task board first") {
		t.Fatal(err)
	}
	h.link(0, []string{cpOne})
	var se *serviceError
	if err := h.viewErr("linear_view_record", "a", ok()); !errors.As(err, &se) || se.code != "forbidden" {
		t.Fatal(err)
	}
	if err := h.viewErr("linear_view_record", "", map[string]any{"instance": "a-instance"}); !strings.Contains(err.Error(), "instance requires session") {
		t.Fatal(err)
	}
	stale := ok()
	stale["expected_binding_revision"] = 2
	if err := h.viewErr("linear_view_record", "", stale); !errors.As(err, &se) || se.code != "conflict" {
		t.Fatal(err)
	}
	for name, mod := range map[string]func(map[string]any){
		"action":        func(a map[string]any) { a["action"] = "delete" },
		"view id":       func(a map[string]any) { a["view_id"] = strings.ToUpper("abcdef00-0000-4000-8000-00000000000a") },
		"team":          func(a map[string]any) { a["team_id"] = "eng" },
		"name":          func(a map[string]any) { a["name"] = "Bad\u202ename" },
		"empty name":    func(a map[string]any) { a["name"] = " " },
		"digest":        func(a map[string]any) { a["filter_sha256"] = strings.Repeat("0", 64) },
		"size":          func(a map[string]any) { a["filter"] = strings.Repeat("x", 16<<10+1) },
		"as of":         func(a map[string]any) { a["filter_as_of"] = "yesterday" },
		"expected":      func(a map[string]any) { a["expected_filter_as_of"] = "2026-10-27T21:00:00.000Z" },
		"uses":          func(a map[string]any) { a["agent_filter"], a["uses"] = "unavailable", []any{"or;drop"} },
		"uses mismatch": func(a map[string]any) { a["uses"] = []any{"or"} },
		"unavailable":   func(a map[string]any) { a["agent_filter"] = "unavailable" },
		"revision":      func(a map[string]any) { a["expected_binding_revision"] = 0 },
	} {
		a := ok()
		mod(a)
		if err := h.viewErr("linear_view_record", "", a); !errors.As(err, &se) || se.code != "invalid" {
			t.Fatal(name, err)
		}
	}
	if n := count(t, h.s.db, "linear_views"); n != 0 {
		t.Fatal("a refused record stored", n)
	}
}

// A view whose team left the binding stays listed with team_bound false.
func TestLinearViewTeamLeftTheBinding(t *testing.T) {
	h := newHarness(t)
	h.link(0, []string{cpOne})
	h.call("linear_view_record", "", lvRecord("create", lvView, "Urgent bugs", lvFilter, "2026-10-27T21:00:00.000Z", "", 1))
	h.call("taskboard_clear_v2", "", map[string]any{"expected_revision": 1})
	if v := h.call("linear_view_get", "", map[string]any{"view_id": lvView}).(map[string]any); v["team_bound"] != false {
		t.Fatal(v)
	}
	if v := h.call("linear_views_list", "", nil).(map[string]any)["views"].([]any)[0].(map[string]any); v["team_bound"] != false {
		t.Fatal(v)
	}
}

// viewsDowngradeSQLite returns a schema-37 database to schema 36's shape.
const viewsDowngradeSQLite = "DROP TABLE linear_views;\n"

// viewsDowngradePostgres is viewsDowngradeSQLite for a Postgres schema.
func viewsDowngradePostgres(schema string) string {
	return "DROP TABLE " + schema + ".linear_views;\n"
}

// The owner forgets a view deleted in Linear: only the row the preview
// read, once; a second call is a replay. History says view_deleted with
// the scope.
func TestLinearViewForget(t *testing.T) {
	h := newHarness(t)
	h.link(0, []string{cpOne})
	h.call("linear_view_record", "", lvRecord("create", lvView, "Urgent bugs", lvFilter, "2026-10-27T21:00:00.000Z", "", 1))
	if err := h.viewErr("linear_view_forget", "", map[string]any{"view_id": lvView, "expected_filter_as_of": "2026-10-27T20:00:00.000Z"}); !strings.Contains(err.Error(), "changed since the preview") {
		t.Fatal(err)
	}
	if err := h.viewErr("linear_view_forget", "a", map[string]any{"view_id": lvView, "expected_filter_as_of": "2026-10-27T21:00:00.000Z"}); !strings.Contains(err.Error(), "a session can't") {
		t.Fatal(err)
	}
	if err := h.viewErr("linear_view_forget", "", map[string]any{"view_id": lvView}); !strings.Contains(err.Error(), "required") {
		t.Fatal(err)
	}
	got := h.call("linear_view_forget", "", map[string]any{"view_id": lvView, "expected_filter_as_of": "2026-10-27T21:00:00.000Z"}).(map[string]any)
	if got["forgotten"] != true || got["replayed"] != false || count(t, h.s.db, "linear_views") != 0 {
		t.Fatal(got)
	}
	if again := h.call("linear_view_forget", "", map[string]any{"view_id": lvView, "expected_filter_as_of": "2026-10-27T21:00:00.000Z"}).(map[string]any); again["replayed"] != true {
		t.Fatal(again)
	}
	var details string
	if err := h.s.db.QueryRow(`SELECT details FROM history WHERE action='view_deleted'`).Scan(&details); err != nil || !strings.Contains(details, `"scope":"team:`+sfTeam+`"`) || strings.Contains(details, "Urgent") {
		t.Fatal(details, err)
	}
}

// An adoption records a view made in Linear as ClawBridge's (adopted
// true) once its read-back proves the new filter; history names the old
// filter's digest, never the filter. old_filter_sha256 goes with an
// adoption only.
func TestLinearViewAdoptRecord(t *testing.T) {
	h := newHarness(t)
	h.link(0, []string{cpOne})
	old := strings.Repeat("a", 64)
	bad := lvRecord("create", lvView, "Urgent bugs", lvFilter, "2026-10-27T21:00:00.000Z", "", 1)
	bad["old_filter_sha256"] = old
	if err := h.viewErr("linear_view_record", "", bad); !strings.Contains(err.Error(), "only with one") {
		t.Fatal(err)
	}
	if err := h.viewErr("linear_view_record", "", lvRecord("adopt", lvView, "Urgent bugs", lvFilter, "2026-10-27T21:00:00.000Z", "", 1)); !strings.Contains(err.Error(), "only with one") {
		t.Fatal(err)
	}
	a := lvRecord("adopt", lvView, "Urgent bugs", lvFilter, "2026-10-27T21:00:00.000Z", "", 1)
	a["old_filter_sha256"] = old
	view := h.call("linear_view_record", "", a).(map[string]any)["view"].(map[string]any)
	if view["adopted"] != true || view["scope"] != "team" {
		t.Fatal(view)
	}
	var details string
	if err := h.s.db.QueryRow(`SELECT details FROM history WHERE action='view_adopted'`).Scan(&details); err != nil || !strings.Contains(details, `"old_filter_sha256":"`+old+`"`) || strings.Contains(details, "priority") {
		t.Fatal(details, err)
	}
	// It is ClawBridge's now: a second adoption is a conflict, an update works.
	if err := h.viewErr("linear_view_record", "", map[string]any{"action": "adopt", "view_id": lvView, "team_id": sfTeam, "name": "Other", "filter": lvFilter, "filter_sha256": a["filter_sha256"], "filter_as_of": "2026-10-27T21:01:00.000Z", "agent_filter": "available", "expected_binding_revision": 1, "old_filter_sha256": old}); !strings.Contains(err.Error(), "already recorded") {
		t.Fatal(err)
	}
}
