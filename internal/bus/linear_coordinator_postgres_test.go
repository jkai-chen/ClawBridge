package bus

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// On the hosted store, with the dispatcher on, the domain coordinator
// session of principal A, on A's device D, moves an issue in its lane. A
// session of principal B whose ID is placed in the domain ref with A's
// principal is refused.
func TestPostgresDomainCoordinatorMovesInItsLane(t *testing.T) {
	f := newPGIdentityFixture(t)
	f.s.SetIssueMoveProven(true)
	f.s.SetProjectStatusDomainProven(true)
	a, b := newID("a")+"@identity-test.example", newID("b")+"@identity-test.example"
	key := newKey(t)
	if _, err := f.join(a, f.invite(f.owner, a, "member")["code"].(string), key); err != nil {
		t.Fatal(err)
	}
	device := keyID(t, key)
	f.member(b)
	ownerDo := func(op string, args map[string]any) map[string]any {
		return f.must(f.owner, "sub-owner", Request{Operation: op, Workspace: f.workspace, Project: "p", Args: args}).Data.(map[string]any)
	}
	if _, err := f.as(f.owner, "sub-owner", Request{Operation: "taskboard_set_v2", Workspace: f.workspace, Project: "p", Args: taskboardArgsLanes(0)}); err != nil {
		t.Fatal(err)
	}
	// Binding of a project set, so enable names a default domain.
	set := taskboardArgsLanes(1)
	set["mode"], set["linear_project_ids"] = "project_set", []string{addProject}
	f.must(f.owner, "sub-owner", Request{Operation: "taskboard_set_v2", Workspace: f.workspace, Project: "p", Args: set})
	register := func(session string) Request {
		return Request{Operation: "register_session", Workspace: f.workspace, Project: "p", Session: session, Args: map[string]any{"instance": session + "-i"}}
	}
	if _, err := f.asDevice(a, device, register("coord-a")); err != nil {
		t.Fatal(err)
	}
	f.must(b, "sub-"+b, register("coord-b"))
	// Lane pins are set from a person's signed-in device; this test seeds them.
	pid := `(SELECT id FROM projects WHERE slug='p' AND workspace_id=$1)`
	for key, who := range map[string]string{"coord-a": a, "coord-b": b} {
		f.exec(`INSERT INTO lane_pins(project_id,kind,pin_key,principal,lane,created_by,created_at) VALUES(`+pid+`,'session',$2,$3,'both',$4,'2026-10-28T12:00:00Z')`, f.workspaceID(f.workspace), key, who, f.owner)
	}
	on := ownerDo("dispatcher_enable", map[string]any{"expected_revision": 0, "confirm": true})
	ownerDo("dispatcher_domain_edit", map[string]any{"expected_revision": on["revision"], "domain": defaultDomainID, "action": "coordinator", "target_session": "coord-a"})
	move := func(who string) error {
		session, request := "coord-a", func(r Request) error { _, err := f.asDevice(a, device, r); return err }
		if who == "b" {
			session, request = "coord-b", func(r Request) error { _, err := f.as(b, "sub-"+b, r); return err }
		}
		args := moveArgs("m-"+who, teamTwo, teamOne)
		args["instance"] = session + "-i"
		return request(Request{Operation: "issue_move_reserve", Workspace: f.workspace, Project: "p", Session: session, Args: args})
	}
	if err := move("a"); err != nil {
		t.Fatal("the domain coordinator on its device was refused:", err)
	}
	var by, lane string
	if err := f.s.db.QueryRow(`SELECT by_role,lane FROM linear_write_reservations WHERE operation_id='m-a' AND project_id=(SELECT id FROM projects WHERE slug='p' AND workspace_id=$1)`, f.workspaceID(f.workspace)).Scan(&by, &lane); err != nil || by != "coordinator" || lane != "both" {
		t.Fatal(by, lane, err)
	}
	// The same session sets the project's status when every team the server
	// read is in its lane; the lane bound is the claim's.
	status := func(op string, args map[string]any) error {
		args["instance"], args["linear_project"] = "coord-a-i", addProject
		_, err := f.s.ExecuteAsIdentity(WithAppStep(context.Background()), Identity{Principal: a, Issuer: DeviceIssuer, Subject: device}, Request{Operation: op, Workspace: f.workspace, Project: "p", Session: "coord-a", Args: args})
		return err
	}
	if err := status("project_status_begin", map[string]any{"set": true, "to_type": "started", "operation_id": "s-1"}); err != nil {
		t.Fatal("the domain coordinator's begin was refused:", err)
	}
	claim := func(op string, teams []string) map[string]any {
		return map[string]any{"to_type": "started", "operation_id": op, "from_type": "planned", "to_status_id": psStarted, "expected_status_id": psPlanned, "project_teams": teams}
	}
	if err := status("project_status_claim", claim("s-1", []string{teamOne, teamTwo})); err != nil {
		t.Fatal("the in-lane claim was refused:", err)
	}
	if err := status("project_status_claim", claim("s-2", []string{teamOne, unboundTeam})); err == nil || !strings.Contains(err.Error(), "outside_lane") {
		t.Fatal("the cross-lane claim was accepted:", err)
	}
	// B's session ID with A's principal in the domain ref.
	var raw, idB string
	if err := f.s.db.QueryRow(`SELECT domains FROM dispatcher_policies WHERE project_id=(SELECT id FROM projects WHERE slug='p' AND workspace_id=$1)`, f.workspaceID(f.workspace)).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	if err := f.s.db.QueryRow(`SELECT id FROM sessions WHERE name='coord-b' AND project_id=(SELECT id FROM projects WHERE slug='p' AND workspace_id=$1)`, f.workspaceID(f.workspace)).Scan(&idB); err != nil {
		t.Fatal(err)
	}
	var doc DispatcherDomains
	if err := json.Unmarshal([]byte(raw), &doc); err != nil {
		t.Fatal(err)
	}
	doc.Domains[0].CoordinatorSession = &DispatcherSessionRef{ID: idB, Name: "coord-b", Principal: a}
	out, _ := json.Marshal(doc)
	f.exec(`UPDATE dispatcher_policies SET domains=$1 WHERE project_id=(SELECT id FROM projects WHERE slug='p' AND workspace_id=$2)`, string(out), f.workspaceID(f.workspace))
	if err := move("b"); err == nil || !strings.Contains(err.Error(), "domain's coordinator session") {
		t.Fatal("a session of another principal was accepted:", err)
	}
}
