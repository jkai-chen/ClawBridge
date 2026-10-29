package bus

import (
	"context"
	"reflect"
	"strings"
	"testing"
	"time"
)

// ledgerTx is a transaction on h's store, and its project's ID.
func ledgerTx(t *testing.T, h *harness) (*transaction, string) {
	t.Helper()
	raw, err := h.s.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { raw.Rollback() })
	tx := &transaction{Tx: raw, postgres: h.s.postgres}
	var pid string
	if err = tx.QueryRow(`SELECT project_id FROM sessions WHERE name='c'`).Scan(&pid); err != nil {
		t.Fatal(err)
	}
	return tx, pid
}

// withKind adds a kind to the ledger's set for one test: a later write's
// kind (FR7, FR10) joins the same way.
func withKind(t *testing.T, kind string, keys ...string) {
	linearWriteKinds[kind] = keys
	t.Cleanup(func() { delete(linearWriteKinds, kind) })
}

func ledgerRows(t *testing.T, tx *transaction) int {
	t.Helper()
	var n int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM linear_write_reservations`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// The kinds the server records, and the detail keys each may hold: IDs of
// the subject and target only, never a member's ID, a name or a title.
// Every kind the code writes is in the set.
func TestLinearWriteKindsPinned(t *testing.T) {
	want := map[string][]string{"project_create": {"kind_label_id"}}
	if !reflect.DeepEqual(linearWriteKinds, want) {
		t.Fatalf("the kinds changed: %v; pin the new kind's detail keys here", linearWriteKinds)
	}
	for _, kind := range []string{projectCreateKind} {
		if _, ok := linearWriteKinds[kind]; !ok {
			t.Fatal("the code writes an unlisted kind:", kind)
		}
	}
}

// An unknown kind, a detail key its kind doesn't list, or an unknown role
// is refused before anything is inserted.
func TestLinearWriteRefusedBeforeInsert(t *testing.T) {
	h, _ := createsHarness(t)
	tx, pid := ledgerTx(t, h)
	now := time.Now()
	ok := linearWrite{Kind: projectCreateKind, Operation: "op-1", Session: "s", Principal: "p", ByRole: "coordinator", Team: "t", Plan: "plan"}
	bad := map[string]linearWrite{}
	w := ok
	w.Kind = "issue_delete"
	bad["kind"] = w
	w = ok
	w.Detail = map[string]any{"member_id": "m"}
	bad["detail"] = w
	w = ok
	w.ByRole = "member"
	bad["role"] = w
	for name, w := range bad {
		if _, _, err := reserveLinearWrite(context.Background(), tx, pid, w, now); err == nil {
			t.Fatal("reserved with a bad", name)
		}
		if n := ledgerRows(t, tx); n != 0 {
			t.Fatalf("a bad %s inserted %d rows", name, n)
		}
	}
}

// The kind is in the plan digest: an operation_id used again for another
// kind, or by another principal, is a conflict, never the other's replay.
// A not_sent one reopens; a settle adds its kind's detail keys only, and
// a different settle is a conflict.
func TestLinearWriteReplayAndSettle(t *testing.T) {
	h, _ := createsHarness(t)
	withKind(t, "project_team_add", "changed")
	tx, pid := ledgerTx(t, h)
	ctx, now := context.Background(), time.Now()
	if linearWritePlan("project_create", "plan") == linearWritePlan("project_team_add", "plan") {
		t.Fatal("the kind isn't in the plan digest")
	}
	w := linearWrite{Kind: "project_team_add", Operation: "op-1", Session: "s", Principal: "p", ByRole: "lead", Team: "t", Subject: "proj", Plan: "plan"}
	row, replay, err := reserveLinearWrite(ctx, tx, pid, w, now)
	if err != nil || replay || str(row, "state") != "reserved" || str(row, "detail") != "{}" {
		t.Fatal(row, replay, err)
	}
	if _, replay, err = reserveLinearWrite(ctx, tx, pid, w, now); err != nil || !replay {
		t.Fatal("a repeat isn't a replay:", replay, err)
	}
	for name, change := range map[string]func(*linearWrite){
		"kind":      func(w *linearWrite) { w.Kind = projectCreateKind },
		"principal": func(w *linearWrite) { w.Principal = "q" },
		"session":   func(w *linearWrite) { w.Session = "other" },
		"plan":      func(w *linearWrite) { w.Plan = "another plan" },
	} {
		other := w
		change(&other)
		if _, _, err = reserveLinearWrite(ctx, tx, pid, other, now); err == nil || !strings.Contains(err.Error(), "different Linear write") {
			t.Fatal("another", name, "took the reservation:", err)
		}
	}
	if _, err = linearWriteOwn(ctx, tx, pid, "op-1", "project_team_add", "s", "q"); err == nil {
		t.Fatal("another principal settles it")
	}
	if _, _, err = settleLinearWrite(ctx, tx, pid, row, "accepted", "", map[string]any{"member_id": "m"}, now); err == nil {
		t.Fatal("a settle wrote a detail key its kind doesn't list")
	}
	row, replay, err = settleLinearWrite(ctx, tx, pid, row, "accepted", "", map[string]any{"changed": []string{"teams"}}, now)
	if err != nil || replay || str(row, "state") != "accepted" || str(row, "detail") != `{"changed":["teams"]}` {
		t.Fatal(row, err)
	}
	if _, replay, err = settleLinearWrite(ctx, tx, pid, row, "accepted", "", nil, now); err != nil || !replay {
		t.Fatal("the same settle isn't a replay:", err)
	}
	if _, _, err = settleLinearWrite(ctx, tx, pid, row, "not_sent", "", nil, now); err == nil {
		t.Fatal("a different settle changed it")
	}
	w.Operation = "op-2"
	row, _, _ = reserveLinearWrite(ctx, tx, pid, w, now)
	settleLinearWrite(ctx, tx, pid, row, "not_sent", "", nil, now)
	if row, replay, err = reserveLinearWrite(ctx, tx, pid, w, now); err != nil || replay || str(row, "state") != "reserved" {
		t.Fatal("a not_sent one didn't reopen:", row, replay, err)
	}
	if _, err = tx.Exec(`UPDATE linear_write_reservations SET state='created' WHERE operation_id='op-1'`); err == nil {
		t.Fatal("the table took a state outside its set")
	}
}

// One query finds every kind's stale reservation: the overview lists each
// with its kind, and the owner is told of each once.
func TestLinearWriteStaleAcrossKinds(t *testing.T) {
	h, now := createsHarness(t)
	withKind(t, "project_team_add", "changed")
	h.pin("lane_pin_set", map[string]any{"expected_revision": 0, "kind": "session", "key": "c", "lane": "both"})
	h.seedKinds(teamTwo)
	h.call("project_create_reserve", "c", reserveArgs("op-1", teamTwo, "Engineering"))
	tx, pid := ledgerTx(t, h)
	if _, _, err := reserveLinearWrite(context.Background(), tx, pid, linearWrite{Kind: "project_team_add", Operation: "op-2", Session: "s", Principal: "p", ByRole: "lead", Team: teamTwo, Subject: createdProject, Plan: "plan"}, *now); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	*now = now.Add(11 * time.Minute)
	u := h.overview("")["project_creates"].(map[string]any)["unconfirmed"].([]map[string]any)
	if len(u) != 2 || u[0]["write"] != "project_create" || u[0]["kind"] != "Engineering" || u[1]["write"] != "project_team_add" || u[1]["subject"] != createdProject || u[1]["kind"] != nil {
		t.Fatal(u)
	}
	h.call("project_create_list", "", nil)
	if n := h.messagesOf("project-create-unconfirmed"); n != 6 {
		t.Fatal("each kind's notice, to the 3 owner sessions:", n)
	}
	h.call("project_create_ack", "", map[string]any{"operation_id": "op-2"})
	if u = h.overview("")["project_creates"].(map[string]any)["unconfirmed"].([]map[string]any); len(u) != 1 {
		t.Fatal("an acknowledged write of another kind still shows:", u)
	}
	// Settled uncertain is terminal, and shows at once, labelled, until the
	// owner acknowledges it; accepted and not_sent never show.
	tx, _ = ledgerTx(t, h)
	for op, state := range map[string]string{"op-3": "uncertain", "op-4": "accepted", "op-5": "not_sent"} {
		row, _, err := reserveLinearWrite(context.Background(), tx, pid, linearWrite{Kind: "project_team_add", Operation: op, Session: "s", Principal: "p", ByRole: "lead", Team: teamTwo, Subject: createdProject, Plan: op}, *now)
		if err == nil {
			_, _, err = settleLinearWrite(context.Background(), tx, pid, row, state, "", nil, *now)
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	u = h.overview("")["project_creates"].(map[string]any)["unconfirmed"].([]map[string]any)
	if len(u) != 2 || u[1]["operation_id"] != "op-3" || u[1]["state"] != "uncertain" || u[0]["state"] != "reserved" {
		t.Fatal(u)
	}
	h.call("project_create_list", "", nil)
	var body string
	if e := h.s.db.QueryRow(`SELECT body FROM messages WHERE kind='project-create-unconfirmed' AND body LIKE '%op-3%' LIMIT 1`).Scan(&body); e != nil || !strings.Contains(body, "uncertain") || !strings.Contains(body, "clawbridge linear writes --ack op-3") {
		t.Fatal("the uncertain notice:", body, e)
	}
	h.call("project_create_ack", "", map[string]any{"operation_id": "op-3"})
	if u = h.overview("")["project_creates"].(map[string]any)["unconfirmed"].([]map[string]any); len(u) != 1 {
		t.Fatal("an acknowledged uncertain write still shows:", u)
	}
}
