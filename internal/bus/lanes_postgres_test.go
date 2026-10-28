package bus

import (
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
