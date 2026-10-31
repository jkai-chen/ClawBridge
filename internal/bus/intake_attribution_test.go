package bus

import (
	"context"
	"strings"
	"testing"
	"time"
)

const (
	needsAgentID = "00000000-0000-4000-8000-0000000000c1"
	otherLabelID = "00000000-0000-4000-8000-0000000000c2"
	commentID    = "00000000-0000-4000-8000-0000000000d1"
)

// attributionHarness is a local store whose project p is bound to a board,
// with trustedUser on the trusted list linked to the workspace owner.
type attributionHarness struct {
	*harness
	pid, owner string
}

func newAttributionHarness(t *testing.T) *attributionHarness {
	t.Helper()
	h := &attributionHarness{harness: newHarness(t)}
	h.owner = h.principal()
	if err := h.s.db.QueryRow(`SELECT id FROM projects WHERE slug='p'`).Scan(&h.pid); err != nil {
		t.Fatal(err)
	}
	if _, err := h.s.db.Exec(`INSERT INTO taskboard_bindings(project_id,revision,mapping,updated_at) VALUES(?,1,'{}','2026-01-01T00:00:00Z')`, h.pid); err != nil {
		t.Fatal(err)
	}
	h.call("intake_trust_add", "", map[string]any{"linear_user_id": trustedUser, "principal": h.owner})
	return h
}

// device is the owner on their own signed-in device.
func (h *attributionHarness) device() Identity {
	return Identity{Principal: h.owner, Issuer: DeviceIssuer, Subject: "dev_1"}
}

// write records an app write that added needs-agent to intakeIssue.
func (h *attributionHarness) write(w AppWrite) {
	h.t.Helper()
	w.Workspace = "w"
	if w.Project == "" {
		w.Project = "p"
	}
	if w.Issue == "" {
		w.Issue = intakeIssue
	}
	if w.Labels == nil && w.Comment == "" {
		w.Labels = []string{needsAgentID}
	}
	if w.Identity.Issuer == "" {
		w.Identity = h.device()
	}
	if err := h.s.RecordAppWrite(context.Background(), w); err != nil {
		h.t.Fatal(err)
	}
}

// credit reads the needs-agent writes on intakeIssue from a minute ago to
// a minute ahead.
func (h *attributionHarness) credit(kind, target string) AttributionCredit {
	h.t.Helper()
	now := time.Now()
	c, err := h.s.IntakeAttribution(context.Background(), h.pid, strings.ToUpper(intakeIssue), kind, target, now.Add(-time.Minute), now.Add(time.Minute))
	if err != nil {
		h.t.Fatal(err)
	}
	return c
}

func (h *attributionHarness) rows() int {
	h.t.Helper()
	var n int
	if err := h.s.db.QueryRow(`SELECT COUNT(*) FROM app_write_attributions`).Scan(&n); err != nil {
		h.t.Fatal(err)
	}
	return n
}

// The owner's own sessionless write from their device is credited to the
// Linear user their principal is linked to.
func TestIntakeAttributionCreditsTheOwnersOwnWrite(t *testing.T) {
	h := newAttributionHarness(t)
	h.write(AppWrite{AgentMarker: "", OperationID: "op-1"})
	if c := h.credit("label", strings.ToUpper(needsAgentID)); c.Result != "credited" || c.Actor != trustedUser || c.Principal != h.owner {
		t.Fatal(c)
	}
	if c := h.credit("label", otherLabelID); c.Result != "" {
		t.Fatal("another label's window was credited:", c)
	}
	h.write(AppWrite{Comment: strings.ToUpper(commentID), Labels: []string{}})
	if c := h.credit("comment", commentID); c.Result != "credited" || c.Actor != trustedUser {
		t.Fatal(c)
	}
}

// A1: the owner adds needs-agent from their terminal, and an agent's
// session adds it again within the window. The window holds two writes, so
// neither is credited: the agent's signal never passes as the owner's.
func TestIntakeAttributionSessionWriteInTheWindowIsAmbiguous(t *testing.T) {
	h := newAttributionHarness(t)
	h.write(AppWrite{})
	h.write(AppWrite{Session: "c", AgentMarker: "CLAUDECODE"})
	if c := h.credit("label", needsAgentID); c.Result != "attribution_ambiguous" || c.Actor != "" {
		t.Fatal(c)
	}
	// Two of the owner's own writes are just as ambiguous.
	h2 := newAttributionHarness(t)
	h2.write(AppWrite{})
	h2.write(AppWrite{})
	if c := h2.credit("label", needsAgentID); c.Result != "attribution_ambiguous" {
		t.Fatal(c)
	}
}

// A2: only a person's own device, with no session and no agent marker, is
// creditable; a client that sent no marker is an older one.
func TestIntakeAttributionNeedsTheOwnersDeviceWithoutAnAgent(t *testing.T) {
	for name, w := range map[string]AppWrite{
		"session":       {Session: "c"},
		"agent marker":  {AgentMarker: "CLAUDECODE"},
		"marker unsent": {AgentMarker: MarkerUnsent},
		"google token":  {Identity: Identity{Principal: "owner@example.test", Issuer: GoogleIssuer, Subject: "g1"}},
		"ci device":     {Identity: Identity{Principal: "ci:deploy@w", Issuer: DeviceIssuer, Subject: "dev_ci"}},
	} {
		t.Run(name, func(t *testing.T) {
			h := newAttributionHarness(t)
			if w.Identity.Principal == "owner@example.test" {
				w.Identity.Principal = h.owner
			}
			h.write(w)
			if c := h.credit("label", needsAgentID); c.Result != "attributed_untrusted" || c.Actor != "" {
				t.Fatal(c)
			}
			var creditable int
			if err := h.s.db.QueryRow(`SELECT creditable FROM app_write_attributions`).Scan(&creditable); err != nil || creditable != 0 {
				t.Fatal("recorded as creditable:", creditable, err)
			}
		})
	}
}

// A4: the link is checked when intake credits, not when the write was
// recorded.
func TestIntakeAttributionChecksTrustAtCreditTime(t *testing.T) {
	h := newAttributionHarness(t)
	h.write(AppWrite{})
	h.call("intake_trust_remove", "", map[string]any{"linear_user_id": trustedUser})
	if c := h.credit("label", needsAgentID); c.Result != "attributed_untrusted" {
		t.Fatal("a removed trustee was credited:", c)
	}
	// Trusted again, but with no link: still nobody's.
	h.call("intake_trust_add", "", map[string]any{"linear_user_id": trustedUser})
	if c := h.credit("label", needsAgentID); c.Result != "attributed_untrusted" {
		t.Fatal("an unlinked trustee was credited:", c)
	}
	// A principal that is no longer the project's member credits nothing.
	h3 := newAttributionHarness(t)
	h3.write(AppWrite{Identity: Identity{Principal: "former@example.test", Issuer: DeviceIssuer, Subject: "dev_2"}})
	if _, err := h3.s.db.Exec(`UPDATE intake_trusted SET principal='former@example.test'`); err != nil {
		t.Fatal(err)
	}
	if c := h3.credit("label", needsAgentID); c.Result != "attributed_untrusted" {
		t.Fatal("a non-member was credited:", c)
	}
}

// A5: the window is the caller's; a write outside it is not seen.
func TestIntakeAttributionWindow(t *testing.T) {
	h := newAttributionHarness(t)
	h.write(AppWrite{})
	now := time.Now()
	for _, w := range [][2]time.Time{{now.Add(-time.Hour), now.Add(-time.Minute)}, {now.Add(time.Minute), now.Add(time.Hour)}} {
		c, err := h.s.IntakeAttribution(context.Background(), h.pid, intakeIssue, "label", needsAgentID, w[0], w[1])
		if err != nil || c.Result != "" {
			t.Fatal("a write outside the window was read:", c, err)
		}
	}
	if _, err := h.s.IntakeAttribution(context.Background(), h.pid, intakeIssue, "state", needsAgentID, now, now); err == nil {
		t.Fatal("an unknown kind was read")
	}
}

// Every label counts once; a write with nothing to attribute, or to a
// project with no board, records nothing; rows older than 7 days go.
func TestIntakeAttributionRecords(t *testing.T) {
	h := newAttributionHarness(t)
	h.write(AppWrite{Labels: []string{needsAgentID, strings.ToUpper(needsAgentID), otherLabelID}})
	if n := h.rows(); n != 2 {
		t.Fatal("labels not recorded once each:", n)
	}
	h.write(AppWrite{Labels: []string{}, Comment: ""})
	h.write(AppWrite{Issue: "FIX-7"})
	h.write(AppWrite{Project: "listed"})
	if n := h.rows(); n != 2 {
		t.Fatal("a write with nothing to attribute was recorded:", n)
	}
	old := layout(time.Now().Add(-8 * 24 * time.Hour))
	if _, err := h.s.db.Exec(`INSERT INTO app_write_attributions(project_id,issue_uuid,kind,target_id,principal,issuer,creditable,written_at) VALUES(?,?,'label',?,?,?,1,?)`, h.pid, intakeOther, needsAgentID, h.owner, DeviceIssuer, old); err != nil {
		t.Fatal(err)
	}
	h.write(AppWrite{})
	if n := h.rows(); n != 3 {
		t.Fatal("the 8-day-old row was not pruned:", n)
	}
}

// A3: attribution is the server's own record; no client operation reads or
// writes it.
func TestIntakeAttributionIsNotAClientOperation(t *testing.T) {
	h := newAttributionHarness(t)
	for _, op := range []string{"app_write_attribute", "intake_attribution", "app_write_attributions"} {
		if e := h.errorOf(op, "", map[string]any{"issue_uuid": intakeIssue}); e == "" {
			t.Fatal(op, "was accepted from a client")
		}
	}
	if n := h.rows(); n != 0 {
		t.Fatal(n)
	}
}

// Q2: trust add links a principal explicitly, a project member only; a
// re-add without one keeps the link, and history says it. (The link by
// email is TestPostgresIntakeTrustLinksByEmail: a local owner has no email.)
func TestIntakeTrustLinksAPrincipal(t *testing.T) {
	h := newIntakeHarness(t)
	owner := h.principal()
	if e := h.errorOf("intake_trust_add", "c", map[string]any{"linear_user_id": trustedUser, "principal": "stranger@example.test"}); !strings.Contains(e, "member of this ClawBridge project") {
		t.Fatal("a non-member was linked:", e)
	}
	list := h.call("intake_trust_add", "c", map[string]any{"linear_user_id": trustedUser, "link_email": "stranger@example.test"}).([]map[string]any)
	if len(list) != 1 || list[0]["principal"] != nil {
		t.Fatal("an email no member has was linked:", list)
	}
	list = h.call("intake_trust_add", "c", map[string]any{"linear_user_id": trustedUser, "principal": owner}).([]map[string]any)
	if list[0]["principal"] != owner {
		t.Fatal("the member was not linked:", list)
	}
	list = h.call("intake_trust_add", "c", map[string]any{"linear_user_id": trustedUser}).([]map[string]any)
	if list[0]["principal"] != owner {
		t.Fatal("a re-add dropped the link:", list)
	}
	if got := h.call("intake_trust_list", "a", nil).([]map[string]any); got[0]["principal"] != owner {
		t.Fatal(got)
	}
	linked := false
	for _, row := range h.historyRows("intake") {
		linked = linked || strings.Contains(str(row, "details"), `"principal":"`+owner+`"`)
	}
	if !linked {
		t.Fatal("history does not record the link")
	}
}

// The credited signal's queue row and history say it came through
// ClawBridge, and whose write it was.
func TestIntakeQueuedViaClawBridge(t *testing.T) {
	h := newAttributionHarness(t)
	h.call("coordinator_set", "c", map[string]any{"target_session": "c"})
	ev := IntakeEvent{Signal: "S1", Actor: trustedUser, Trusted: true, At: stamp(), Via: h.owner, Issue: IntakeIssue{UUID: intakeIssue, Identifier: "FIX-7", TitleSHA256: "t", DescriptionSHA256: "d", UpdatedAt: stamp()}}
	if got, err := h.s.IntakeEnqueueSystem(context.Background(), h.pid, ev); err != nil || got != "queued" {
		t.Fatal(got, err)
	}
	var setBy string
	if err := h.s.db.QueryRow(`SELECT cap_set_by FROM intake_queue`).Scan(&setBy); err != nil || !strings.Contains(setBy, trustedUser+" via clawbridge") {
		t.Fatal(setBy, err)
	}
	via := false
	for _, row := range h.historyRows("intake") {
		via = via || (strings.Contains(str(row, "details"), `"via":"clawbridge"`) && strings.Contains(str(row, "details"), h.owner))
	}
	if !via {
		t.Fatal("history does not say the signal came through ClawBridge")
	}
}

// historyRows is the project's history of entity, details as stored.
func (h *harness) historyRows(entity string) []map[string]any {
	h.t.Helper()
	rows, err := h.s.db.Query(`SELECT details FROM history WHERE entity=?`, entity)
	if err != nil {
		h.t.Fatal(err)
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var d string
		if err = rows.Scan(&d); err != nil {
			h.t.Fatal(err)
		}
		out = append(out, map[string]any{"details": d})
	}
	return out
}

// IntakeEnqueueNew queues an issue the project has no row for, and for one
// it has, in any state, changes nothing.
func TestIntakeEnqueueNewOnlyWithoutARow(t *testing.T) {
	h := newAttributionHarness(t)
	h.call("coordinator_set", "c", map[string]any{"target_session": "c"})
	ev := IntakeEvent{Signal: "S1", At: stamp(), Issue: IntakeIssue{UUID: intakeIssue, Identifier: "FIX-7", TitleSHA256: "t", DescriptionSHA256: "d", UpdatedAt: stamp()}}
	ctx := context.Background()
	if got, err := h.s.IntakeEnqueueNew(ctx, h.pid, ev); err != nil || got != "queued" {
		t.Fatal(got, err)
	}
	if _, err := h.s.db.Exec(`UPDATE intake_queue SET state='dismissed',closed_at=?`, stamp()); err != nil {
		t.Fatal(err)
	}
	if got, err := h.s.IntakeEnqueueNew(ctx, h.pid, ev); err != nil || got != "has_row" {
		t.Fatal(got, err)
	}
	var state string
	if err := h.s.db.QueryRow(`SELECT state FROM intake_queue`).Scan(&state); err != nil || state != "dismissed" {
		t.Fatal(state, err)
	}
}
