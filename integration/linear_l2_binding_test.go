package integration

import (
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"testing"

	"clawbridge/internal/bus"
)

const l2ProjectB = "55555555-5555-4555-8555-555555555555"

func l2MappingArgs(revision int64) map[string]any {
	return map[string]any{"expected_revision": revision, "mode": "project_set", "linear_team_ids": []string{l1TeamB, l1TeamA}, "linear_default_team_id": l1TeamA, "linear_project_ids": []string{l2ProjectB, l1Project}}
}

func l2CheckBinding(t *testing.T, value any, configured bool, revision int64) map[string]any {
	t.Helper()
	m := object(t, jsonData(t, value))
	keys := func(m map[string]any) []string {
		out := []string{}
		for k := range m {
			out = append(out, k)
		}
		sort.Strings(out)
		return out
	}
	if got := keys(m); !reflect.DeepEqual(got, []string{"binding", "configured", "mapping_version", "project_id", "revision", "updated_at", "workspace_id"}) {
		t.Fatalf("V2 envelope fields=%v", got)
	}
	if m["mapping_version"] != float64(2) || m["configured"] != configured || m["revision"] != float64(revision) {
		t.Fatalf("wrong V2 state: %v", m)
	}
	if _, err := bus.ParseTaskboardBindingV2(value); err != nil {
		t.Fatalf("public parser rejected authority result: %v", err)
	}
	if configured {
		b := object(t, m["binding"])
		if got := keys(b); !reflect.DeepEqual(got, []string{"linear_default_team_id", "linear_project_ids", "linear_team_ids", "linear_workspace_id", "mode", "provider"}) {
			t.Fatalf("V2 nested fields=%v", got)
		}
		if b["provider"] != "linear" {
			t.Fatal("caller-controlled provider")
		}
		for _, key := range []string{"linear_team_ids", "linear_project_ids"} {
			ids := []string{}
			for _, raw := range b[key].([]any) {
				ids = append(ids, raw.(string))
			}
			if !sort.StringsAreSorted(ids) {
				t.Fatalf("%s not canonical sorted set", key)
			}
		}
	} else if m["binding"] != nil {
		t.Fatal("unconfigured mapping is not null")
	}
	return m
}

func l2BindingSnapshot(t *testing.T, a *notificationAuthority) string {
	t.Helper()
	rows, err := a.db.Query(a.query(`SELECT project_id,mapping_version FROM taskboard_bindings WHERE project_id IN (SELECT id FROM projects WHERE workspace_id=?) ORDER BY project_id`), a.workspaceID)
	if err != nil {
		t.Fatal(err)
	}
	var versions [][]any
	for rows.Next() {
		var id string
		var version int
		if err := rows.Scan(&id, &version); err != nil {
			t.Fatal(err)
		}
		versions = append(versions, []any{id, version})
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	rows.Close()
	raw, _ := json.Marshal(versions)
	return l1BindingSnapshot(t, a) + "\n" + string(raw)
}

func TestL2BindingVersionedWireAndLegacyRefusalWithoutWrites(t *testing.T) {
	for _, pg := range []bool{false, true} {
		name := "SQLite"
		if pg {
			name = "Postgres"
		}
		t.Run(name, func(t *testing.T) {
			a := newNotificationAuthority(t, pg)
			if pg {
				// This wire/compatibility scenario needs an authorized mapping
				// administrator. The shared notification fixture starts Alice
				// as a member; reader/member denial is exercised separately.
				a.exec("UPDATE project_roles SET role='lead' WHERE principal=? AND project_id IN (SELECT id FROM projects WHERE workspace_id=?)", pgPrincipal("alice"), a.workspaceID)
			}
			call := func(op string, args map[string]any) bus.Result {
				t.Helper()
				r, err := a.execute(bus.Request{Operation: op, Workspace: a.workspace, Project: "p", Args: args})
				if err != nil {
					t.Fatal(op, err)
				}
				return r
			}
			l2CheckBinding(t, call("taskboard_get_v2", nil).Data, false, 0)
			l1CheckBinding(t, call("taskboard_set", l1MappingArgs(0)).Data, true, 1)
			before := l2BindingSnapshot(t, a)
			converted := l2CheckBinding(t, call("taskboard_get_v2", nil).Data, true, 1)
			b := object(t, converted["binding"])
			if b["mode"] != "team_board" || len(b["linear_project_ids"].([]any)) != 0 {
				t.Fatal("legacy teamboard incorrectly normalized")
			}
			if before != l2BindingSnapshot(t, a) {
				t.Fatal("V2 normalization rewrote legacy storage or stamped state")
			}
			multi := l2CheckBinding(t, call("taskboard_set_v2", l2MappingArgs(1)).Data, true, 2)
			if multi["workspace_id"] != converted["workspace_id"] || multi["project_id"] != converted["project_id"] {
				t.Fatal("V2 scope changed")
			}
			before = l2BindingSnapshot(t, a)
			for _, op := range []string{"taskboard_get", "taskboard_set", "taskboard_clear"} {
				args := l1MappingArgs(2)
				if op == "taskboard_get" {
					args = nil
				}
				if op == "taskboard_clear" {
					args = map[string]any{"expected_revision": 2}
				}
				_, err := a.execute(bus.Request{Operation: op, Workspace: a.workspace, Project: "p", Args: args})
				var coded interface{ Code() string }
				if !errors.As(err, &coded) || coded.Code() != "unsupported_mapping" {
					t.Errorf("legacy %s did not definitively refuse: %v", op, err)
				}
				if before != l2BindingSnapshot(t, a) {
					t.Fatalf("legacy %s refusal changed storage", op)
				}
			}
			actorName := "worker"
			if pg {
				actorName = "alice"
			}
			for _, op := range []string{"taskboard_get_v2", "taskboard_access"} {
				args := actor(actorName, nil)
				if op == "taskboard_access" {
					args["ledger"] = 1
				}
				if _, err := a.execute(bus.Request{Operation: op, Workspace: a.workspace, Project: "p", Session: actorName, Args: args}); err != nil {
					t.Fatal(op, err)
				}
				if _, err := a.execute(bus.Request{Operation: op, Workspace: a.workspace, Project: "p", Session: actorName, Args: map[string]any{"instance": "stale"}}); err == nil {
					t.Fatal("stale actor accepted", op)
				}
				if before != l2BindingSnapshot(t, a) {
					t.Fatal("read/access/fencing stamped domain", op)
				}
			}
			l2CheckBinding(t, call("taskboard_clear_v2", map[string]any{"expected_revision": 2}).Data, false, 3)
			l1CheckBinding(t, call("taskboard_get", nil).Data, false, 3)
			args := l2MappingArgs(3)
			args["linear_project_ids"] = []string{l1Project}
			l2CheckBinding(t, call("taskboard_set_v2", args).Data, true, 4)
			legacy := l1CheckBinding(t, call("taskboard_get", nil).Data, true, 4)
			if object(t, legacy["binding"])["linear_project_id"] != l1Project {
				t.Fatal("representable singleton changed legacy shape")
			}
		})
	}
}

func TestL2BindingClosedArgumentsRejectAtomically(t *testing.T) {
	a := newNotificationAuthority(t, false)
	cases := map[string]func(map[string]any){
		"missing_mode":           func(m map[string]any) { delete(m, "mode") },
		"null_mode":              func(m map[string]any) { m["mode"] = nil },
		"missing_projects":       func(m map[string]any) { delete(m, "linear_project_ids") },
		"null_projects":          func(m map[string]any) { m["linear_project_ids"] = nil },
		"duplicate_projects":     func(m map[string]any) { m["linear_project_ids"] = []string{l1Project, l1Project} },
		"wrong_project_type":     func(m map[string]any) { m["linear_project_ids"] = []any{l1Project, true} },
		"empty_projectset":       func(m map[string]any) { m["linear_project_ids"] = []string{} },
		"teamboard_with_project": func(m map[string]any) { m["mode"] = "team_board" },
		"too_many_projects": func(m map[string]any) {
			ids := []string{}
			for i := 0; i < 51; i++ {
				ids = append(ids, fmt.Sprintf("%08x-aaaa-4aaa-8aaa-aaaaaaaaaaaa", i))
			}
			m["linear_project_ids"] = ids
		},
		"duplicate_teams":   func(m map[string]any) { m["linear_team_ids"] = []string{l1TeamA, l1TeamA} },
		"caller_version":    func(m map[string]any) { m["mapping_version"] = 2 },
		"caller_provider":   func(m map[string]any) { m["provider"] = "linear" },
		"caller_profile":    func(m map[string]any) { m["profile_id"] = "SYNTHETIC_PRIVATE_PROFILE" },
		"caller_time":       func(m map[string]any) { m["updated_at"] = "2100-01-01T00:00:00Z" },
		"legacy_project":    func(m map[string]any) { m["linear_project_id"] = l1Project },
		"fraction_revision": func(m map[string]any) { m["expected_revision"] = 0.5 },
	}
	for name, change := range cases {
		t.Run(name, func(t *testing.T) {
			args := l2MappingArgs(0)
			change(args)
			before := l2BindingSnapshot(t, a)
			_, err := a.execute(bus.Request{Operation: "taskboard_set_v2", Workspace: a.workspace, Project: "p", Session: "worker", Args: actor("worker", args)})
			if err == nil {
				t.Error("invalid mapping accepted")
			}
			if err != nil && strings.Contains(err.Error(), "SYNTHETIC_PRIVATE_PROFILE") {
				t.Error("private argument echoed")
			}
			if before != l2BindingSnapshot(t, a) {
				t.Fatal("denied mutation changed authority state")
			}
		})
	}
}
