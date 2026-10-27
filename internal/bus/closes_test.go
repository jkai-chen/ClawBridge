package bus

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

const (
	cpOne   = "3333aaaa-3333-4333-8333-333333333331"
	cpTwo   = "3333aaaa-3333-4333-8333-333333333332"
	cpThree = "3333aaaa-3333-4333-8333-333333333333"
)

// link binds the project to Linear projects, as clawbridge linear link does.
func (h *harness) link(rev int, projects []string, pending ...string) {
	h.t.Helper()
	a := map[string]any{"expected_revision": rev, "mode": "project_set", "linear_team_ids": []string{sfTeam}, "linear_default_team_id": sfTeam, "linear_project_ids": projects}
	if len(pending) > 0 {
		a["closes_pending"] = pending
	}
	h.call("taskboard_set_v2", "", a)
}

func (h *harness) closesSet(rev int, project, closes, via string) (map[string]any, error) {
	r, err := h.s.Execute(context.Background(), h.request("status_closes_set", "", map[string]any{"expected_revision": rev, "linear_project": project, "closes": closes, "via": via}))
	m, _ := r.Data.(map[string]any)
	return m, err
}

func (h *harness) closePolicyNotices() []string {
	h.t.Helper()
	rows, err := h.s.db.Query(`SELECT body FROM messages WHERE kind='close-policy' ORDER BY created_at, id`)
	if err != nil {
		h.t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var body string
		_ = rows.Scan(&body)
		out = append(out, body)
	}
	return out
}

func (h *harness) storedPolicy() StatusPolicy {
	h.t.Helper()
	var raw string
	if err := h.s.db.QueryRow(`SELECT policy FROM linear_status_policy`).Scan(&raw); err != nil {
		h.t.Fatal(err)
	}
	var p StatusPolicy
	_ = json.Unmarshal([]byte(raw), &p)
	return p
}

// A3 (§10.1 d): closes set changes one key at the policy's revision,
// deletes it on human, drops unbound keys, audits with via, and leaves the
// rest of the policy alone.
func TestClosesSetChangesOneProject(t *testing.T) {
	h := newHarness(t)
	h.link(0, []string{cpOne, cpTwo})
	if _, err := h.closesSet(0, cpOne, "agent", "cli-tty"); err == nil || !strings.Contains(err.Error(), "has none") {
		t.Fatal("closes set with no policy:", err)
	}
	policy := sfPolicy(true)
	policy["claim_from_triage"] = true
	policy["closes"] = map[string]any{cpThree: "agent"} // not bound
	h.call("status_policy_set", "", map[string]any{"expected_revision": 0, "status_policy": policy, "via": "cli-tty"})
	before := h.storedPolicy()
	got, err := h.closesSet(1, cpOne, "agent", "cli-tty")
	if err != nil || got["revision"] != int64(2) {
		t.Fatal(got, err)
	}
	after := h.storedPolicy()
	if len(after.Closes) != 1 || after.Closes[cpOne] != "agent" {
		t.Fatal("closes after set:", after.Closes)
	}
	after.Closes, before.Closes = nil, nil
	a, _ := json.Marshal(after)
	b, _ := json.Marshal(before)
	if string(a) != string(b) {
		t.Fatalf("closes set changed the rest of the policy:\n%s\n%s", b, a)
	}
	// Human deletes the key rather than storing it.
	if _, err = h.closesSet(2, cpOne, "human", "cli"); err != nil {
		t.Fatal(err)
	}
	if p := h.storedPolicy(); p.Closes != nil {
		t.Fatal("human was stored:", p.Closes)
	}
	var details string
	if err = h.s.db.QueryRow(`SELECT details FROM history WHERE entity='linear_status_policy' AND action='closes_set' AND details LIKE '%"closes":"agent"%'`).Scan(&details); err != nil || !strings.Contains(details, `"via":"cli-tty"`) || !strings.Contains(details, cpOne) {
		t.Fatal("audit", details, err)
	}
	for name, c := range map[string]struct {
		rev                  int
		project, closes, via string
		want                 string
	}{
		"stale":           {2, cpTwo, "agent", "cli-tty", "revision"},
		"unbound":         {3, cpThree, "agent", "cli-tty", "not in this project's board"},
		"agent untyped":   {3, cpTwo, "agent", "cli", "typing the project's name"},
		"bad value":       {3, cpTwo, "bot", "cli-tty", "agent or human"},
		"bad via":         {3, cpTwo, "human", "mcp", "via"},
		"upper-case uuid": {3, strings.ToUpper(cpTwo), "human", "cli", ""},
	} {
		_, err := h.closesSet(c.rev, c.project, c.closes, c.via)
		if name == "upper-case uuid" {
			// Canonicalised, as every Linear UUID the bus takes.
			if err != nil {
				t.Fatal(name, err)
			}
			continue
		}
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Fatal(name, err)
		}
	}
	// The TUI's link review has the person type the name too.
	var rev int
	if err = h.s.db.QueryRow(`SELECT revision FROM linear_status_policy`).Scan(&rev); err != nil {
		t.Fatal(err)
	}
	if _, err = h.closesSet(rev, cpTwo, "agent", "tui"); err != nil {
		t.Fatal("tui:", err)
	}
	if err = h.s.db.QueryRow(`SELECT details FROM history WHERE entity='linear_status_policy' AND action='closes_set' AND details LIKE '%"via":"tui"%'`).Scan(&details); err != nil || !strings.Contains(details, cpTwo) {
		t.Fatal("tui audit", details, err)
	}
}

// A session never sets who closes, owner or not; a team board has no
// per-project rule.
func TestClosesSetRefusesSessionsAndTeamBoards(t *testing.T) {
	h := newHarness(t)
	h.link(0, []string{cpOne})
	h.call("status_policy_set", "", map[string]any{"expected_revision": 0, "status_policy": sfPolicy(true)})
	if _, err := h.s.Execute(context.Background(), h.request("status_closes_set", "a", map[string]any{"expected_revision": 1, "linear_project": cpOne, "closes": "agent", "via": "cli-tty"})); err == nil || !strings.Contains(err.Error(), "session can never") {
		t.Fatal("a session set who closes:", err)
	}
	h.call("taskboard_set_v2", "", map[string]any{"expected_revision": 1, "mode": "team_board", "linear_team_ids": []string{sfTeam}, "linear_default_team_id": sfTeam, "linear_project_ids": []string{}})
	if _, err := h.closesSet(1, cpOne, "human", "cli"); err == nil || !strings.Contains(err.Error(), "team board") {
		t.Fatal("closes set on a team board:", err)
	}
}

// §10.1 c: one notice per project that joins unlisted under an enabled
// policy, routed to the coordinator; none for a listed or pending project
// or without an enabled policy; again after a remove and add.
func TestClosePolicyNoticeForAnUnlistedProject(t *testing.T) {
	h := newHarness(t)
	h.call("coordinator_set", "c", map[string]any{"target_session": "c"})
	h.link(0, []string{cpOne})
	if n := h.closePolicyNotices(); len(n) != 0 {
		t.Fatal("a notice with no policy:", n)
	}
	policy := sfPolicy(false)
	policy["closes"] = map[string]any{cpTwo: "agent"}
	h.call("status_policy_set", "", map[string]any{"expected_revision": 0, "status_policy": policy, "via": "cli-tty"})
	h.link(1, []string{cpOne, cpThree})
	if n := h.closePolicyNotices(); len(n) != 0 {
		t.Fatal("a notice under a disabled policy:", n)
	}
	policy["enabled"] = true
	h.call("status_policy_set", "", map[string]any{"expected_revision": 1, "status_policy": policy})
	// cpThree was already bound: nothing to say. cpTwo joins with an agent
	// key set while it wasn't bound (R89-L3): said once.
	h.link(2, []string{cpOne, cpTwo, cpThree})
	n := h.closePolicyNotices()
	if len(n) != 1 || !strings.Contains(n[0], "Linear project "+cpTwo+" joined") || !strings.Contains(n[0], "already says agents close") || !strings.Contains(n[0], "--human") {
		t.Fatal("a stale agent key joined unsaid, or an already bound project noticed:", n)
	}
	h.link(3, []string{cpOne, cpTwo})
	h.link(4, []string{cpOne, cpTwo, cpThree})
	n = h.closePolicyNotices()[1:]
	if len(n) != 1 || !strings.Contains(n[0], "Linear project "+cpThree+" joined") || !strings.Contains(n[0], "closed by people (the default)") || !strings.Contains(n[0], closePolicyLine) || strings.Contains(n[0], "@") {
		t.Fatal("notice after remove and add:", n)
	}
	var to string
	if err := h.s.db.QueryRow(`SELECT d.recipient_id FROM deliveries d JOIN messages m ON m.id=d.message_id WHERE m.kind='close-policy' AND m.body LIKE '%'||?||' joined%'`, cpThree).Scan(&to); err != nil || to != h.sessionID("c") {
		t.Fatal("notice not delivered to the coordinator:", to, err)
	}
	// A project the same link is about to set is skipped, and audited.
	h.link(5, []string{cpOne, cpTwo})
	h.link(6, []string{cpOne, cpTwo, cpThree}, cpThree)
	if n := h.closePolicyNotices(); len(n) != 2 {
		t.Fatal("closes_pending ignored:", n)
	}
	var details string
	if err := h.s.db.QueryRow(`SELECT details FROM history WHERE entity='taskboard' AND details LIKE '%"revision":7%'`).Scan(&details); err != nil || !strings.Contains(details, `"closes_pending":["`+cpThree+`"]`) {
		t.Fatal("binding audit lacks closes_pending:", details, err)
	}
	if _, err := h.s.Execute(context.Background(), h.request("taskboard_set_v2", "", map[string]any{"expected_revision": 7, "mode": "project_set", "linear_team_ids": []string{sfTeam}, "linear_default_team_id": sfTeam, "linear_project_ids": []string{cpOne}, "closes_pending": []string{cpTwo}})); err == nil {
		t.Fatal("closes_pending outside the link accepted")
	}
}

// R89-H1: the older status_policy_set never lets a session change who
// closes, even the owner's own session; it still edits the rest.
// R89-M1: whatever writes it, a key turned to agent needs via cli-tty;
// turning one back to human needs nothing.
func TestStatusPolicySetGuardsWhoCloses(t *testing.T) {
	h := newHarness(t)
	h.link(0, []string{cpOne, cpTwo})
	h.call("status_policy_set", "", map[string]any{"expected_revision": 0, "status_policy": sfPolicy(true)})
	loosen := sfPolicy(true)
	loosen["closes"] = map[string]any{cpOne: "agent"}
	set := func(session string, rev int, policy map[string]any, via string) error {
		a := map[string]any{"expected_revision": rev, "status_policy": policy}
		if via != "" {
			a["via"] = via
		}
		_, err := h.s.Execute(context.Background(), h.request("status_policy_set", session, a))
		return err
	}
	if err := set("a", 1, loosen, "cli-tty"); err == nil || !strings.Contains(err.Error(), "session can never change who closes") {
		t.Fatal("the owner's session loosened closes:", err)
	}
	for _, via := range []string{"", "cli"} {
		if err := set("", 1, loosen, via); err == nil || !strings.Contains(err.Error(), "confirmed at a terminal") || !strings.Contains(err.Error(), cpOne) {
			t.Fatal("loosened without a terminal, via", via, err)
		}
	}
	if err := set("", 1, loosen, "cli-tty"); err != nil {
		t.Fatal(err)
	}
	// The session may still change the rest, with closes as stored.
	edit := sfPolicy(false)
	edit["closes"] = map[string]any{cpOne: "agent"}
	if err := set("a", 2, edit, ""); err != nil {
		t.Fatal("a session edited the rest:", err)
	}
	if err := set("a", 3, sfPolicy(false), ""); err == nil || !strings.Contains(err.Error(), "session can never change who closes") {
		t.Fatal("the owner's session tightened closes:", err)
	}
	if err := set("", 3, sfPolicy(false), ""); err != nil {
		t.Fatal("tightening asked for a terminal:", err)
	}
}
