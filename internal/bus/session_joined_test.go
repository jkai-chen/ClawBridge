package bus

import (
	"context"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"
)

// joined is each session-joined message: its body, sender and recipient
// names, oldest first.
type joined struct{ body, from, to string }

func (h *harness) joinNotices() []joined {
	h.t.Helper()
	rows, err := h.s.db.Query(`SELECT m.body,s.name,r.name FROM messages m JOIN sessions s ON s.id=m.sender_id JOIN deliveries d ON d.message_id=m.id JOIN sessions r ON r.id=d.recipient_id WHERE m.kind='session-joined' ORDER BY m.sequence,r.name`)
	if err != nil {
		h.t.Fatal(err)
	}
	defer rows.Close()
	var out []joined
	for rows.Next() {
		var j joined
		if err := rows.Scan(&j.body, &j.from, &j.to); err != nil {
			h.t.Fatal(err)
		}
		out = append(out, j)
	}
	return out
}

func (h *harness) exec(q string, args ...any) {
	h.t.Helper()
	if _, err := h.s.db.Exec(q, args...); err != nil {
		h.t.Fatal(err)
	}
}

// FR23: a new name tells the project's coordinator, in typed fields only;
// a name registered again, resumed, refused or retired tells no one, and
// nor does a registration with no coordinator.
func TestSessionJoinedNotifiesTheCoordinator(t *testing.T) {
	h := newHarness(t)
	if got := h.joinNotices(); len(got) != 0 {
		t.Fatal("notices with no coordinator:", got)
	}
	h.exec(`UPDATE projects SET coordinator_session=(SELECT id FROM sessions WHERE name='a') WHERE slug='p'`)
	h.call("register_session", "d", map[string]any{"instance": "d-instance", "role": "worker", "agent_kind": "codex"})
	got := h.joinNotices()
	want := "Session d joined: a member's codex agent, no lanes.\n" + sessionJoinedLine
	if len(got) != 1 || got[0].body != want || got[0].from != "d" || got[0].to != "a" {
		t.Fatalf("notices: %+v", got)
	}
	if p := h.principal(); strings.Contains(got[0].body, p) || strings.Contains(got[0].body, "@") {
		t.Fatal("the notice names the principal:", got[0].body)
	}
	// The coordinator reads it in its inbox.
	inbox := h.call("inbox", "a", nil).(map[string]any)
	found := false
	for _, m := range inbox["messages"].([]map[string]any) {
		found = found || m["kind"] == sessionJoinedKind
	}
	if !found {
		t.Fatal("the coordinator's inbox has no join notice:", inbox)
	}
	// Registered again by the same instance, resumed, or claimed by
	// another instance: nothing more.
	h.call("register_session", "d", map[string]any{"instance": "d-instance", "role": "worker", "agent_kind": "codex"})
	resumed := h.call("register_session", "d", map[string]any{"instance": "d-instance", "resume": true}).(map[string]any)
	h.fail("register_session", "d", map[string]any{"instance": "other-instance", "role": "worker"})
	// A retired name is refused, and tells no one.
	h.call("retire_session", "d", map[string]any{"instance": resumed["instance"], "status": "retired"})
	h.fail("register_session", "d", map[string]any{"instance": resumed["instance"], "role": "worker", "agent_kind": "codex"})
	if got := h.joinNotices(); len(got) != 1 {
		t.Fatalf("a known name sent a notice: %+v", got)
	}
	// The coordinator gone: nothing.
	h.call("retire_session", "a", map[string]any{"status": "retired"})
	h.call("register_session", "e", map[string]any{"instance": "e-instance", "role": "worker"})
	if got := h.joinNotices(); len(got) != 1 {
		t.Fatalf("a notice with the coordinator retired: %+v", got)
	}
}

// The notice names the new session's lane and where it came from, or says
// the binding has no lanes.
func TestSessionJoinedNamesTheLane(t *testing.T) {
	h := newHarness(t)
	h.exec(`UPDATE projects SET coordinator_session=(SELECT id FROM sessions WHERE name='a') WHERE slug='p'`)
	h.call("taskboard_set_v2", "a", taskboardArgsLanes(0))
	// A pin can only be set on a session that exists; one kept for a
	// name whose session is gone is the way a new name meets one.
	h.exec(`INSERT INTO lane_pins(project_id,kind,pin_key,principal,lane,created_by,created_at) SELECT id,'session','pinned',?,'one',?,'2026-10-28T12:00:00Z' FROM projects WHERE slug='p'`, h.principal(), h.principal())
	h.call("register_session", "pinned", map[string]any{"instance": "pinned-instance", "role": "worker", "agent_kind": "claude-code"})
	h.call("register_session", "loose", map[string]any{"instance": "loose-instance", "role": "worker"})
	h.pin("lane_pin_set", map[string]any{"expected_revision": 0, "kind": "principal", "key": h.principal(), "lane": "both"})
	h.call("register_session", "member", map[string]any{"instance": "member-instance", "role": "worker", "agent_kind": "human"})
	got := h.joinNotices()
	if len(got) != 3 {
		t.Fatalf("notices: %+v", got)
	}
	for i, want := range []string{
		"Session pinned joined: a member's claude-code agent, lane one (session_pin).",
		"Session loose joined: a member's other agent, lane " + LaneReadOnly + " (unpinned).",
		"Session member joined: a member's human agent, lane both (principal_pin).",
	} {
		if !strings.HasPrefix(got[i].body, want+"\n") {
			t.Fatalf("notice %d: %q, want %q", i, got[i].body, want)
		}
	}
}

// The notice is in the registration's transaction: a registration that
// fails after it leaves neither the session nor the message.
func TestSessionJoinedSameTransaction(t *testing.T) {
	h := newHarness(t)
	h.exec(`UPDATE projects SET coordinator_session=(SELECT id FROM sessions WHERE name='a') WHERE slug='p'`)
	h.exec(`CREATE TRIGGER fail_join AFTER INSERT ON deliveries WHEN (SELECT kind FROM messages WHERE id=NEW.message_id)='session-joined' BEGIN SELECT RAISE(ABORT,'forced failure'); END`)
	h.fail("register_session", "d", map[string]any{"instance": "d-instance", "role": "worker"})
	var sessions, messages int
	if err := h.s.db.QueryRow(`SELECT (SELECT COUNT(*) FROM sessions WHERE name='d'),(SELECT COUNT(*) FROM messages WHERE kind='session-joined')`).Scan(&sessions, &messages); err != nil {
		t.Fatal(err)
	}
	if sessions != 0 || messages != 0 {
		t.Fatalf("a failed registration left %d sessions and %d notices", sessions, messages)
	}
	h.exec(`DROP TRIGGER fail_join`)
	h.call("register_session", "d", map[string]any{"instance": "d-instance", "role": "worker"})
	if got := h.joinNotices(); len(got) != 1 {
		t.Fatalf("notices: %+v", got)
	}
}

// At most five join notices per member an hour: the sixth registration
// succeeds silently, with a history row; another member's still notifies,
// and so does the first member's an hour on.
func TestSessionJoinedRateLimit(t *testing.T) {
	h := newHarness(t)
	now := time.Now().UTC()
	hostClock = func() time.Time { return now }
	t.Cleanup(func() { hostClock = time.Now })
	h.exec(`UPDATE projects SET coordinator_session=(SELECT id FROM sessions WHERE name='a') WHERE slug='p'`)
	for i := 1; i <= 6; i++ {
		name := "burst-" + string(rune('0'+i))
		h.call("register_session", name, map[string]any{"instance": name + "-instance", "role": "worker"})
		now = now.Add(time.Minute)
	}
	if got := h.joinNotices(); len(got) != 5 {
		t.Fatalf("%d notices for six registrations", len(got))
	}
	var held int
	if err := h.s.db.QueryRow(`SELECT COUNT(*) FROM history WHERE action='join_notice_suppressed' AND entity_key=(SELECT id FROM sessions WHERE name='burst-6')`).Scan(&held); err != nil || held != 1 {
		t.Fatal("suppressed rows:", held, err)
	}
	// A local store has one member; the burst made another's, its
	// next registration is the first of its hour.
	h.exec(`UPDATE sessions SET owner_principal='principal-two' WHERE name LIKE 'burst-%'`)
	h.call("register_session", "theirs", map[string]any{"instance": "theirs-instance", "role": "worker"})
	if got := h.joinNotices(); len(got) != 6 || !strings.HasPrefix(got[5].body, "Session theirs joined") {
		t.Fatalf("another member's registration: %+v", got)
	}
	h.exec(`UPDATE sessions SET owner_principal=? WHERE name LIKE 'burst-%'`, h.principal())
	now = now.Add(time.Hour)
	h.call("register_session", "later", map[string]any{"instance": "later-instance", "role": "worker"})
	if got := h.joinNotices(); len(got) != 7 || !strings.HasPrefix(got[6].body, "Session later joined") {
		t.Fatalf("an hour on: %+v", got)
	}
}

// With the dispatcher as coordinator the notice goes to coordinatorTargets'
// sessions, never to the session that joined, which may be one of them.
func TestSessionJoinedDispatcherTargets(t *testing.T) {
	h := dispatcherHarness(t)
	var pid string
	if err := h.s.db.QueryRow(`SELECT id FROM projects WHERE slug='p'`).Scan(&pid); err != nil {
		t.Fatal(err)
	}
	h.dispatcherSessions(pid)
	h.exec(`UPDATE projects SET coordinator_session=(SELECT id FROM sessions WHERE name='clawbridge-dispatcher') WHERE id=?`, pid)
	h.call("register_session", "d", map[string]any{"instance": "d-instance", "role": "worker"})
	// The fallback: every session of the owner but d itself.
	ctx := context.Background()
	tx, err := h.s.beginIntake(ctx)
	if err != nil {
		t.Fatal(err)
	}
	targets, err := h.s.coordinatorTargets(ctx, tx, pid, "", nil)
	tx.Rollback()
	if err != nil {
		t.Fatal(err)
	}
	var want, got []string
	for _, r := range targets {
		if str(r, "name") != "d" {
			want = append(want, str(r, "name"))
		}
	}
	for _, j := range h.joinNotices() {
		got = append(got, j.to)
	}
	sort.Strings(want)
	if len(targets) != 4 || !reflect.DeepEqual(got, want) || !reflect.DeepEqual(want, []string{"a", "b", "c"}) {
		t.Fatalf("notified %v, want %v (targets %v)", got, want, targets)
	}
	// Only the joining session is fresh: it is the one target, and is
	// not told of itself.
	h.exec(`UPDATE sessions SET last_seen='2000-01-01T00:00:00Z' WHERE project_id=? AND name IN ('a','b','c','d')`, pid)
	h.call("register_session", "e", map[string]any{"instance": "e-instance", "role": "worker"})
	if got := h.joinNotices(); len(got) != 3 {
		t.Fatalf("the joining session was told of itself: %+v", got)
	}
}

// project_overview lists the newest three sessions registered in the last
// day, newest first, with no principal; none when there are none.
func TestOverviewNewestJoins(t *testing.T) {
	h := newHarness(t)
	now := time.Now().UTC()
	hostClock = func() time.Time { return now }
	t.Cleanup(func() { hostClock = time.Now })
	h.exec(`UPDATE sessions SET created_at='2000-01-01T00:00:00.000000000Z'`)
	if joins := h.overview("a")["newest_joins"].([]map[string]any); len(joins) != 0 {
		t.Fatal("joins with none in the last day:", joins)
	}
	for _, name := range []string{"j1", "j2", "j3", "j4"} {
		now = now.Add(time.Minute)
		h.call("register_session", name, map[string]any{"instance": name + "-instance", "role": "worker", "agent_kind": "gemini-cli"})
	}
	joins := h.overview("a")["newest_joins"].([]map[string]any)
	var names []string
	for _, j := range joins {
		names = append(names, str(j, "name"))
		if j["display_name"] != sessionJoinedAnon || j["agent_kind"] != "gemini-cli" || str(j, "created_at") == "" || len(j) != 4 {
			t.Fatal(j)
		}
	}
	if !reflect.DeepEqual(names, []string{"j4", "j3", "j2"}) {
		t.Fatal(names)
	}
}
