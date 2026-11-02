package bus

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"reflect"
	"strings"
	"sync"
	"testing"

	"clawbridge/internal/devicekey"
)

func withInstance(a map[string]any, instance string) map[string]any {
	a["instance"] = instance
	return a
}

// fromSession sends op as session "a" would: with its session and instance.
func (h *harness) fromSession(op string, a map[string]any) error {
	_, e := h.s.Execute(context.Background(), Request{Operation: op, Workspace: "w", Project: "p", Session: "a", Args: withInstance(a, "a-instance")})
	return e
}

func (h *harness) storedMapping() map[string]any {
	h.t.Helper()
	var raw string
	if e := h.s.db.QueryRow(`SELECT mapping FROM taskboard_bindings`).Scan(&raw); e != nil {
		h.t.Fatal(e)
	}
	var m map[string]any
	if e := json.Unmarshal([]byte(raw), &m); e != nil {
		h.t.Fatal(e)
	}
	return m
}

// Every operation that writes the binding is refused from a session, with
// the words that name the person's own terminal, and works without one.
func TestBindingChangesRefuseASession(t *testing.T) {
	h := newHarness(t)
	h.call("taskboard_set_v2", "", taskboardArgsLanes(0))
	steps := []struct {
		op   string
		args map[string]any
	}{
		{"taskboard_set_v2", taskboardArgsLanes(1)},
		{"taskboard_set", taskboardArgs(1)},
		{"taskboard_names_set", map[string]any{"expected_revision": 1, "linear_team_names": map[string]any{teamOne: "One"}}},
		{"taskboard_lanes_set", map[string]any{"expected_revision": 1, "unpinned": "one"}},
		{"taskboard_clear_v2", map[string]any{"expected_revision": 1}},
		{"taskboard_clear", map[string]any{"expected_revision": 1}},
	}
	for _, step := range steps {
		e := h.fromSession(step.op, step.args)
		if e == nil || !strings.Contains(e.Error(), BindingPersonOnly) {
			t.Fatalf("%s from a session: %v", step.op, e)
		}
	}
	if b, e := ParseTaskboardBindingV2(h.call("taskboard_get_v2", "", nil)); e != nil || b.Revision != 1 || len(b.Lanes) != 2 {
		t.Fatal("a refused change moved the binding:", b, e)
	}
	// Without a session each works; the lead's session is no exception.
	h.call("taskboard_names_set", "", map[string]any{"expected_revision": 1, "linear_team_names": map[string]any{teamOne: "One"}})
	h.call("taskboard_lanes_set", "", map[string]any{"expected_revision": 1, "unpinned": "one"})
	h.call("taskboard_set_v2", "", taskboardArgsLanes(2))
	h.call("taskboard_clear_v2", "", map[string]any{"expected_revision": 3})
}

// A new binding with lanes and no unpinned setting leaves unpinned
// sessions read-only.
func TestNewBindingWithLanesDefaultsToReadOnly(t *testing.T) {
	h := newHarness(t)
	a := taskboardArgsLanes(0)
	if _, ok := a["unpinned"]; ok {
		t.Fatal("the fixture names unpinned")
	}
	b, e := ParseTaskboardBindingV2(h.call("taskboard_set_v2", "", a))
	if e != nil || len(b.Lanes) != 2 || b.Unpinned != LaneReadOnly {
		t.Fatal(b, e)
	}
	if h.storedMapping()["unpinned"] != LaneReadOnly {
		t.Fatal("stored:", h.storedMapping())
	}
}

// taskboard_lanes_set changes the lanes and unpinned and nothing else, at
// the revision the caller read.
func TestLanesSetChangesOnlyLanes(t *testing.T) {
	h := newHarness(t)
	set := taskboardArgsLanes(0)
	set["linear_other_teams"] = map[string]any{teamSub: TeamAccessComment}
	set["linear_parent_team_id"] = teamOne
	set["linear_team_names"] = map[string]any{teamOne: "One", teamTwo: "Two"}
	h.call("taskboard_set_v2", "", set)
	before := h.storedMapping()
	var namesBefore string
	if e := h.s.db.QueryRow(`SELECT names FROM taskboard_bindings`).Scan(&namesBefore); e != nil {
		t.Fatal(e)
	}
	lane := func(name string, teams ...any) map[string]any {
		return map[string]any{"name": name, "write_team_ids": teams, "default_team_id": teams[0]}
	}
	out := h.call("taskboard_lanes_set", "", map[string]any{"expected_revision": 1, "lanes": []any{lane("solo", teamTwo)}, "unpinned": "solo"})
	b, e := ParseTaskboardBindingV2(out)
	if e != nil || b.Revision != 2 || len(b.Lanes) != 1 || b.Lanes[0].Name != "solo" || b.Unpinned != "solo" || b.MappingVersion != 3 {
		t.Fatal(b, e)
	}
	after := h.storedMapping()
	for k, v := range before {
		if k == "lanes" || k == "unpinned" {
			continue
		}
		if !reflect.DeepEqual(v, after[k]) {
			t.Fatalf("%s changed: %v -> %v", k, v, after[k])
		}
	}
	if len(after) != len(before) || after["unpinned"] != "solo" {
		t.Fatal(after)
	}
	var namesAfter string
	if e = h.s.db.QueryRow(`SELECT names FROM taskboard_bindings`).Scan(&namesAfter); e != nil || namesAfter != namesBefore {
		t.Fatal("names changed:", namesBefore, namesAfter, e)
	}
	// Lanes alone keep the unpinned setting while it still names a lane;
	// unpinned alone is checked against the stored lanes.
	b, e = ParseTaskboardBindingV2(h.call("taskboard_lanes_set", "", map[string]any{"expected_revision": 2, "lanes": []any{lane("solo", teamTwo), lane("duo", teamOne, teamTwo)}}))
	if e != nil || b.Unpinned != "solo" || len(b.Lanes) != 2 {
		t.Fatal(b, e)
	}
	b, e = ParseTaskboardBindingV2(h.call("taskboard_lanes_set", "", map[string]any{"expected_revision": 3, "unpinned": LaneReadOnly}))
	if e != nil || b.Unpinned != LaneReadOnly || len(b.Lanes) != 2 || b.Revision != 4 {
		t.Fatal(b, e)
	}
	// An empty list clears the lanes and with them the unpinned setting.
	b, e = ParseTaskboardBindingV2(h.call("taskboard_lanes_set", "", map[string]any{"expected_revision": 4, "lanes": []any{}}))
	if e != nil || len(b.Lanes) != 0 || b.Unpinned != "" || b.LinearOtherTeams[teamSub] != TeamAccessComment {
		t.Fatal(b, e)
	}
}

func TestLanesSetConflictAndValidation(t *testing.T) {
	h := newHarness(t)
	h.call("taskboard_set_v2", "", taskboardArgsLanes(0))
	good := []any{map[string]any{"name": "x", "write_team_ids": []any{teamOne}, "default_team_id": teamOne}}
	fail := func(want string, a map[string]any) {
		t.Helper()
		_, e := h.s.Execute(context.Background(), h.request("taskboard_lanes_set", "", a))
		if e == nil || !strings.Contains(e.Error(), want) {
			t.Fatalf("want %q, got %v", want, e)
		}
	}
	fail("read it again", map[string]any{"expected_revision": 0, "lanes": good})
	fail("lanes, unpinned or both", map[string]any{"expected_revision": 1})
	fail("expected_revision", map[string]any{"lanes": good})
	fail("write teams of the binding", map[string]any{"expected_revision": 1, "lanes": []any{map[string]any{"name": "x", "write_team_ids": []any{teamSub}, "default_team_id": teamSub}}})
	fail("default team", map[string]any{"expected_revision": 1, "lanes": []any{map[string]any{"name": "x", "write_team_ids": []any{teamOne}, "default_team_id": teamTwo}}})
	fail("lane name", map[string]any{"expected_revision": 1, "lanes": []any{map[string]any{"name": "read_only", "write_team_ids": []any{teamOne}, "default_team_id": teamOne}}})
	fail("unpinned", map[string]any{"expected_revision": 1, "unpinned": "nowhere"})
	fail("unpinned", map[string]any{"expected_revision": 1, "lanes": good, "unpinned": "one"})
	many := []any{}
	for _, n := range []string{"a", "b", "c", "d", "e", "f", "g", "h", "i", "j", "k"} {
		many = append(many, map[string]any{"name": n, "write_team_ids": []any{teamOne}, "default_team_id": teamOne})
	}
	fail("at most 10", map[string]any{"expected_revision": 1, "lanes": many})
	if b, e := ParseTaskboardBindingV2(h.call("taskboard_get_v2", "", nil)); e != nil || b.Revision != 1 || len(b.Lanes) != 2 {
		t.Fatal("a refused change moved the binding:", b, e)
	}
	// An unlinked binding has no lanes to set.
	h.call("taskboard_clear_v2", "", map[string]any{"expected_revision": 1})
	fail("link the task board first", map[string]any{"expected_revision": 2, "lanes": good})
}

// The history records counts only.
func TestLanesSetAuditsCountsOnly(t *testing.T) {
	h := newHarness(t)
	h.call("taskboard_set_v2", "", taskboardArgsLanes(0))
	h.call("taskboard_lanes_set", "", map[string]any{"expected_revision": 1, "unpinned": "one"})
	var details string
	if e := h.s.db.QueryRow(`SELECT details FROM history WHERE entity='taskboard_binding' AND action='lanes_set'`).Scan(&details); e != nil {
		t.Fatal(e)
	}
	if !strings.Contains(details, `"lanes":2`) || !strings.Contains(details, `"revision":2`) || strings.Contains(details, "one") || strings.Contains(details, teamOne) {
		t.Fatal(details)
	}
}

// binderDevices are the devices enrolled for binding changes, by store and
// principal.
var binderDevices sync.Map

// executeAs runs a hosted request for a principal the way the older tests
// did, except that a binding change goes as that person on a device of their
// own, without a session, as the CLI sends it.
func executeAs(ctx context.Context, s *Store, principal string, r Request) (Result, error) {
	if !bindingWriteOp(r.Operation) {
		return s.ExecuteAs(ctx, principal, r)
	}
	r.Session = ""
	args := map[string]any{}
	for k, v := range r.Args {
		if k != "instance" {
			args[k] = v
		}
	}
	r.Args = args
	type owner struct {
		s         *Store
		principal string
	}
	device, ok := binderDevices.Load(owner{s, principal})
	if !ok {
		pub, _, err := ed25519.GenerateKey(nil)
		if err != nil {
			return Result{}, err
		}
		google := Identity{Principal: principal, Issuer: GoogleIssuer, Subject: "sub-" + principal}
		if _, err = s.ExecuteAsIdentity(ctx, google, Request{Operation: "device_enroll", Workspace: r.Workspace, Args: map[string]any{"public_key": devicekey.EncodePublicKey(pub), "label": "binding", "key_store": "keyring"}}); err != nil {
			return Result{}, err
		}
		device = devicekey.ID(pub)
		binderDevices.Store(owner{s, principal}, device)
	}
	return s.ExecuteAsIdentity(ctx, Identity{Principal: principal, Issuer: DeviceIssuer, Subject: device.(string)}, r)
}
