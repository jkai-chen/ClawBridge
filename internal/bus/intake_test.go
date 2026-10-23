package bus

import (
	"context"
	"errors"
	"strings"
	"testing"
)

const (
	intakeIssue  = "00000000-0000-4000-8000-0000000000b1"
	intakeOther  = "00000000-0000-4000-8000-0000000000b2"
	trustedUser  = "00000000-0000-4000-8000-0000000000f1"
	strangerUser = "00000000-0000-4000-8000-0000000000f2"
)

// intakeHarness has session c as the designated coordinator and an intake
// token for the project.
type intakeHarness struct {
	*harness
	token  string
	proved []string
	prove  func(ctx context.Context, wid, pid, issue string) (IntakeIssue, error)
}

func newIntakeHarness(t *testing.T) *intakeHarness {
	t.Helper()
	h := &intakeHarness{harness: newHarness(t)}
	h.call("coordinator_set", "c", map[string]any{"target_session": "c"})
	h.token = h.call("intake_token_create", "c", map[string]any{}).(map[string]any)["token"].(string)
	h.prove = func(_ context.Context, _, _, issue string) (IntakeIssue, error) {
		h.proved = append(h.proved, issue)
		switch issue {
		case intakeIssue, "FIX-7":
			return IntakeIssue{UUID: intakeIssue, Identifier: "FIX-7", TitleSHA256: "t1", DescriptionSHA256: "d1", UpdatedAt: "2026-10-23T10:00:00Z"}, nil
		}
		return IntakeIssue{}, ErrIntakeOutOfBinding
	}
	return h
}

func (h *intakeHarness) request(issue string) map[string]any {
	h.t.Helper()
	out, err := h.s.IntakeRequest(context.Background(), h.token, issue, h.prove)
	if err != nil {
		h.t.Fatal(err)
	}
	return out
}

func (h *intakeHarness) intakeInbox(session string) []map[string]any {
	h.t.Helper()
	out := []map[string]any{}
	for _, m := range h.call("inbox", session, nil).(map[string]any)["messages"].([]map[string]any) {
		if m["kind"] == "intake" {
			out = append(out, m)
		}
	}
	return out
}

func TestIntakePolicyDefaultsToNotifyAndOnlyLeadsChangeIt(t *testing.T) {
	h := newIntakeHarness(t)
	p := h.call("intake_policy_get", "a", nil).(map[string]any)
	if p["mode"] != "notify" || p["default"] != true || strings.Join(p["signals"].([]string), ",") != "S1,S2,S3,S5" {
		t.Fatal("default policy:", p)
	}
	for _, mode := range []string{"offer", "auto"} {
		if e := h.errorOf("intake_policy_set", "c", map[string]any{"mode": mode}); !strings.Contains(e, "not available yet") {
			t.Fatal(mode, "accepted:", e)
		}
	}
	if e := h.errorOf("intake_policy_set", "c", map[string]any{"signals": []any{"S9"}}); !strings.Contains(e, "S1, S2") {
		t.Fatal(e)
	}
	set := h.call("intake_policy_set", "c", map[string]any{"mode": "off", "signals": []any{"S1", "S5", "S1"}, "roles": []any{"engineer"}, "expected_revision": 0}).(map[string]any)
	if set["mode"] != "off" || set["revision"] != 1 || strings.Join(set["signals"].([]string), ",") != "S1,S5" {
		t.Fatal(set)
	}
	if e := h.errorOf("intake_policy_set", "c", map[string]any{"mode": "notify", "expected_revision": 0}); !strings.Contains(e, "changed") {
		t.Fatal("a stale revision was accepted:", e)
	}
	if !strings.Contains(strings.Join(h.actions("intake"), ","), "policy_set") {
		t.Fatal("policy change not audited:", h.actions("intake"))
	}
}

func TestIntakeTrustAndTokensAreLeadOnly(t *testing.T) {
	h := newIntakeHarness(t)
	list := h.call("intake_trust_add", "c", map[string]any{"linear_user_id": strings.ToUpper(trustedUser)}).([]map[string]any)
	if len(list) != 1 || list[0]["linear_user_id"] != trustedUser {
		t.Fatal(list)
	}
	if e := h.errorOf("intake_trust_add", "c", map[string]any{"linear_user_id": "not-a-uuid"}); e == "" {
		t.Fatal("a non-UUID was trusted")
	}
	h.call("intake_trust_remove", "c", map[string]any{"linear_user_id": trustedUser})
	if got := h.call("intake_trust_list", "a", nil).([]map[string]any); len(got) != 0 {
		t.Fatal(got)
	}
	tokens := h.call("intake_token_list", "c", nil).([]map[string]any)
	if len(tokens) != 1 {
		t.Fatal(tokens)
	}
	for _, row := range tokens {
		for k, v := range row {
			if s, ok := v.(string); ok && strings.HasPrefix(s, intakeTokenPrefix) {
				t.Fatal("token listed:", k)
			}
		}
	}
	h.call("intake_token_revoke", "c", map[string]any{"token_id": tokens[0]["id"]})
	if _, err := h.s.IntakeToken(context.Background(), h.token); !errors.Is(err, ErrIntakeUnauthorized) {
		t.Fatal("a revoked token passed:", err)
	}
}

// A Loop's request is proven in the binding, queued once, and told to the
// coordinator from typed fields only, capped at offer.
func TestIntakeRequestQueuesAndNotifiesTheCoordinator(t *testing.T) {
	h := newIntakeHarness(t)
	if g, err := h.s.IntakeToken(context.Background(), h.token); err != nil || g.ProjectID == "" {
		t.Fatal(g, err)
	}
	if out := h.request(intakeIssue); out["result"] != "queued" || out["request_id"] == nil {
		t.Fatal(out)
	}
	notices := h.intakeInbox("c")
	if len(notices) != 1 {
		t.Fatal("coordinator not told:", notices)
	}
	body := notices[0]["body"].(string)
	for _, want := range []string{"FIX-7", intakeIssue, "signal S5", "Loop", "no further than offer", intakeLine} {
		if !strings.Contains(body, want) {
			t.Fatalf("notice lacks %q:\n%s", want, body)
		}
	}
	if out := h.request(intakeIssue); out["result"] != "already_queued" || len(h.intakeInbox("c")) != 1 {
		t.Fatal("a repeat was queued or told again:", out)
	}
	row := h.call("intake_list", "a", map[string]any{"state": "queued"}).([]map[string]any)
	if len(row) != 1 || row[0]["trigger_cap"] != "offer" || row[0]["signals"] != "S5" {
		t.Fatal(row)
	}
}

func TestIntakeRequestRefusals(t *testing.T) {
	h := newIntakeHarness(t)
	ctx := context.Background()
	if _, err := h.s.IntakeRequest(ctx, "cbi_"+strings.Repeat("0", 64), intakeIssue, h.prove); !errors.Is(err, ErrIntakeUnauthorized) {
		t.Fatal("an unknown token passed:", err)
	}
	if _, err := h.s.IntakeRequest(ctx, "Bearer google-id-token", intakeIssue, h.prove); !errors.Is(err, ErrIntakeUnauthorized) {
		t.Fatal("a non-intake token passed:", err)
	}
	if out, _ := h.s.IntakeRequest(ctx, h.token, intakeIssue, nil); out["code"] != "app_unavailable" {
		t.Fatal("no prover, yet:", out)
	}
	if out := h.request("not an issue"); out["code"] != "invalid_issue" {
		t.Fatal(out)
	}
	h.call("intake_policy_set", "c", map[string]any{"signals": []any{"S1"}})
	if out := h.request(intakeIssue); out["code"] != "intake_off" || len(h.proved) != 0 {
		t.Fatal("S5 disabled, yet:", out, h.proved)
	}
	h.call("intake_policy_set", "c", map[string]any{"mode": "off", "signals": []any{"S5"}})
	if out := h.request(intakeIssue); out["code"] != "intake_off" {
		t.Fatal("intake off, yet:", out)
	}
}

// Out-of-binding requests are refused without detail and suspend the token
// past the policy's limit.
func TestIntakeOutOfBindingSuspendsTheToken(t *testing.T) {
	h := newIntakeHarness(t)
	h.call("intake_policy_set", "c", map[string]any{"out_of_binding_limit": 2})
	for i := 0; i < 2; i++ {
		if out := h.request(intakeOther); out["code"] != "not_in_binding" {
			t.Fatal(out)
		}
	}
	if _, err := h.s.IntakeToken(context.Background(), h.token); err != nil {
		t.Fatal("suspended before the limit:", err)
	}
	h.request(intakeOther)
	if _, err := h.s.IntakeToken(context.Background(), h.token); !errors.Is(err, ErrIntakeUnauthorized) {
		t.Fatal("not suspended past the limit:", err)
	}
	if !strings.Contains(strings.Join(h.actions("intake"), ","), "token_suspended") {
		t.Fatal("suspension not audited")
	}
}

// The S5 sub-budget pays the proofs; an identifier costs a lookup too.
// Past it, nothing is proven or queued (IN-M3).
func TestIntakeS5BudgetIsItsOwn(t *testing.T) {
	h := newIntakeHarness(t)
	h.call("intake_policy_set", "c", map[string]any{"s5_calls_per_hour": 3})
	if out := h.request("FIX-7"); out["result"] != "queued" {
		t.Fatal(out)
	}
	if out := h.request("FIX-7"); out["code"] != "rate_limited" || len(h.proved) != 1 {
		t.Fatal("the sub-budget let a second identifier through:", out, h.proved)
	}
	if out := h.request(intakeIssue); out["result"] != "already_queued" || len(h.proved) != 2 {
		t.Fatal("a UUID request did not fit the last call:", out)
	}
	if out := h.request(intakeIssue); out["code"] != "rate_limited" {
		t.Fatal(out)
	}
}

func TestIntakeTokenHourlyLimit(t *testing.T) {
	h := newIntakeHarness(t)
	h.call("intake_policy_set", "c", map[string]any{"s5_calls_per_hour": 200})
	for i := 0; i < intakeTokenCallsPerHour; i++ {
		if out := h.request(intakeIssue); out["result"] == "refused" {
			t.Fatal(i, out)
		}
	}
	if out := h.request(intakeIssue); out["code"] != "rate_limited" {
		t.Fatal("the token's hourly limit let a request through:", out)
	}
}

// Caps (§1.1, IN2-L1): a trusted proven actor may go furthest, a Loop to
// offer, anyone else to notify; a later trusted transition raises a row's
// cap and re-records its text, and nothing lowers it.
func TestIntakeCapsOnlyRise(t *testing.T) {
	h := newIntakeHarness(t)
	ctx := context.Background()
	enqueue := func(ev IntakeEvent) string {
		t.Helper()
		var result string
		err := h.inTx(func(tx *transaction, w map[string]any, pid string) error {
			policy, e := intakePolicy(ctx, tx, pid)
			if e != nil {
				return e
			}
			result, e = h.s.intakeEnqueue(ctx, tx, w, pid, policy, ev)
			return e
		})
		if err != nil {
			t.Fatal(err)
		}
		return result
	}
	row := func() map[string]any {
		return h.call("intake_list", "a", nil).([]map[string]any)[0]
	}
	issue := IntakeIssue{UUID: intakeIssue, Identifier: "FIX-7", TitleSHA256: "t1", DescriptionSHA256: "d1"}
	enqueue(IntakeEvent{Signal: "S1", Issue: issue, Actor: strangerUser})
	if r := row(); r["trigger_cap"] != "notify" {
		t.Fatal("an untrusted actor went past notify:", r)
	}
	edited := issue
	edited.DescriptionSHA256 = "d2"
	enqueue(IntakeEvent{Signal: "S2", Issue: edited, Actor: trustedUser, Trusted: true})
	var digest string
	if e := h.s.db.QueryRow(`SELECT queued_description_sha256 FROM intake_queue`).Scan(&digest); e != nil || digest != "d2" || row()["trigger_cap"] != "auto" || row()["signals"] != "S1,S2" {
		t.Fatal("a trusted transition did not raise the cap and re-record the text:", row(), digest)
	}
	enqueue(IntakeEvent{Signal: "S5", Issue: issue})
	if row()["trigger_cap"] != "auto" {
		t.Fatal("a later event lowered the cap:", row())
	}
}

func TestIntakeDismissAndRearm(t *testing.T) {
	h := newIntakeHarness(t)
	h.request(intakeIssue)
	// Who may dismiss is a hosted-role rule; a local store's sessions are all
	// the owner's (see the Postgres test).
	if out := h.call("intake_dismiss", "c", map[string]any{"issue_uuid": intakeIssue}).(map[string]any); out["state"] != "dismissed" {
		t.Fatal(out)
	}
	if out := h.call("intake_rearm", "c", map[string]any{"issue_uuid": intakeIssue}).(map[string]any); out["rearmed"] != 1 && out["rearmed"] != int64(1) {
		t.Fatal(out)
	}
	// A new request re-opens a dismissed row.
	if out := h.request(intakeIssue); out["result"] != "queued" {
		t.Fatal(out)
	}
}

// With no active coordinator the row records it; nobody is told.
func TestIntakeWithoutCoordinatorRecordsNoRecipient(t *testing.T) {
	h := newIntakeHarness(t)
	if _, e := h.s.db.Exec(`UPDATE projects SET coordinator_session=NULL`); e != nil {
		t.Fatal(e)
	}
	h.request(intakeIssue)
	if r := h.call("intake_list", "a", nil).([]map[string]any)[0]; r["reason"] != "no_recipient" {
		t.Fatal(r)
	}
}

// inTx runs fn in one transaction on the harness's only project.
func (h *intakeHarness) inTx(fn func(tx *transaction, w map[string]any, pid string) error) error {
	// The store has one connection: resolve the session before taking it.
	sid := h.sessionID("c")
	raw, err := h.s.db.Begin()
	if err != nil {
		return err
	}
	tx := &transaction{Tx: raw}
	defer tx.Rollback()
	p, err := one(context.Background(), tx, `SELECT p.id,p.workspace_id FROM projects p JOIN sessions s ON s.project_id=p.id WHERE s.id=?`, sid)
	if err != nil {
		return err
	}
	w, err := one(context.Background(), tx, `SELECT * FROM workspaces WHERE id=?`, p["workspace_id"])
	if err != nil {
		return err
	}
	if err = fn(tx, w, str(p, "id")); err != nil {
		return err
	}
	return tx.Commit()
}
