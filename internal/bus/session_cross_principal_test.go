package bus

import (
	"context"
	"errors"
	"reflect"
	"testing"
)

// A local store has one principal (everyone else is "a different OS user"
// before any session is looked at), so the SQLite variant reassigns
// session "a" to a second principal and plays the store's own principal as
// the intruder. The Postgres variant below uses two real members.
const sessionOwner = "bob@example.test"

func errorCode(t *testing.T, err error) string {
	t.Helper()
	// The server classifies plain refusals on the way out; do the same.
	err = classify(err)
	var coded interface{ Code() string }
	if err == nil || !errors.As(err, &coded) {
		t.Fatalf("want a coded service error, got %v", err)
	}
	return coded.Code()
}

// sessionRow reads the stored columns another principal must not change.
func (h *harness) sessionRow(name string) map[string]any {
	h.t.Helper()
	var owner, instance, role, notes, lastSeen, updatedAt, aliveUntil, aliveState string
	if err := h.s.db.QueryRow(`SELECT owner_principal,instance,COALESCE(role,''),COALESCE(notes,''),last_seen,updated_at,COALESCE(alive_until,''),COALESCE(alive_state,'') FROM sessions WHERE name=?`, name).Scan(&owner, &instance, &role, &notes, &lastSeen, &updatedAt, &aliveUntil, &aliveState); err != nil {
		h.t.Fatal(err)
	}
	return map[string]any{"owner_principal": owner, "instance": instance, "role": role, "notes": notes,
		"last_seen": lastSeen, "updated_at": updatedAt, "alive_until": aliveUntil, "alive_state": aliveState}
}

func TestAnotherPrincipalCannotRegisterOverOrActAsASession(t *testing.T) {
	h := newHarness(t)
	if h.s.principal == sessionOwner {
		t.Fatal("principals must differ")
	}
	h.call("update_session", "a", map[string]any{"notes": "bob notes"})
	// A fixed old last_seen and updated_at: any accepted call by the session's
	// owner would move them to "now", so an unchanged value proves no effect.
	if _, err := h.s.db.Exec(`UPDATE sessions SET owner_principal=?,last_seen='2000-01-01T00:00:00Z',updated_at='2000-01-01T00:00:00Z' WHERE name='a'`, sessionOwner); err != nil {
		t.Fatal(err)
	}
	before := h.sessionRow("a")
	if before["owner_principal"] != sessionOwner || before["instance"] != "a-instance" || before["role"] != "worker" || before["notes"] != "bob notes" || before["last_seen"] != "2000-01-01T00:00:00Z" {
		t.Fatal("setup", before)
	}
	unchanged := func(what string) {
		t.Helper()
		if after := h.sessionRow("a"); !reflect.DeepEqual(before, after) {
			t.Fatalf("%s changed the other principal's session: %v -> %v", what, before, after)
		}
	}
	do := func(op string, a map[string]any) error {
		_, err := h.s.Execute(context.Background(), Request{Operation: op, Workspace: "w", Project: "p", Session: "a", Args: a})
		return err
	}

	t.Run("register without an instance is refused", func(t *testing.T) {
		if code := errorCode(t, do("register_session", map[string]any{"role": "worker"})); code != "conflict" {
			t.Fatalf("code %q, want conflict", code)
		}
		unchanged("register")
	})
	t.Run("resume with the real instance is refused", func(t *testing.T) {
		if code := errorCode(t, do("register_session", map[string]any{"instance": "a-instance", "resume": true})); code != "conflict" {
			t.Fatalf("code %q, want conflict", code)
		}
		unchanged("resume")
	})
	for op, a := range map[string]map[string]any{
		"heartbeat":      {},
		"inbox":          {},
		"update_session": {"notes": "hijacked"},
		"send":           {"to": "b", "body": "as bob", "operation_id": "xprin-1"},
	} {
		t.Run("act as it with the real instance: "+op, func(t *testing.T) {
			a["instance"] = "a-instance"
			if code := errorCode(t, do(op, a)); code != "forbidden" {
				t.Fatalf("code %q, want forbidden", code)
			}
			unchanged(op)
		})
	}

	// The refused send must not have delivered anything: b's inbox, read
	// after the loop (its subtests run in map order), holds nothing from "a".
	t.Run("the refused send delivered nothing", func(t *testing.T) {
		inbox := h.call("inbox", "b", map[string]any{"unread_only": false}).(map[string]any)["messages"].([]map[string]any)
		for _, m := range inbox {
			if m["sender"] == "a" || m["body"] == "as bob" {
				t.Fatalf("a refused send reached b's inbox: %v", m)
			}
		}
	})

	// Control: the same calls on the caller's own session ("b") with a stale
	// instance get the other refusal, so the cases above tell the two apart.
	for op, a := range map[string]map[string]any{
		"inbox":          {},
		"update_session": {"notes": "stale"},
		"send":           {"to": "c", "body": "stale", "operation_id": "xprin-2"},
	} {
		t.Run("own session with a wrong instance: "+op, func(t *testing.T) {
			a["instance"] = "b-stale-instance"
			_, err := h.s.Execute(context.Background(), Request{Operation: op, Workspace: "w", Project: "p", Session: "b", Args: a})
			if code := errorCode(t, err); code != "invalid" {
				t.Fatalf("code %q, want invalid (instance mismatch)", code)
			}
		})
	}
}
