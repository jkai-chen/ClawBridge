package bus

import (
	"testing"
)

// On the hosted authority a member's session that is the designated
// coordinator reserves and settles a create; only the project owner's
// sessions are told; the acknowledgement is the owner's, from their own
// device.
func TestPostgresProjectCreates(t *testing.T) {
	f := newPGIdentityFixture(t)
	device := map[string]string{}
	people := map[string]string{}
	for _, role := range []string{"owner", "member"} {
		p := newID(role) + "@identity-test.example"
		code := f.invite(f.owner, p, role)["code"].(string)
		key := newKey(t)
		if _, err := f.join(p, code, key); err != nil {
			t.Fatal(err)
		}
		people[role], device[role] = p, keyID(t, key)
	}
	as := func(role string, r Request) (Result, error) {
		r.Workspace, r.Project = f.workspace, "p"
		return f.asDevice(people[role], device[role], r)
	}
	must := func(role string, r Request) map[string]any {
		t.Helper()
		out, err := as(role, r)
		if err != nil {
			t.Fatal(r.Operation, err)
		}
		m, _ := out.Data.(map[string]any)
		return m
	}
	must("owner", Request{Operation: "taskboard_set_v2", Args: taskboardArgsV2(0)})
	must("owner", Request{Operation: "project_kind_set", Args: kindSet(0, teamOne, "", "Engineering", "Review")})
	must("owner", Request{Operation: "project_kind_record", Args: kindRecord(1, pkEng, pkRev)})
	must("owner", Request{Operation: "register_session", Session: "o", Args: map[string]any{"instance": "o-i", "role": "worker"}})
	must("member", Request{Operation: "register_session", Session: "m", Args: map[string]any{"instance": "m-i", "role": "worker"}})
	must("owner", Request{Operation: "coordinator_set", Session: "o", Args: map[string]any{"instance": "o-i", "target_session": "m"}})
	access := must("member", Request{Operation: "taskboard_access", Session: "m", Args: map[string]any{"instance": "m-i", "ledger": 1, "coordinator_scope": 1}})
	if access["coordinator"] != true {
		t.Fatal(access)
	}
	reserve := map[string]any{"instance": "m-i", "operation_id": "op-1", "team_id": teamOne, "kind": "Review", "name_sha256": "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}
	if got := must("member", Request{Operation: "project_create_reserve", Session: "m", Args: reserve}); got["kind_label_id"] != pkRev {
		t.Fatal(got)
	}
	got := must("member", Request{Operation: "project_create_settle", Session: "m", Args: map[string]any{"instance": "m-i", "operation_id": "op-1", "outcome": "created", "linear_project_id": createdProject}})
	if asInt(got["owner_sessions_told"]) != 1 {
		t.Fatal("only the owner's session is told:", got)
	}
	var to string
	if err := f.s.db.QueryRow(`SELECT address FROM messages WHERE kind='project-created'`).Scan(&to); err != nil || to != "o" {
		t.Fatal(to, err)
	}
	must("member", Request{Operation: "project_create_reserve", Session: "m", Args: map[string]any{"instance": "m-i", "operation_id": "op-2", "team_id": teamOne, "kind": "Review", "name_sha256": reserve["name_sha256"]}})
	if _, err := as("member", Request{Operation: "project_create_ack", Args: map[string]any{"operation_id": "op-2"}}); !refusedWith(err, "owner") {
		t.Fatal("member ack:", err)
	}
	if _, err := f.as(f.owner, "sub-owner", Request{Operation: "project_create_ack", Workspace: f.workspace, Project: "p", Args: map[string]any{"operation_id": "op-2"}}); !refusedWith(err, "device") {
		t.Fatal("google ack:", err)
	}
	must("owner", Request{Operation: "project_create_ack", Args: map[string]any{"operation_id": "op-2"}})
	// A coordinator demoted to reader keeps its session, but reserves
	// nothing.
	must("owner", Request{Operation: "project_role_set", Args: map[string]any{"principal": people["member"], "role": "reader"}})
	if _, err := as("member", Request{Operation: "project_create_reserve", Session: "m", Args: map[string]any{"instance": "m-i", "operation_id": "op-3", "team_id": teamOne, "kind": "Review", "name_sha256": reserve["name_sha256"]}}); !refusedWith(err, "member permission") {
		t.Fatal("a reader reserved:", err)
	}
}
