package bus

import (
	"context"
	"reflect"
	"strings"
	"testing"
)

func (h *harness) cancelKindsSet(rev int, kinds any, via string) (map[string]any, error) {
	r, err := h.s.Execute(context.Background(), h.request("status_cancel_kinds_set", "", map[string]any{"expected_revision": rev, "kinds": kinds, "via": via}))
	m, _ := r.Data.(map[string]any)
	return m, err
}

// cancelKindsAs runs the op as a principal holding role, as the hosted
// authority resolves it: a local store makes everyone its owner.
func (h *harness) cancelKindsAs(role string, a map[string]any) (any, error) {
	h.t.Helper()
	ctx := context.Background()
	tx, err := h.s.db.BeginTx(ctx, nil)
	if err != nil {
		h.t.Fatal(err)
	}
	defer tx.Rollback()
	t := &transaction{Tx: tx}
	p, err := one(ctx, t, `SELECT id,workspace_id FROM projects WHERE slug='p'`)
	if err != nil {
		h.t.Fatal(err)
	}
	return h.s.statusCancelKindsOp(ctx, t, str(p, "workspace_id"), str(p, "id"), role, nil, Request{Operation: "status_cancel_kinds_set", Args: a})
}

// AGENT-CANCEL R2: the op stores the kinds in the stored spelling, leaves
// the rest of the policy alone, audits old -> new with via, tells the
// coordinator, and clears with an empty list.
func TestCancelKindsSetStoresKinds(t *testing.T) {
	h := newHarness(t)
	h.call("coordinator_set", "c", map[string]any{"target_session": "c"})
	h.link(0, []string{cpOne})
	h.call("project_kind_set", "", kindSet(0, "", "", "Engineering", "Research"))
	h.call("status_policy_set", "", map[string]any{"expected_revision": 0, "status_policy": sfPolicy(true)})
	before := h.storedPolicy()
	got, err := h.cancelKindsSet(1, []any{" engineering ", "ENGINEERING", "Research"}, "cli-tty")
	if err != nil || got["revision"] != int64(2) || !reflect.DeepEqual(got["agent_may_cancel_kinds"], []string{"Engineering", "Research"}) {
		t.Fatal(got, err)
	}
	after := h.storedPolicy()
	if !reflect.DeepEqual(after.AgentMayCancelKinds, []string{"Engineering", "Research"}) {
		t.Fatal(after.AgentMayCancelKinds)
	}
	after.AgentMayCancelKinds = nil
	if !reflect.DeepEqual(after, before) {
		t.Fatalf("the op changed the rest of the policy:\n%v\n%v", before, after)
	}
	var details string
	if err = h.s.db.QueryRow(`SELECT details FROM history WHERE entity='linear_status_policy' AND action='cancel_kinds_set'`).Scan(&details); err != nil || !strings.Contains(details, `"kinds_before":0`) || !strings.Contains(details, `"kinds_after":2`) || !strings.Contains(details, `"via":"cli-tty"`) || strings.Contains(details, "Engineering") {
		t.Fatal("audit:", details, err)
	}
	n := h.closePolicyNotices()
	if len(n) != 1 || !strings.Contains(n[0], "Agents may now cancel or mark Duplicate issues in projects of kind Engineering, Research (set by the project owner).") || strings.Contains(n[0], "@") || !strings.Contains(n[0], closePolicyLine) {
		t.Fatal("notice:", n)
	}
	var to string
	if err = h.s.db.QueryRow(`SELECT d.recipient_id FROM deliveries d JOIN messages m ON m.id=d.message_id WHERE m.kind='close-policy'`).Scan(&to); err != nil || to != h.sessionID("c") {
		t.Fatal("notice not delivered to the coordinator:", to, err)
	}
	// The TUI types the names too; an unchanged list tells nobody again.
	if _, err = h.cancelKindsSet(2, []string{"Research", "Engineering"}, "tui"); err != nil || len(h.closePolicyNotices()) != 1 {
		t.Fatal("tui:", err, h.closePolicyNotices())
	}
	// Clearing needs no typing.
	if got, err = h.cancelKindsSet(3, []any{}, "cli"); err != nil || got["agent_may_cancel_kinds"] != nil || h.storedPolicy().AgentMayCancelKinds != nil {
		t.Fatal("clear:", got, err)
	}
	if n = h.closePolicyNotices(); len(n) != 2 || !strings.Contains(n[1], "Agents no longer cancel issues in this project (set by the project owner).") {
		t.Fatal("clear notice:", n)
	}
}

// Every refusal, in the order the op checks them.
func TestCancelKindsSetRefusals(t *testing.T) {
	h := newHarness(t)
	h.link(0, []string{cpOne})
	if _, err := h.cancelKindsSet(0, []string{"Engineering"}, "cli-tty"); err == nil || !strings.Contains(err.Error(), "has none") {
		t.Fatal("no policy:", err)
	}
	h.call("status_policy_set", "", map[string]any{"expected_revision": 0, "status_policy": sfPolicy(true)})
	if _, err := h.cancelKindsSet(1, []string{"Engineering"}, "cli-tty"); err == nil || !strings.Contains(err.Error(), "set the project kinds first: clawbridge linear kind set") {
		t.Fatal("no kind setting:", err)
	}
	h.call("project_kind_set", "", kindSet(0, "", "", "Engineering"))
	if _, err := h.s.Execute(context.Background(), h.request("status_cancel_kinds_set", "a", map[string]any{"expected_revision": 1, "kinds": []string{"Engineering"}, "via": "cli-tty"})); err == nil || !strings.Contains(err.Error(), "a session can never set who may cancel issues; the project owner or a lead runs it without --session") {
		t.Fatal("a session:", err)
	}
	ok := map[string]any{"expected_revision": 1, "kinds": []string{"Engineering"}, "via": "cli-tty"}
	if _, err := h.cancelKindsAs("member", ok); err == nil || !strings.Contains(err.Error(), "owner or lead") {
		t.Fatal("a member:", err)
	}
	many := make([]string, 21)
	for i := range many {
		many[i] = "K" + strings.Repeat("x", i)
	}
	for name, c := range map[string]struct {
		rev   int
		kinds any
		via   string
		want  string
	}{
		"bad via":         {1, []string{}, "mcp", "via is cli, cli-tty or tui"},
		"no via":          {1, []string{}, "", "via is cli, cli-tty or tui"},
		"not a list":      {1, "Engineering", "cli-tty", "array of strings"},
		"untyped":         {1, []string{"Engineering"}, "cli", "confirmed at a terminal"},
		"too many":        {1, many, "cli-tty", "at most 20"},
		"empty name":      {1, []string{" "}, "cli-tty", "1 to 80 characters"},
		"stale":           {7, []string{"Engineering"}, "cli-tty", "current revision is 1"},
		"unknown kind":    {1, []string{"Engineering", "Ops"}, "cli-tty", `"Ops" is not a kind of this project (its kinds: Engineering)`},
		"untyped too":     {1, []string{"Ops"}, "cli", "confirmed at a terminal"},
		"no revision arg": {-1, []string{}, "cli", "requires expected_revision"},
	} {
		a := map[string]any{"kinds": c.kinds, "via": c.via}
		if c.rev >= 0 {
			a["expected_revision"] = c.rev
		}
		if _, err := h.s.Execute(context.Background(), h.request("status_cancel_kinds_set", "", a)); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Fatal(name, err)
		}
	}
	if p := h.storedPolicy(); p.AgentMayCancelKinds != nil {
		t.Fatal("a refusal stored kinds:", p.AgentMayCancelKinds)
	}
	// A lead may set it, as who closes.
	if _, err := h.cancelKindsAs("lead", ok); err != nil {
		t.Fatal("a lead:", err)
	}
}

// R2: status_policy_set takes the key, but a session never changes it, and
// adding a kind is typed at a terminal; names are spelled as the kind
// setting spells them, and a kept name no longer a kind may stay.
func TestStatusPolicySetGuardsWhoMayCancel(t *testing.T) {
	h := newHarness(t)
	h.link(0, []string{cpOne})
	h.call("project_kind_set", "", kindSet(0, "", "", "Engineering", "Research"))
	h.call("status_policy_set", "", map[string]any{"expected_revision": 0, "status_policy": sfPolicy(true)})
	set := func(session string, rev int, kinds []string, via string) error {
		policy := sfPolicy(true)
		if kinds != nil {
			policy["agent_may_cancel_kinds"] = kinds
		}
		a := map[string]any{"expected_revision": rev, "status_policy": policy}
		if via != "" {
			a["via"] = via
		}
		_, err := h.s.Execute(context.Background(), h.request("status_policy_set", session, a))
		return err
	}
	if err := set("a", 1, []string{"Engineering"}, "cli-tty"); err == nil || !strings.Contains(err.Error(), "session can never change who may cancel") {
		t.Fatal("a session loosened:", err)
	}
	for _, via := range []string{"", "cli"} {
		if err := set("", 1, []string{"Engineering"}, via); err == nil || !strings.Contains(err.Error(), "typing each kind's name (Engineering)") {
			t.Fatal("loosened untyped, via", via, err)
		}
	}
	if err := set("", 1, []string{"Ops"}, "cli-tty"); err == nil || !strings.Contains(err.Error(), "not a kind of this project") {
		t.Fatal("an unknown kind:", err)
	}
	if err := set("", 1, []string{"engineering"}, "cli-tty"); err != nil || !reflect.DeepEqual(h.storedPolicy().AgentMayCancelKinds, []string{"Engineering"}) {
		t.Fatal(err, h.storedPolicy().AgentMayCancelKinds)
	}
	// A session edits the rest with the list as stored, never changes it.
	if err := set("a", 2, []string{"ENGINEERING"}, ""); err != nil {
		t.Fatal("a session edited the rest:", err)
	}
	if err := set("a", 3, []string{}, ""); err == nil || !strings.Contains(err.Error(), "session can never change who may cancel") {
		t.Fatal("a session tightened:", err)
	}
	// The kind leaves the setting: the stored name may stay; tightening
	// needs no terminal.
	h.call("project_kind_set", "", kindSet(1, "", "", "Research"))
	if err := set("", 3, []string{"Engineering"}, ""); err != nil {
		t.Fatal("a kept stale name:", err)
	}
	if err := set("", 4, nil, ""); err != nil || h.storedPolicy().AgentMayCancelKinds != nil {
		t.Fatal("cleared:", err, h.storedPolicy().AgentMayCancelKinds)
	}
}

// R1: the field is parsed, deduped and capped, and read back.
func TestParseStatusPolicyCancelKinds(t *testing.T) {
	p, err := ParseStatusPolicy(map[string]any{"enabled": true, "agent_may_cancel_kinds": []any{" A ", "a", "B"}})
	if err != nil || !reflect.DeepEqual(p.AgentMayCancelKinds, []string{"A", "B"}) {
		t.Fatal(p.AgentMayCancelKinds, err)
	}
	if p, err = ParseStatusPolicy(map[string]any{"enabled": true, "agent_may_cancel_kinds": []any{}}); err != nil || p.AgentMayCancelKinds != nil {
		t.Fatal(p.AgentMayCancelKinds, err)
	}
	if _, err = ParseStatusPolicy(map[string]any{"enabled": true, "agent_may_cancel_kinds": []any{""}}); err == nil {
		t.Fatal("an empty name")
	}
	if _, err = ParseStatusPolicy(map[string]any{"enabled": true, "agent_may_cancel_kinds": "A"}); err == nil || !strings.Contains(err.Error(), `"agent_may_cancel_kinds": [kind names]`) {
		t.Fatal("not a list:", err)
	}
}

// R10: the overview's status_policy carries the list (empty when agents
// never cancel) and the listed kinds the kind setting no longer has.
func TestOverviewCarriesCancelKinds(t *testing.T) {
	h := newHarness(t)
	policyOf := func() map[string]any {
		return h.overview("")["linear"].(map[string]any)["status_policy"].(map[string]any)
	}
	h.link(0, []string{cpOne})
	h.call("status_policy_set", "", map[string]any{"expected_revision": 0, "status_policy": sfPolicy(true)})
	if p := policyOf(); !reflect.DeepEqual(p["agent_may_cancel_kinds"], []string{}) || p["cancel_kinds_unknown"] != nil || p["cancel_kinds_no_setting"] != nil {
		t.Fatal(p)
	}
	h.call("project_kind_set", "", kindSet(0, "", "", "Engineering", "Research"))
	if _, err := h.cancelKindsSet(1, []string{"Engineering", "Research"}, "cli-tty"); err != nil {
		t.Fatal(err)
	}
	if p := policyOf(); !reflect.DeepEqual(p["agent_may_cancel_kinds"], []string{"Engineering", "Research"}) || p["cancel_kinds_unknown"] != nil {
		t.Fatal(p)
	}
	h.call("project_kind_set", "", kindSet(1, "", "", "research"))
	if p := policyOf(); !reflect.DeepEqual(p["cancel_kinds_unknown"], []string{"Engineering"}) || p["cancel_kinds_no_setting"] != nil {
		t.Fatal(p)
	}
	// With no kind setting at all (as after a rollback), every name is.
	if _, err := h.s.db.Exec(`DELETE FROM project_kind_policy`); err != nil {
		t.Fatal(err)
	}
	if p := policyOf(); p["cancel_kinds_no_setting"] != true {
		t.Fatal(p)
	}
}
