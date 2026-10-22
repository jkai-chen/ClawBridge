package bus

import (
	"context"
	"os"
	"reflect"
	"sort"
	"testing"
)

func recipientIDs(m map[string]any) []string {
	var ids []string
	for _, d := range m["deliveries"].([]map[string]any) {
		ids = append(ids, d["recipient_id"].(string))
	}
	sort.Strings(ids)
	return ids
}

func TestReplyToOwnMessageReachesItsRecipients(t *testing.T) {
	h := newHarness(t)
	a, b, c := h.sessionID("a"), h.sessionID("b"), h.sessionID("c")
	direct := h.call("send", "a", map[string]any{"to": "b", "body": "question", "operation_id": "direct"}).(map[string]any)
	followUp := h.call("reply", "a", map[string]any{"message_id": direct["id"], "body": "one more thing", "operation_id": "follow-up"}).(map[string]any)
	if got := recipientIDs(followUp); !reflect.DeepEqual(got, []string{b}) || followUp["address"] != "b" || followUp["thread_id"] != direct["id"] {
		t.Fatal("a reply to one's own direct message must reach its recipient", got, followUp["address"])
	}
	// The other side's reply still returns to the sender.
	answer := h.call("reply", "b", map[string]any{"message_id": direct["id"], "body": "answer", "operation_id": "answer"}).(map[string]any)
	if got := recipientIDs(answer); !reflect.DeepEqual(got, []string{a}) {
		t.Fatal("a reply to another session's message must return to its sender", got)
	}
	group := h.call("send", "a", map[string]any{"to": "@role:worker", "body": "to the role", "operation_id": "group"}).(map[string]any)
	want := []string{b, c}
	sort.Strings(want)
	followUp = h.call("reply", "a", map[string]any{"message_id": group["id"], "body": "to the role again", "operation_id": "group-follow-up"}).(map[string]any)
	if got := recipientIDs(followUp); !reflect.DeepEqual(got, want) || followUp["address"] != "@role:worker" {
		t.Fatal("a reply to one's own role message must reach the original recipients", got, followUp["address"])
	}
	// Retired recipients are dropped, as for any send; none left is an error.
	h.call("retire_session", "c", nil)
	followUp = h.call("reply", "a", map[string]any{"message_id": group["id"], "body": "c has gone", "operation_id": "after-retire"}).(map[string]any)
	if got := recipientIDs(followUp); !reflect.DeepEqual(got, []string{b}) {
		t.Fatal("a retired recipient received a reply", got)
	}
	h.call("retire_session", "b", nil)
	h.fail("reply", "a", map[string]any{"message_id": group["id"], "body": "nobody left", "operation_id": "nobody"})
}

func TestPostgresReplyToOwnMessageReachesItsRecipients(t *testing.T) {
	dsn := os.Getenv("CLAWBRIDGE_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("reviewer supplies disposable Postgres DSN")
	}
	ctx := context.Background()
	owner := "owner@reply-test.example"
	s, err := OpenPostgres(ctx, dsn, owner)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	w := newID("unit_")
	exec := func(op, session string, a map[string]any) (Result, error) {
		if session != "" {
			a["instance"] = session + "-inst"
		}
		return s.ExecuteAs(ctx, owner, Request{Operation: op, Workspace: w, Project: "p", Session: session, Args: a})
	}
	call := func(op, session string, a map[string]any) map[string]any {
		t.Helper()
		r, e := exec(op, session, a)
		if e != nil {
			t.Fatalf("%s: %v", op, e)
		}
		return r.Data.(map[string]any)
	}
	call("workspace_init", "", map[string]any{"mode": "server"})
	call("project_create", "", map[string]any{"slug": "p"})
	ids := map[string]string{}
	for _, name := range []string{"a", "b", "c"} {
		ids[name] = call("register_session", name, map[string]any{"role": "worker"})["id"].(string)
	}
	group := call("send", "a", map[string]any{"to": "@role:worker", "body": "to the role", "operation_id": "group"})
	want := []string{ids["b"], ids["c"]}
	sort.Strings(want)
	followUp := call("reply", "a", map[string]any{"message_id": group["id"], "body": "to the role again", "operation_id": "group-follow-up"})
	if got := recipientIDs(followUp); !reflect.DeepEqual(got, want) || followUp["address"] != "@role:worker" {
		t.Fatal("a reply to one's own message must reach the original recipients", got, followUp["address"])
	}
	answer := call("reply", "b", map[string]any{"message_id": group["id"], "body": "answer", "operation_id": "answer"})
	if got := recipientIDs(answer); !reflect.DeepEqual(got, []string{ids["a"]}) {
		t.Fatal("a reply to another session's message must return to its sender", got)
	}
	call("retire_session", "b", map[string]any{})
	call("retire_session", "c", map[string]any{})
	if _, e := exec("reply", "a", map[string]any{"message_id": group["id"], "body": "nobody left", "operation_id": "nobody"}); e == nil {
		t.Fatal("a reply reached no active recipient but succeeded")
	}
}
