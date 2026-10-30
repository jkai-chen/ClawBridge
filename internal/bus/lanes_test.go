package bus

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

const teamTwo = "22222222-2222-4222-8222-222222222222"

// taskboardArgsLanes binds teamOne and teamTwo with two lanes: "one"
// writes teamOne, "both" writes both.
func taskboardArgsLanes(rev int) map[string]any {
	a := taskboardArgsV2(rev)
	a["linear_team_ids"] = []string{teamOne, teamTwo}
	a["lanes"] = []any{
		map[string]any{"name": "one", "write_team_ids": []any{teamOne}, "default_team_id": teamOne},
		map[string]any{"name": "both", "write_team_ids": []any{teamTwo, teamOne}, "default_team_id": teamTwo},
	}
	return a
}

// Lanes go on the wire as version 3, with unpinned read_only by default,
// and read back the same; the wire carries all ten fields.
func TestLanesRoundTrip(t *testing.T) {
	h := newHarness(t)
	b, e := ParseTaskboardBindingV2(h.call("taskboard_set_v2", "a", taskboardArgsLanes(0)))
	if e != nil || b.MappingVersion != 3 || len(b.Lanes) != 2 || b.Unpinned != LaneReadOnly {
		t.Fatal(b, e)
	}
	// Sorted by name, each lane's teams sorted.
	if b.Lanes[0].Name != "both" || b.Lanes[0].DefaultTeamID != teamTwo || b.Lanes[0].WriteTeamIDs[0] != teamOne || b.Lanes[1].Name != "one" {
		t.Fatal(b.Lanes)
	}
	raw, _ := json.Marshal(h.call("taskboard_get_v2", "a", nil))
	var fields map[string]any
	json.Unmarshal(raw, &fields)
	if fields["mapping_version"] != float64(3) || len(fields["binding"].(map[string]any)) != 10 {
		t.Fatal(fields)
	}
	got, e := ParseTaskboardBindingV2(json.RawMessage(raw))
	if e != nil || len(got.Lanes) != 2 || got.Unpinned != LaneReadOnly {
		t.Fatal(got, e)
	}
	a := taskboardArgsLanes(1)
	a["unpinned"] = "one"
	b, e = ParseTaskboardBindingV2(h.call("taskboard_set_v2", "a", a))
	if e != nil || b.Unpinned != "one" {
		t.Fatal(b, e)
	}
	if _, ok := b.Lane("one"); !ok {
		t.Fatal("lane lookup")
	}
	if _, ok := b.Lane(LaneReadOnly); ok {
		t.Fatal("read_only is no lane")
	}
}

// Lanes are checked as strictly as the rest of the mapping.
func TestLanesStrictFields(t *testing.T) {
	h := newHarness(t)
	lane := func(name string, teams []any, def string) map[string]any {
		return map[string]any{"name": name, "write_team_ids": teams, "default_team_id": def}
	}
	bad := map[string]func(map[string]any){
		"not a write team": func(a map[string]any) { a["lanes"] = []any{lane("x", []any{teamSub}, teamSub)} },
		"default outside":  func(a map[string]any) { a["lanes"] = []any{lane("x", []any{teamOne}, teamTwo)} },
		"no teams":         func(a map[string]any) { a["lanes"] = []any{lane("x", []any{}, teamOne)} },
		"repeated team":    func(a map[string]any) { a["lanes"] = []any{lane("x", []any{teamOne, teamOne}, teamOne)} },
		"bad name":         func(a map[string]any) { a["lanes"] = []any{lane("X", []any{teamOne}, teamOne)} },
		"read_only name":   func(a map[string]any) { a["lanes"] = []any{lane(LaneReadOnly, []any{teamOne}, teamOne)} },
		"long name":        func(a map[string]any) { a["lanes"] = []any{lane("a"+strings.Repeat("b", 32), []any{teamOne}, teamOne)} },
		"duplicate": func(a map[string]any) {
			a["lanes"] = []any{lane("x", []any{teamOne}, teamOne), lane("x", []any{teamTwo}, teamTwo)}
		},
		"extra field": func(a map[string]any) {
			a["lanes"] = []any{map[string]any{"name": "x", "write_team_ids": []any{teamOne}, "default_team_id": teamOne, "more": 1}}
		},
		"not a list":           func(a map[string]any) { a["lanes"] = "x" },
		"unpinned unknown":     func(a map[string]any) { a["unpinned"] = "nope" },
		"unpinned not string":  func(a map[string]any) { a["unpinned"] = 1 },
		"unpinned, no lanes":   func(a map[string]any) { delete(a, "lanes"); a["unpinned"] = LaneReadOnly },
		"eleven lanes":         func(a map[string]any) { a["lanes"] = elevenLanes() },
		"lane item not object": func(a map[string]any) { a["lanes"] = []any{"x"} },
	}
	for name, change := range bad {
		a := taskboardArgsLanes(0)
		change(a)
		if _, e := h.s.Execute(context.Background(), h.request("taskboard_set_v2", "a", a)); e == nil {
			t.Fatal(name, "was accepted")
		}
	}
	// Ten lanes are the limit itself.
	a := taskboardArgsLanes(0)
	a["lanes"] = elevenLanes()[:10]
	h.call("taskboard_set_v2", "a", a)
	// A wire mapping without lanes' two fields is refused as version 3.
	raw, _ := json.Marshal(h.call("taskboard_get_v2", "a", nil))
	var m map[string]any
	json.Unmarshal(raw, &m)
	delete(m["binding"].(map[string]any), "unpinned")
	if _, e := ParseTaskboardBindingV2(m); e == nil {
		t.Fatal("a version 3 mapping without unpinned was read")
	}
	if e := ValidateLanes([]string{teamOne}, nil, LaneReadOnly); e == nil {
		t.Fatal("unpinned without lanes validated")
	}
	if e := ValidateLanes([]string{teamOne}, []Lane{{Name: "x", WriteTeamIDs: []string{teamOne}, DefaultTeamID: teamOne}}, ""); e != nil {
		t.Fatal(e)
	}
}

func elevenLanes() []any {
	out := []any{}
	for i := 0; i < 11; i++ {
		out = append(out, map[string]any{"name": "l" + string(rune('a'+i)), "write_team_ids": []any{teamOne}, "default_team_id": teamOne})
	}
	return out
}

// pin runs a confirmed pin change by the owner, sessionless.
func (h *harness) pin(op string, a map[string]any) map[string]any {
	h.t.Helper()
	a["confirm"] = true
	return h.call(op, "", a).(map[string]any)
}

func (h *harness) lane(session string) (string, []string, bool) {
	h.t.Helper()
	out := h.call("taskboard_access", session, map[string]any{"ledger": 1, "lane_scope": 1}).(map[string]any)
	access, e := ParseTaskboardAccess(out)
	if e != nil {
		h.t.Fatal(e)
	}
	return access.Lane, access.LaneWriteTeamIDs, access.LaneWriteTeamIDs != nil
}

// laneFrom is where the server found session's lane, asked for alongside
// the lane (lane_from: 1).
func (h *harness) laneFrom(session string) string {
	h.t.Helper()
	out := h.call("taskboard_access", session, map[string]any{"ledger": 1, "lane_scope": 1, "lane_from": 1}).(map[string]any)
	access, e := ParseTaskboardAccess(out)
	if e != nil {
		h.t.Fatal(e)
	}
	return access.LaneFrom
}

// A session's lane: its own pin, then a pin of the session it moved from
// under the same principal, then its principal's pin, then unpinned. A
// session pin binds the principal it was set for (X1).
func TestSessionLaneResolution(t *testing.T) {
	h := newHarness(t)
	if _, _, named := h.lane("a"); named {
		t.Fatal("an unlinked board named a lane")
	}
	h.call("taskboard_set_v2", "a", taskboardArgsV2(0))
	if _, _, named := h.lane("a"); named {
		t.Fatal("a binding without lanes named a lane")
	}
	if from := h.laneFrom("a"); from != "" {
		t.Fatal("a binding without lanes named a lane source:", from)
	}
	h.call("taskboard_set_v2", "a", taskboardArgsLanes(1))
	// Unpinned: read_only by default, no write team.
	if lane, teams, named := h.lane("a"); !named || lane != LaneReadOnly || len(teams) != 0 {
		t.Fatal(lane, teams, named)
	}
	// An older client asks for no lane and gets the old form.
	out := h.call("taskboard_access", "a", map[string]any{"ledger": 1}).(map[string]any)
	if _, present := out["lane"]; present {
		t.Fatal("a lane without lane_scope:", out)
	}
	if from := h.laneFrom("a"); from != LaneFromUnpinned {
		t.Fatal("unpinned source:", from)
	}
	// A v0.3.11 client asks for the lane but not its source, and gets no
	// lane_from.
	out = h.call("taskboard_access", "a", map[string]any{"ledger": 1, "lane_scope": 1}).(map[string]any)
	if _, present := out["lane_from"]; present || out["lane"] != LaneReadOnly {
		t.Fatal("lane_from without the ask:", out)
	}
	// lane_from alone names nothing.
	out = h.call("taskboard_access", "a", map[string]any{"ledger": 1, "lane_from": 1}).(map[string]any)
	if _, present := out["lane_from"]; present {
		t.Fatal("lane_from without lane_scope:", out)
	}
	owner := h.principal()
	// Sessionless: the principal's pin, then unpinned.
	if lane, teams, named := h.lane(""); !named || lane != LaneReadOnly || len(teams) != 0 {
		t.Fatal("sessionless unpinned:", lane, teams, named)
	}
	h.pin("lane_pin_set", map[string]any{"expected_revision": 0, "kind": "principal", "key": strings.ToUpper(owner), "lane": "both"})
	if lane, teams, _ := h.lane(""); lane != "both" || len(teams) != 2 {
		t.Fatal("sessionless principal pin:", lane, teams)
	}
	if lane, teams, _ := h.lane("a"); lane != "both" || len(teams) != 2 {
		t.Fatal("principal pin:", lane, teams)
	}
	if from, sessionless := h.laneFrom("a"), h.laneFrom(""); from != LaneFromPrincipalPin || sessionless != LaneFromPrincipalPin {
		t.Fatal("principal pin source:", from, sessionless)
	}
	h.pin("lane_pin_set", map[string]any{"expected_revision": 1, "kind": "session", "key": "a", "lane": "one"})
	if lane, teams, _ := h.lane("a"); lane != "one" || len(teams) != 1 || teams[0] != teamOne {
		t.Fatal("session pin first:", lane, teams)
	}
	if from := h.laneFrom("a"); from != LaneFromSessionPin {
		t.Fatal("session pin source:", from)
	}
	// X1: a pin set for another principal's session of the name doesn't
	// pass to this one.
	if _, e := h.s.db.Exec(`UPDATE lane_pins SET principal='other@example.com' WHERE kind='session'`); e != nil {
		t.Fatal(e)
	}
	if lane, _, _ := h.lane("a"); lane != "both" {
		t.Fatal("another principal's session pin applied:", lane)
	}
	if _, e := h.s.db.Exec(`UPDATE lane_pins SET principal=? WHERE kind='session'`, owner); e != nil {
		t.Fatal(e)
	}
	// A successor inherits the pin of the session it moved from, under the
	// same principal only.
	h.call("register_session", "d", map[string]any{"instance": "d-instance", "role": "worker"})
	h.call("retire_session", "a", map[string]any{"status": "moved", "successor": "d"})
	h.pin("lane_pin_clear", map[string]any{"expected_revision": 2, "kind": "principal", "key": owner})
	if lane, _, _ := h.lane("d"); lane != "one" {
		t.Fatal("successor:", lane)
	}
	if from := h.laneFrom("d"); from != LaneFromMovedSessionPin {
		t.Fatal("successor source:", from)
	}
	if _, e := h.s.db.Exec(`UPDATE sessions SET owner_principal='other@example.com' WHERE name='a'`); e != nil {
		t.Fatal(e)
	}
	if lane, _, _ := h.lane("d"); lane != LaneReadOnly {
		t.Fatal("a successor inherited across principals:", lane)
	}
	if from := h.laneFrom("d"); from != LaneFromUnpinned {
		t.Fatal("a successor's source across principals:", from)
	}
	// unpinned may be a lane; a pin to a lane the binding dropped leaves
	// the session read-only.
	a := taskboardArgsLanes(2)
	a["unpinned"] = "both"
	h.call("taskboard_set_v2", "b", a)
	if lane, _, _ := h.lane("b"); lane != "both" {
		t.Fatal("unpinned lane:", lane)
	}
	h.pin("lane_pin_set", map[string]any{"expected_revision": 3, "kind": "session", "key": "b", "lane": "one"})
	a = taskboardArgsLanes(3)
	a["lanes"] = a["lanes"].([]any)[1:]
	a["unpinned"] = "both"
	h.call("taskboard_set_v2", "b", a)
	if lane, teams, _ := h.lane("b"); lane != "one" || len(teams) != 0 {
		t.Fatal("a vanished lane:", lane, teams)
	}
	// The pin still names where the lane came from.
	if from := h.laneFrom("b"); from != LaneFromSessionPin {
		t.Fatal("a vanished lane's source:", from)
	}
}

// Pins are the project owner's, sessionless, at the pins' revision; a pin
// without confirm is a preview naming the principal; history keeps counts,
// never a name or a principal.
func TestLanePinsAreTheOwners(t *testing.T) {
	h := newHarness(t)
	set := map[string]any{"expected_revision": 0, "kind": "session", "key": "a", "lane": "one"}
	if e := h.errorOf("lane_pin_set", "", set); !strings.Contains(e, "binding") && !strings.Contains(e, "lane") {
		t.Fatal("pin without a binding:", e)
	}
	h.call("taskboard_set_v2", "a", taskboardArgsLanes(0))
	if e := h.errorOf("lane_pin_set", "a", map[string]any{"expected_revision": 0, "kind": "session", "key": "a", "lane": "one", "confirm": true}); !strings.Contains(e, "a session can't pin lanes") {
		t.Fatal(e)
	}
	preview := h.call("lane_pin_set", "", map[string]any{"expected_revision": 0, "kind": "session", "key": "a", "lane": "one"}).(map[string]any)
	if p := preview["preview"].(map[string]any); p["principal"] != h.principal() || preview["confirmation_required"] != true {
		t.Fatal(preview)
	}
	if pins := h.call("lane_pins_get", "b", nil).(map[string]any); pins["revision"] != int64(0) || len(pins["pins"].([]map[string]any)) != 0 {
		t.Fatal("a preview wrote:", pins)
	}
	for name, a := range map[string]map[string]any{
		"unknown lane":    {"expected_revision": 0, "kind": "session", "key": "a", "lane": "nope"},
		"unknown session": {"expected_revision": 0, "kind": "session", "key": "zz", "lane": "one"},
		"bad kind":        {"expected_revision": 0, "kind": "team", "key": "a", "lane": "one"},
		"bad principal":   {"expected_revision": 0, "kind": "principal", "key": "not a principal", "lane": "one"},
		"stale revision":  {"expected_revision": 1, "kind": "session", "key": "a", "lane": "one"},
		"no revision":     {"kind": "session", "key": "a", "lane": "one"},
	} {
		a["confirm"] = true
		if e := h.errorOf("lane_pin_set", "", a); e == "" {
			t.Fatal(name, "was accepted")
		}
	}
	out := h.pin("lane_pin_set", set)
	if out["revision"] != int64(1) || len(out["pins"].([]map[string]any)) != 1 {
		t.Fatal(out)
	}
	// Setting it again replaces it.
	set["expected_revision"], set["lane"] = 1, "both"
	out = h.pin("lane_pin_set", set)
	pins := out["pins"].([]map[string]any)
	if out["revision"] != int64(2) || len(pins) != 1 || pins[0]["lane"] != "both" || pins[0]["principal"] != h.principal() {
		t.Fatal(out)
	}
	unpin := map[string]any{"expected_revision": 2, "kind": "session", "key": "a"}
	if p := h.call("lane_pin_clear", "", unpin).(map[string]any); p["preview"].(map[string]any)["lane"] != "both" {
		t.Fatal(p)
	}
	h.pin("lane_pin_clear", unpin)
	if e := h.errorOf("lane_pin_clear", "", map[string]any{"expected_revision": 3, "kind": "session", "key": "a", "confirm": true}); !strings.Contains(e, "no such lane pin") {
		t.Fatal(e)
	}
	owner := h.principal()
	rows, e := h.s.db.Query(`SELECT action,details FROM history WHERE entity='lane_pins' ORDER BY sequence`)
	if e != nil {
		t.Fatal(e)
	}
	defer rows.Close()
	var actions []string
	for rows.Next() {
		var action, details string
		rows.Scan(&action, &details)
		actions = append(actions, action)
		if strings.Contains(details, owner) || strings.Contains(details, `"a"`) || !strings.Contains(details, `"pins":`) {
			t.Fatal("history details:", details)
		}
	}
	if strings.Join(actions, ",") != "lane_pin_set,lane_pin_set,lane_pin_clear" {
		t.Fatal(actions)
	}
}

// The access parser takes a lane only with its write teams.
func TestTaskboardAccessParserChecksTheLane(t *testing.T) {
	base := func() map[string]any {
		return map[string]any{"workspace_id": "w", "project_id": "p", "session_id": "s1", "session_name": "a", "can_write": true, "project_role": "owner"}
	}
	good := base()
	good["lane"], good["lane_write_team_ids"] = "one", []any{teamOne}
	if a, e := ParseTaskboardAccess(good); e != nil || a.Lane != "one" || len(a.LaneWriteTeamIDs) != 1 {
		t.Fatal(a, e)
	}
	empty := base()
	empty["lane"], empty["lane_write_team_ids"] = LaneReadOnly, []any{}
	if a, e := ParseTaskboardAccess(empty); e != nil || a.LaneWriteTeamIDs == nil || len(a.LaneWriteTeamIDs) != 0 {
		t.Fatal("read-only lane:", a, e)
	}
	if a, e := ParseTaskboardAccess(base()); e != nil || a.LaneWriteTeamIDs != nil {
		t.Fatal("no lane:", a, e)
	}
	// A lane's source is optional: a v0.3.11 server names none.
	if a, e := ParseTaskboardAccess(good); e != nil || a.LaneFrom != "" {
		t.Fatal("a v0.3.11 lane:", a, e)
	}
	for _, from := range []string{LaneFromSessionPin, LaneFromMovedSessionPin, LaneFromPrincipalPin, LaneFromUnpinned} {
		m := base()
		m["lane"], m["lane_write_team_ids"], m["lane_from"] = "one", []any{teamOne}, from
		a, e := ParseTaskboardAccess(m)
		if e != nil || a.LaneFrom != from {
			t.Fatal(from, a, e)
		}
		raw, _ := json.Marshal(a)
		if back, e := ParseTaskboardAccess(json.RawMessage(raw)); e != nil || back.LaneFrom != from {
			t.Fatal("round trip:", from, back, e)
		}
	}
	for name, change := range map[string]func(map[string]any){
		"lane alone":  func(m map[string]any) { m["lane"] = "one" },
		"teams alone": func(m map[string]any) { m["lane_write_team_ids"] = []any{teamOne} },
		"bad name":    func(m map[string]any) { m["lane"], m["lane_write_team_ids"] = "One", []any{teamOne} },
		"bad team":    func(m map[string]any) { m["lane"], m["lane_write_team_ids"] = "one", []any{"FIX"} },
		"not a list":  func(m map[string]any) { m["lane"], m["lane_write_team_ids"] = "one", teamOne },
		"extra field": func(m map[string]any) { m["lane"], m["lane_write_team_ids"], m["more"] = "one", []any{teamOne}, 1 },
		"bad source": func(m map[string]any) {
			m["lane"], m["lane_write_team_ids"], m["lane_from"] = "one", []any{teamOne}, "binding"
		},
		"source not a string": func(m map[string]any) { m["lane"], m["lane_write_team_ids"], m["lane_from"] = "one", []any{teamOne}, 1 },
		"source alone":        func(m map[string]any) { m["lane_from"] = LaneFromUnpinned },
	} {
		m := base()
		change(m)
		if _, e := ParseTaskboardAccess(m); e == nil {
			t.Fatal(name, "parsed")
		}
	}
	// Round trip.
	a, _ := ParseTaskboardAccess(good)
	raw, _ := json.Marshal(a)
	if back, e := ParseTaskboardAccess(json.RawMessage(raw)); e != nil || back.Lane != "one" {
		t.Fatal(back, e)
	}
}
