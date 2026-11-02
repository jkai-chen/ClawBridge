package bus

import (
	"context"
	"strings"
	"testing"
)

// On the hosted authority only the project owner pins lanes, from their
// own device key, at the pins' revision; a session pin records its
// session's principal (X1), and that member's session gets the lane.
func TestPostgresLanePins(t *testing.T) {
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
	a := taskboardArgsLanes(0)
	if _, err := as("owner", Request{Operation: "taskboard_set_v2", Args: a}); err != nil {
		t.Fatal(err)
	}
	if _, err := as("member", Request{Operation: "register_session", Session: "m", Args: map[string]any{"instance": "m-i", "role": "worker"}}); err != nil {
		t.Fatal(err)
	}
	lane := func() (string, int) {
		t.Helper()
		out, err := as("member", Request{Operation: "taskboard_access", Session: "m", Args: map[string]any{"instance": "m-i", "ledger": 1, "lane_scope": 1}})
		if err != nil {
			t.Fatal(err)
		}
		access, err := ParseTaskboardAccess(out.Data)
		if err != nil {
			t.Fatal(err)
		}
		return access.Lane, len(access.LaneWriteTeamIDs)
	}
	if name, n := lane(); name != LaneReadOnly || n != 0 {
		t.Fatal("unpinned:", name, n)
	}
	pin := map[string]any{"expected_revision": 0, "kind": "session", "key": "m", "lane": "one", "confirm": true}
	if _, err := as("member", Request{Operation: "lane_pin_set", Args: pin}); !refusedWith(err, "owner") {
		t.Fatal("member:", err)
	}
	if _, err := f.as(f.owner, "sub-owner", Request{Operation: "lane_pin_set", Workspace: f.workspace, Project: "p", Args: pin}); !refusedWith(err, "device") {
		t.Fatal("google:", err)
	}
	out, err := as("owner", Request{Operation: "lane_pin_set", Args: pin})
	if err != nil {
		t.Fatal(err)
	}
	pins := out.Data.(map[string]any)["pins"].([]map[string]any)
	if len(pins) != 1 || pins[0]["principal"] != people["member"] {
		t.Fatal("the session pin's principal:", pins)
	}
	if _, err := as("owner", Request{Operation: "lane_pin_set", Args: pin}); err == nil || !strings.Contains(err.Error(), "current revision is 1") {
		t.Fatal("stale revision:", err)
	}
	if name, n := lane(); name != "one" || n != 1 {
		t.Fatal("pinned:", name, n)
	}
	got, err := as("member", Request{Operation: "lane_pins_get", Args: map[string]any{}})
	if err != nil || asInt(got.Data.(map[string]any)["revision"]) != 1 {
		t.Fatal(got, err)
	}
}

// On the hosted authority a binding is changed only by the project owner or
// a lead, without a session, from their own device key: a Google sign-in, a
// session and a member are each refused, and the narrow lanes operation
// follows the same rule.
func TestPostgresBindingChangesNeedThePersonsDevice(t *testing.T) {
	f := newPGIdentityFixture(t)
	device, people := map[string]string{}, map[string]string{}
	for _, role := range []string{"lead", "member"} {
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
	if _, err := as("lead", Request{Operation: "taskboard_set_v2", Args: taskboardArgsLanes(0)}); err != nil {
		t.Fatal("a lead on their device was refused:", err)
	}
	// The workspace owner signing in with Google, not a device, is refused.
	google := func(op string, a map[string]any) error {
		_, err := f.s.ExecuteAsIdentity(context.Background(), Identity{Principal: f.owner, Issuer: GoogleIssuer, Subject: "sub-owner"}, Request{Operation: op, Workspace: f.workspace, Project: "p", Args: a})
		return err
	}
	lanes := map[string]any{"expected_revision": 1, "unpinned": "one"}
	for op, a := range map[string]map[string]any{
		"taskboard_set_v2":    taskboardArgsLanes(1),
		"taskboard_lanes_set": lanes,
		"taskboard_clear_v2":  {"expected_revision": 1},
	} {
		if err := google(op, a); !refusedWith(err, "signed-in device") {
			t.Fatal(op, "from a Google sign-in:", err)
		}
		if _, err := as("member", Request{Operation: op, Args: a}); err == nil {
			t.Fatal(op, "from a member")
		}
	}
	// A session is refused even on the lead's device.
	if _, err := as("lead", Request{Operation: "register_session", Session: "s", Args: map[string]any{"instance": "s-i", "role": "worker"}}); err != nil {
		t.Fatal(err)
	}
	withSession := map[string]any{"instance": "s-i", "expected_revision": 1, "unpinned": "one"}
	if _, err := as("lead", Request{Operation: "taskboard_lanes_set", Session: "s", Args: withSession}); !refusedWith(err, "own terminal") {
		t.Fatal("a session set the lanes:", err)
	}
	out, err := as("lead", Request{Operation: "taskboard_lanes_set", Args: lanes})
	if err != nil {
		t.Fatal(err)
	}
	if b, err := ParseTaskboardBindingV2(out.Data); err != nil || b.Unpinned != "one" || b.Revision != 2 {
		t.Fatal(b, err)
	}
}
