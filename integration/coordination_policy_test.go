package integration

import (
	"context"
	"reflect"
	"testing"

	"clawbridge/internal/bus"
)

func TestCoordinationPolicyStatusShapeAndNoStamp(t *testing.T) {
	for _, pg := range []bool{false, true} {
		name := "SQLite"
		if pg {
			name = "Postgres"
		}
		t.Run(name, func(t *testing.T) { coordinationPolicyStatusShapeAndNoStamp(t, pg) })
	}
}
func coordinationPolicyStatusShapeAndNoStamp(t *testing.T, pg bool) {
	a := newNotificationAuthority(t, pg)
	session := "worker"
	source := "local-authority"
	if pg {
		session = "alice"
		source = "server"
	}
	request := func(op string, args map[string]any) bus.Request {
		return bus.Request{Operation: op, Workspace: a.workspace, Project: "p", Session: session, Args: args}
	}
	poll := func(op string) map[string]any {
		t.Helper()
		r, err := a.execute(request(op, actor(session, nil)))
		if err != nil {
			t.Fatal(err)
		}
		if r.Source != source {
			t.Fatal("status changed authority")
		}
		return object(t, jsonData(t, r.Data))
	}
	initial := poll("notification_status")
	if len(initial) != 7 || initial["policy"] != "accept" {
		t.Fatalf("new status does not default accept: %v", initial)
	}
	var originalInstance string
	if err := a.db.QueryRow(a.query("SELECT instance FROM sessions WHERE id=?"), initial["session_id"]).Scan(&originalInstance); err != nil {
		t.Fatal(err)
	}
	for _, policy := range []string{"hold", "refuse", "accept"} {
		want := map[string]any{"workspace_id": initial["workspace_id"], "project_id": initial["project_id"], "session_id": initial["session_id"], "policy": policy}
		for repeat := 0; repeat < 2; repeat++ {
			result, err := a.execute(request("set_notification_policy", actor(session, map[string]any{"policy": policy})))
			if err != nil {
				t.Fatalf("policy=%s repeat=%d: %v", policy, repeat, err)
			}
			changed := object(t, jsonData(t, result.Data))
			if result.Source != source || !reflect.DeepEqual(changed, want) {
				t.Errorf("setter repeat=%d source=%s shape=%v want=%v", repeat, result.Source, changed, want)
			}
			var currentInstance string
			if err := a.db.QueryRow(a.query("SELECT instance FROM sessions WHERE id=?"), initial["session_id"]).Scan(&currentInstance); err != nil {
				t.Fatal(err)
			}
			if currentInstance != originalInstance {
				t.Fatal("policy mutation/repeat rotated actor instance")
			}
		}
		// Deliberately old presence makes accidental heartbeat visible without sleeps.
		a.exec("UPDATE sessions SET last_seen=? WHERE project_id IN (SELECT id FROM projects WHERE workspace_id=?)", "2001-01-01T00:00:00.000000000Z", a.workspaceID)
		before := a.snapshot()
		for i := 0; i < 3; i++ {
			current := poll("notification_status")
			if len(current) != 7 || current["policy"] != policy {
				t.Error("policy status lost field")
			}
			old := poll("inbox_status")
			if len(old) != 6 {
				t.Error("six-field status changed")
			}
			if _, ok := old["policy"]; ok {
				t.Error("old status exposes policy")
			}
		}
		if before != a.snapshot() {
			t.Fatal("status observation changed persisted state")
		}
	}
	for _, args := range []map[string]any{
		actor(session, map[string]any{"policy": "unknown"}), actor(session, map[string]any{"policy": ""}), actor(session, map[string]any{"policy": "hold", "target_session": "lead"}), {"policy": "hold"}, {"policy": "hold", "instance": "wrong"},
	} {
		before := a.snapshot()
		if _, err := a.execute(request("set_notification_policy", args)); err == nil {
			t.Error("invalid/peer policy accepted")
		}
		if before != a.snapshot() {
			t.Error("rejected policy mutated authority")
		}
	}
}
func TestCoordinationPolicyPersistsResumeAndFencesOldInstance(t *testing.T) {
	f := newFixture(t)
	f.setup("w", "p")
	f.call("set_notification_policy", "w", "p", "worker", actor("worker", map[string]any{"policy": "refuse"}))
	resumed := f.call("register_session", "w", "p", "worker", map[string]any{"instance": "worker-instance", "resume": true})
	next := stringField(t, resumed, "instance")
	f.reject("set_notification_policy", "w", "p", "worker", actor("worker", map[string]any{"policy": "accept"}))
	result := object(t, f.call("notification_status", "w", "p", "worker", map[string]any{"instance": next}))
	if result["policy"] != "refuse" {
		t.Fatal("resume reset wake policy")
	}
	f.call("retire_session", "w", "p", "worker", map[string]any{"instance": next})
	f.reject("set_notification_policy", "w", "p", "worker", map[string]any{"instance": next, "policy": "accept"})
	f.reject("notification_status", "w", "p", "worker", map[string]any{"instance": next})
}
func TestCoordinationReceiptMetadataAndAuditPrivacy(t *testing.T) {
	f := newFixture(t)
	f.setup("w", "p")
	c := coordinationObject("coordinator-7", "ready")
	sent := coordinationSend(f, "worker", "receipt-coordination", c)
	status := object(t, f.call("message_status", "w", "p", "lead", actor("lead", map[string]any{"message_id": stringField(t, sent, "id")})))
	if !reflect.DeepEqual(status["coordination"], c) {
		t.Errorf("sender receipt lost coordination metadata: %v", status["coordination"])
	}
	f.reject("message_status", "w", "p", "outsider", actor("outsider", map[string]any{"message_id": stringField(t, sent, "id")}))
	events := pageItems(t, "events", f.call("history", "w", "p", "outsider", actor("outsider", nil)))
	for _, event := range events {
		details := object(t, event)["details"]
		if m, ok := details.(map[string]any); ok {
			if _, present := m["coordination"]; present {
				t.Error("participant context exposed in audit details")
			}
		}
	}
}
func TestCoordinationPostgresReaderOwnPolicy(t *testing.T) {
	f := newPGFixture(t)
	f.setup("p")
	f.call(pgPrincipal("reader"), "set_notification_policy", "p", "reader", actor("reader", map[string]any{"policy": "hold"}))
	got := object(t, f.call(pgPrincipal("reader"), "notification_status", "p", "reader", actor("reader", nil)))
	if got["policy"] != "hold" {
		t.Fatal("reader own policy not applied")
	}
	for _, principal := range []string{pgPrincipal("lead"), pgOwner, pgPrincipal("bob")} {
		f.reject(principal, "set_notification_policy", "p", "reader", actor("reader", map[string]any{"policy": "accept"}))
	}
	f.reject(pgOwner, "set_notification_policy", "p", "owner", actor("owner", map[string]any{"policy": "accept", "target_session": "reader"}))
	// Normal administrative project roles confer no peer wake-policy override.
	if _, err := f.store.ExecuteAs(context.Background(), pgOwner, f.request("notification_status", "p", "reader", actor("reader", nil))); err == nil {
		t.Fatal("administrator inspected peer's private notification status")
	}
}
