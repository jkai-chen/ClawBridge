package integration

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"clawbridge/internal/bus"
)

func TestCoordinationParserExactBounds(t *testing.T) {
	for _, intent := range []string{"request", "accept", "decline", "clarify", "progress", "ready", "blocked", "handoff"} {
		input := coordinationObject(" "+strings.Repeat("R", 126)+" ", intent)
		input["source_commit"] = strings.Repeat("a", 64)
		input["request_id"] = strings.Repeat("X", 128)
		parsed, err := bus.ParseCoordination(input)
		if err != nil {
			t.Fatalf("exact bounds rejected for %s: %v", intent, err)
		}
		if parsed.AssignmentRevision != input["assignment_revision"] || parsed.RequestID != input["request_id"] || parsed.SourceCommit != input["source_commit"] {
			t.Fatal("opaque references trimmed or normalized")
		}
	}
	for key, values := range map[string][]any{
		"issue_id":      {"AAAAAAAA-bbbb-cccc-dddd-eeeeeeeeeeee", "11111111222233334444555555555555"},
		"source_commit": {strings.Repeat("a", 39), strings.Repeat("a", 41), strings.Repeat("a", 63), strings.Repeat("A", 64), strings.Repeat("a", 65), nil},
		"request_id":    {"", strings.Repeat("X", 129), "contains space", "contains/slash", "contains\ncontrol", nil},
	} {
		for _, value := range values {
			input := coordinationObject("opaque", "request")
			input[key] = value
			if _, err := bus.ParseCoordination(input); err == nil {
				t.Errorf("invalid %s accepted", key)
			}
		}
	}
	// The field bounds alone do not establish the encoded bound: printable '<'
	// expands during canonical JSON serialization. Check the actual byte budget.
	oversize := bus.Coordination{IssueID: coordinationIssue, AssignmentRevision: strings.Repeat("<", 128), Intent: "request", RequestID: strings.Repeat("X", 128), SourceCommit: strings.Repeat("a", 64)}
	raw, err := json.Marshal(oversize)
	if err != nil || len(raw) <= 1024 {
		t.Fatal("invalid oversize fixture")
	}
	if _, err = bus.ParseCoordination(oversize); err == nil {
		t.Fatal("canonical metadata byte cap not enforced")
	}
}

func TestCoordinationCorrelationLifecycleAtomicity(t *testing.T) {
	a := newNotificationAuthority(t, false)
	call := func(op, workspace, session string, args map[string]any) map[string]any {
		t.Helper()
		r, err := a.execute(bus.Request{Operation: op, Workspace: workspace, Project: "p", Session: session, Args: actor(session, args)})
		if err != nil {
			t.Fatal(err)
		}
		return object(t, jsonData(t, r.Data))
	}
	reject := func(op, workspace, session string, args map[string]any) {
		t.Helper()
		before := a.snapshot()
		if _, err := a.execute(bus.Request{Operation: op, Workspace: workspace, Project: "p", Session: session, Args: actor(session, args)}); err == nil {
			t.Fatal("invalid context accepted")
		}
		if before != a.snapshot() {
			t.Fatal("failed correlation changed domain state/heartbeat")
		}
	}
	root := call("send", "w", "lead", map[string]any{"to": "worker", "body": "progress question", "operation_id": "nested-root", "coordination": coordinationObject("opaque-current", "progress")})
	rootID := root["id"].(string)
	metadata := coordinationObject("opaque-current", "ready")
	metadata["request_id"] = rootID
	parent := call("reply", "w", "worker", map[string]any{"message_id": rootID, "body": "artifact context", "operation_id": "nested-parent", "coordination": metadata})
	parentID := parent["id"].(string)
	ack := coordinationObject("opaque-current", "accept")
	ack["request_id"] = rootID
	reject("reply", "w", "lead", map[string]any{"message_id": parentID, "body": "wrong immediate correlation", "operation_id": "wrong-root-not-parent", "coordination": ack})
	ack["request_id"] = parentID
	response := call("reply", "w", "lead", map[string]any{"message_id": parentID, "body": "context accepted not approval", "operation_id": "nested-correct", "coordination": ack})
	if response["thread_id"] != rootID || response["reply_to"] != parentID || !reflect.DeepEqual(response["coordination"], ack) {
		t.Fatal("nested parent/root distinction lost")
	}
	plain := call("reply", "w", "worker", map[string]any{"message_id": response["id"], "body": "plain response", "operation_id": "plain-no-inherit"})
	if _, present := plain["coordination"]; present {
		t.Fatal("omitted metadata inherited from parent")
	}
	legacy := call("send", "w", "lead", map[string]any{"to": "worker", "body": "legacy context", "operation_id": "legacy-parent"})
	ack["request_id"] = legacy["id"]
	reject("reply", "w", "worker", map[string]any{"message_id": legacy["id"], "body": "cannot invent context", "operation_id": "legacy-context-reject", "coordination": ack})
	ack["request_id"] = parentID
	reject("send", "other-workspace", "worker", map[string]any{"to": "lead", "body": "cross-workspace context", "operation_id": "cross-workspace-link", "coordination": ack})
	reject("send", "w", "outsider", map[string]any{"to": "lead", "body": "not a participant", "operation_id": "outsider-link", "coordination": ack})
	reject("reply", "w", "lead", map[string]any{"message_id": parentID, "body": "valid metadata does not waive kind", "kind": "human-approved", "operation_id": "metadata-not-authority", "coordination": ack})
	a.exec("UPDATE messages SET expires_at=? WHERE id=?", "2000-01-01T00:00:00.000000000Z", parentID)
	for _, intent := range []string{"accept", "progress"} {
		link := coordinationObject("opaque-current", intent)
		link["request_id"] = parentID
		reject("send", "w", "lead", map[string]any{"to": "worker", "body": "expired context", "operation_id": "expired-link-" + intent, "coordination": link})
	}
}
