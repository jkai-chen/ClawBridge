package integration

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"clawbridge/internal/bus"
)

// Synthetic global IDs, unrelated to the private live-team fixture.
const l1TeamA = "11111111-1111-4111-8111-111111111111"
const l1TeamB = "22222222-2222-4222-8222-222222222222"
const l1Project = "33333333-3333-4333-8333-333333333333"
const l1Workspace = "44444444-4444-4444-8444-444444444444"

func l1MappingArgs(revision int64) map[string]any {
	return map[string]any{"expected_revision": revision, "linear_team_ids": []string{l1TeamA, l1TeamB}, "linear_default_team_id": l1TeamA, "linear_project_id": nil}
}
func l1CheckBinding(t *testing.T, v any, configured bool, revision int64) map[string]any {
	t.Helper()
	m := object(t, jsonData(t, v))
	keys := []string{}
	for key := range m {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	want := []string{"binding", "configured", "project_id", "revision", "updated_at", "workspace_id"}
	if !reflect.DeepEqual(keys, want) {
		t.Errorf("binding envelope keys=%v want=%v", keys, want)
	}
	if m["configured"] != configured || m["revision"] != float64(revision) || m["workspace_id"] == "" || m["project_id"] == "" {
		t.Errorf("invalid canonical binding state: configured=%v revision=%v", m["configured"], m["revision"])
	}
	if revision == 0 {
		if m["updated_at"] != nil {
			t.Error("initial binding timestamp must be null")
		}
	} else {
		ts, ok := m["updated_at"].(string)
		if !ok {
			t.Error("mutated binding timestamp missing")
		} else if parsed, err := time.Parse(time.RFC3339Nano, ts); err != nil || parsed.Location() != time.UTC {
			t.Error("binding timestamp not server UTC RFC3339")
		}
	}
	if !configured {
		if m["binding"] != nil {
			t.Error("unconfigured binding must be null")
		}
		return m
	}
	b, ok := m["binding"].(map[string]any)
	if !ok {
		t.Error("configured mapping must be nested binding object")
		return m
	}
	keys = nil
	for key := range b {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	if !reflect.DeepEqual(keys, []string{"linear_default_team_id", "linear_project_id", "linear_team_ids", "linear_workspace_id", "provider"}) {
		t.Errorf("mapping is not exact five fields: %v", keys)
	}
	if b["provider"] != "linear" {
		t.Error("provider must be fixed linear")
	}
	return m
}

// Includes the new shared table without changing the historical notification
// observer or its accepted report/probes.
func l1BindingSnapshot(t *testing.T, a *notificationAuthority) string {
	t.Helper()
	rows, err := a.db.Query(a.query("SELECT project_id,revision,mapping,updated_at FROM taskboard_bindings WHERE project_id IN (SELECT id FROM projects WHERE workspace_id=?) ORDER BY project_id"), a.workspaceID)
	if err != nil {
		t.Fatal(err)
	}
	var records [][]any
	for rows.Next() {
		var id, at string
		var revision int64
		var mapping any
		if err = rows.Scan(&id, &revision, &mapping, &at); err != nil {
			t.Fatal(err)
		}
		if b, ok := mapping.([]byte); ok {
			mapping = string(b)
		}
		records = append(records, []any{id, revision, mapping, at})
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(records)
	return a.snapshot() + "\n" + string(raw)
}

func TestL1TaskboardInitialExactShapeAndNoStamp(t *testing.T) {
	for _, pg := range []bool{false, true} {
		name := "SQLite"
		if pg {
			name = "Postgres"
		}
		t.Run(name, func(t *testing.T) {
			a := newNotificationAuthority(t, pg)
			session := "worker"
			source := "local-authority"
			if pg {
				session = "alice"
				source = "server"
			}
			a.exec("UPDATE sessions SET last_seen=? WHERE project_id IN (SELECT id FROM projects WHERE workspace_id=?)", "2001-01-01T00:00:00.000000000Z", a.workspaceID)
			before := l1BindingSnapshot(t, a)
			for _, actorName := range []string{"", session, session} {
				args := map[string]any{}
				if actorName != "" {
					args = actor(actorName, nil)
				}
				r, err := a.execute(bus.Request{Operation: "taskboard_get", Workspace: a.workspace, Project: "p", Session: actorName, Args: args})
				if err != nil {
					t.Fatal(err)
				}
				if r.Source != source {
					t.Error("binding get authority source changed")
				}
				l1CheckBinding(t, r.Data, false, 0)
			}
			if before != l1BindingSnapshot(t, a) {
				t.Fatal("taskboard_get persisted heartbeat/history/binding changes")
			}
			for _, r := range []bus.Request{
				{Operation: "taskboard_get", Workspace: a.workspace, Project: "p", Session: session, Args: map[string]any{"instance": "wrong"}},
				{Operation: "taskboard_get", Workspace: a.workspace, Project: "p", Args: actor(session, nil)},
				{Operation: "taskboard_get", Workspace: a.workspace, Project: "p", Args: map[string]any{"profile": "forbidden"}},
			} {
				if _, err := a.execute(r); err == nil {
					t.Error("invalid binding get accepted")
				}
				if before != l1BindingSnapshot(t, a) {
					t.Error("denied get mutated domain")
				}
			}
		})
	}
}

func TestL1TaskboardExplicitNullTeamBoardAndTombstone(t *testing.T) {
	f := newFixture(t)
	f.setup("w", "p")
	first := l1CheckBinding(t, f.call("taskboard_set", "w", "p", "", l1MappingArgs(0)), true, 1)
	if b, ok := first["binding"].(map[string]any); ok {
		if b["linear_project_id"] != nil || b["linear_workspace_id"] != nil {
			t.Error("team-board/unavailable workspace must be explicit null")
		}
	}
	cleared := l1CheckBinding(t, f.call("taskboard_clear", "w", "p", "", map[string]any{"expected_revision": int64(1)}), false, 2)
	if !reflect.DeepEqual(cleared, l1CheckBinding(t, f.call("taskboard_get", "w", "p", "", nil), false, 2)) {
		t.Fatal("clear tombstone lost on get")
	}
	f.reject("taskboard_set", "w", "p", "", l1MappingArgs(1))
	args := l1MappingArgs(2)
	args["linear_project_id"] = l1Project
	args["linear_workspace_id"] = l1Workspace
	updated := l1CheckBinding(t, f.call("taskboard_set", "w", "p", "worker", actor("worker", args)), true, 3)
	if b, ok := updated["binding"].(map[string]any); ok {
		if b["linear_project_id"] != l1Project || b["linear_workspace_id"] != l1Workspace {
			t.Error("optional verified IDs lost")
		}
	}
	// Clear while already unconfigured still has a monotonic revision record.
	f.call("taskboard_clear", "w", "p", "", map[string]any{"expected_revision": int64(3)})
	l1CheckBinding(t, f.call("taskboard_clear", "w", "p", "", map[string]any{"expected_revision": int64(4)}), false, 5)
}

func TestL1TaskboardClosedArgumentsAtomic(t *testing.T) {
	a := newNotificationAuthority(t, false)
	cases := map[string]func(map[string]any){
		"missing_revision": func(m map[string]any) { delete(m, "expected_revision") }, "null_revision": func(m map[string]any) { m["expected_revision"] = nil }, "negative_revision": func(m map[string]any) { m["expected_revision"] = -1 }, "fraction_revision": func(m map[string]any) { m["expected_revision"] = 1.25 }, "overflow_revision": func(m map[string]any) { m["expected_revision"] = int64(9007199254740991) },
		"missing_project_choice": func(m map[string]any) { delete(m, "linear_project_id") }, "empty_project": func(m map[string]any) { m["linear_project_id"] = "" }, "wrong_project_type": func(m map[string]any) { m["linear_project_id"] = false },
		"empty_teams": func(m map[string]any) { m["linear_team_ids"] = []string{} }, "null_teams": func(m map[string]any) { m["linear_team_ids"] = nil }, "duplicate_teams": func(m map[string]any) { m["linear_team_ids"] = []string{l1TeamA, l1TeamA} }, "bad_team_type": func(m map[string]any) { m["linear_team_ids"] = []any{l1TeamA, 7} }, "noncanonical_team": func(m map[string]any) { m["linear_team_ids"] = []string{"AAAAAAAA-bbbb-cccc-dddd-eeeeeeeeeeee"} },
		"too_many_teams": func(m map[string]any) {
			ids := []string{}
			for i := 0; i < 21; i++ {
				ids = append(ids, fmt.Sprintf("%08x-aaaa-4aaa-8aaa-aaaaaaaaaaaa", i))
			}
			m["linear_team_ids"] = ids
			m["linear_default_team_id"] = ids[0]
		},
		"default_outside_set": func(m map[string]any) { m["linear_default_team_id"] = l1Project }, "null_default": func(m map[string]any) { m["linear_default_team_id"] = nil },
		"caller_provider": func(m map[string]any) { m["provider"] = "linear"; m["linear_project_id"] = l1Project }, "caller_provenance": func(m map[string]any) { m["linear_workspace_provenance"] = "linear-provider" },
		"caller_timestamp": func(m map[string]any) { m["updated_at"] = "2100-01-01T00:00:00Z" }, "caller_profile": func(m map[string]any) { m["profile"] = "private-local-profile" }, "credential_field": func(m map[string]any) { m["oauth_token"] = "SYNTHETIC_AUTH_VALUE_NEVER_ECHO" }, "peer_actor": func(m map[string]any) { m["target_session"] = "lead" },
	}
	for name, change := range cases {
		t.Run(name, func(t *testing.T) {
			args := l1MappingArgs(0)
			change(args)
			before := l1BindingSnapshot(t, a)
			_, err := a.execute(bus.Request{Operation: "taskboard_set", Workspace: "w", Project: "p", Session: "worker", Args: actor("worker", args)})
			if err == nil {
				t.Error("invalid closed mapping accepted")
			}
			if err != nil && strings.Contains(err.Error(), "SYNTHETIC_AUTH_VALUE_NEVER_ECHO") {
				t.Error("credential-like input reflected")
			}
			if before != l1BindingSnapshot(t, a) {
				t.Error("rejected mapping changed domain/audit/revision")
			}
		})
	}
}

func TestL1TaskboardScopeAndCAS(t *testing.T) {
	f := newFixture(t)
	f.setup("w", "p", "other")
	f.setup("other-workspace", "p")
	r := l1CheckBinding(t, f.call("taskboard_set", "w", "p", "", l1MappingArgs(0)), true, 1)
	for _, scope := range [][2]string{{"w", "other"}, {"other-workspace", "p"}} {
		m := l1CheckBinding(t, f.call("taskboard_get", scope[0], scope[1], "", nil), false, 0)
		if m["project_id"] == r["project_id"] {
			t.Fatal("mapping leaked across scope")
		}
	}
	const workers = 6
	results := make(chan error, workers)
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := f.store.Execute(context.Background(), f.request("taskboard_set", "w", "p", "", l1MappingArgs(1)))
			results <- err
		}()
	}
	wg.Wait()
	close(results)
	success := 0
	for err := range results {
		if err == nil {
			success++
		}
	}
	if success != 1 {
		t.Fatalf("same expected revision had %d successful replacements", success)
	}
	l1CheckBinding(t, f.call("taskboard_get", "w", "p", "", nil), true, 2)
	resumed := f.call("register_session", "w", "p", "worker", map[string]any{"instance": "worker-instance", "resume": true})
	next := stringField(t, resumed, "instance")
	f.reject("taskboard_get", "w", "p", "worker", actor("worker", nil))
	f.reject("taskboard_set", "w", "p", "worker", actor("worker", l1MappingArgs(2)))
	f.call("retire_session", "w", "p", "worker", map[string]any{"instance": next})
	f.call("taskboard_get", "w", "p", "worker", map[string]any{"instance": next})
	f.reject("taskboard_clear", "w", "p", "worker", map[string]any{"instance": next, "expected_revision": int64(2)})
}

func TestL1TaskboardPostgresRoleAndPrincipalFences(t *testing.T) {
	f := newPGFixture(t)
	f.setup("p", "other")
	for _, name := range []string{"owner", "lead"} {
		f.call(pgPrincipal(name), "taskboard_get", "p", "", nil)
	}
	f.call(pgPrincipal("lead"), "taskboard_set", "p", "", l1MappingArgs(0))
	for _, name := range []string{"alice", "reader"} {
		l1CheckBinding(t, f.call(pgPrincipal(name), "taskboard_get", "p", name, actor(name, nil)), true, 1)
		f.reject(pgPrincipal(name), "taskboard_set", "p", name, actor(name, l1MappingArgs(1)))
		f.reject(pgPrincipal(name), "taskboard_clear", "p", "", map[string]any{"expected_revision": int64(1)})
	}
	f.reject(pgOwner, "taskboard_get", "p", "alice", actor("alice", nil))
	f.reject("stranger@example.test", "taskboard_get", "p", "", nil)
	f.reject(pgPrincipal("lead"), "taskboard_set", "p", "alice", actor("alice", l1MappingArgs(1)))
	f.call(pgOwner, "taskboard_clear", "p", "owner", actor("owner", map[string]any{"expected_revision": int64(1)}))
}
