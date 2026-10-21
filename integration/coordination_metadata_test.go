package integration

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"

	"clawbridge/internal/bus"
)

const coordinationIssue = "11111111-2222-3333-4444-555555555555"

func coordinationObject(revision, intent string) map[string]any {
	return map[string]any{"issue_id": coordinationIssue, "assignment_revision": revision, "intent": intent}
}

func TestCoordinationConcurrentIdenticalRetry(t *testing.T) {
	for _, pg := range []bool{false, true} {
		name := "SQLite"
		if pg {
			name = "Postgres"
		}
		t.Run(name, func(t *testing.T) {
			var execute func() (bus.Result, error)
			metadata := coordinationObject("same-concurrent-revision", "ready")
			if pg {
				f := newPGFixture(t)
				f.setup("p")
				r := f.request("send", "p", "alice", actor("alice", map[string]any{"to": "bob", "body": "same structured retry", "operation_id": "same-structured-op", "coordination": metadata}))
				execute = func() (bus.Result, error) { return f.store.ExecuteAs(context.Background(), pgPrincipal("alice"), r) }
			} else {
				f := newFixture(t)
				f.setup("w", "p")
				r := f.request("send", "w", "p", "lead", actor("lead", map[string]any{"to": "worker", "body": "same structured retry", "operation_id": "same-structured-op", "coordination": metadata}))
				execute = func() (bus.Result, error) { return f.store.Execute(context.Background(), r) }
			}
			const workers = 8
			results := make(chan bus.Result, workers)
			failures := make(chan error, workers)
			var wg sync.WaitGroup
			for i := 0; i < workers; i++ {
				wg.Add(1)
				go func() {
					defer wg.Done()
					r, err := execute()
					if err != nil {
						failures <- err
					} else {
						results <- r
					}
				}()
			}
			wg.Wait()
			close(results)
			close(failures)
			for err := range failures {
				t.Error(err)
			}
			id := ""
			count := 0
			for result := range results {
				count++
				data := object(t, jsonData(t, result.Data))
				if !reflect.DeepEqual(data["coordination"], metadata) {
					t.Error("concurrent retry lost metadata")
				}
				current := stringField(t, data, "id")
				if id != "" && id != current {
					t.Error("concurrent retry duplicated message")
				}
				id = current
				if len(data["deliveries"].([]any)) != 1 {
					t.Error("concurrent retry changed fanout")
				}
			}
			if count != workers {
				t.Fatal("concurrent retry lost successful result")
			}
		})
	}
}
func coordinationSend(f *fixture, to, id string, metadata any) any {
	args := actor("lead", map[string]any{"to": to, "body": "bounded coordination context", "operation_id": id, "coordination": metadata})
	return f.call("send", "w", "p", "lead", args)
}
func TestCoordinationMetadataValidationAndRetry(t *testing.T) {
	f := newFixture(t)
	f.setup("w", "p", "other")
	valid := coordinationObject("coordinator-7", "request")
	valid["source_commit"] = strings.Repeat("a", 40)
	sent := coordinationSend(f, "worker", "metadata-original", valid)
	if !reflect.DeepEqual(object(t, sent)["coordination"], valid) {
		t.Fatal("canonical metadata missing or changed")
	}
	// Different JSON key order must retain logical operation identity.
	reordered := json.RawMessage(`{"source_commit":"` + strings.Repeat("a", 40) + `","intent":"request","assignment_revision":"coordinator-7","issue_id":"` + coordinationIssue + `"}`)
	replay := coordinationSend(f, "worker", "metadata-original", reordered)
	if !reflect.DeepEqual(sent, replay) {
		t.Fatal("key order changed exact retry")
	}
	for key, value := range map[string]any{"issue_id": "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee", "assignment_revision": "coordinator-8", "intent": "progress", "source_commit": strings.Repeat("b", 40), "request_id": stringField(t, sent, "id")} {
		changed := map[string]any{}
		for k, v := range valid {
			changed[k] = v
		}
		changed[key] = value
		f.reject("send", "w", "p", "lead", actor("lead", map[string]any{"to": "worker", "body": "bounded coordination context", "operation_id": "metadata-original", "coordination": changed}))
	}
	invalid := map[string]any{"null": nil, "empty": map[string]any{}, "array": []any{}, "scalar": "text"}
	for key, value := range map[string]any{"issue_id": "UPPERCASE-INVALID", "assignment_revision": "", "intent": "completed", "request_id": "", "source_commit": "", "human_approval": true} {
		changed := coordinationObject("coordinator-7", "request")
		changed[key] = value
		invalid[key] = changed
	}
	for name, value := range map[string]any{"revision-controls": "bad\nrevision", "revision-unicode": "révision", "revision-overlong": strings.Repeat("x", 129), "revision-secret": "ghp_" + strings.Repeat("a", 36), "revision-null": nil, "revision-number": 7} {
		changed := coordinationObject("coordinator-7", "request")
		changed["assignment_revision"] = value
		invalid[name] = changed
	}
	for _, key := range []string{"issue_id", "assignment_revision", "intent"} {
		changed := coordinationObject("coordinator-7", "request")
		delete(changed, key)
		invalid["missing-"+key] = changed
	}
	for _, intent := range []string{"accept", "decline", "clarify"} {
		invalid["missing-request-"+intent] = coordinationObject("coordinator-7", intent)
	}
	before := f.call("history", "w", "p", "", nil)
	for name, metadata := range invalid {
		t.Run(name, func(t *testing.T) {
			err := f.reject("send", "w", "p", "lead", actor("lead", map[string]any{"to": "worker", "body": "valid body", "operation_id": "bad-" + name, "coordination": metadata}))
			if strings.Contains(err.Error(), "ghp_"+strings.Repeat("a", 36)) {
				t.Fatal("metadata secret echoed")
			}
		})
	}
	after := f.call("history", "w", "p", "", nil)
	if !reflect.DeepEqual(before, after) {
		t.Fatal("metadata rejection changed audit history")
	}
	inbox := pageItems(t, "messages", f.call("inbox", "w", "p", "worker", actor("worker", nil)))
	if len(inbox) != 1 || !reflect.DeepEqual(object(t, inbox[0])["coordination"], valid) {
		t.Fatal("metadata not retained on received message")
	}
	for _, metadata := range []map[string]any{coordinationObject(" ", "progress"), coordinationObject(strings.Repeat("R", 128), "ready"), coordinationObject("rev-999", "handoff")} {
		coordinationSend(f, "worker", fmt.Sprint("valid-", len(metadata["assignment_revision"].(string))), metadata)
	}
	legacy := f.send("w", "p", "lead", "worker", "legacy body", "legacy-coordination-absent")
	if _, ok := object(t, legacy)["coordination"]; ok {
		t.Fatal("legacy message exposes coordination field")
	}
}
func TestCoordinationCorrelationAndOpaqueRevision(t *testing.T) {
	f := newFixture(t)
	f.setup("w", "p", "other")
	sent := coordinationSend(f, "worker", "correlation-root", coordinationObject("coordinator-7", "request"))
	id := stringField(t, sent, "id")
	for _, intent := range []string{"accept", "decline", "clarify"} {
		metadata := coordinationObject("coordinator-7", intent)
		metadata["request_id"] = id
		got := f.call("reply", "w", "p", "worker", actor("worker", map[string]any{"message_id": id, "body": "bounded response", "operation_id": "response-" + intent, "coordination": metadata}))
		if stringField(t, got, "reply_to") != id || !reflect.DeepEqual(object(t, got)["coordination"], metadata) {
			t.Fatal("correlated reply lost parent or metadata")
		}
	}
	for name, change := range map[string]func(map[string]any){
		"other-revision":          func(c map[string]any) { c["assignment_revision"] = "coordinator-8" },
		"higher-looking-revision": func(c map[string]any) { c["assignment_revision"] = "999999" },
		"other-issue":             func(c map[string]any) { c["issue_id"] = "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee" },
		"unknown-request":         func(c map[string]any) { c["request_id"] = "msg_missing" },
	} {
		t.Run(name, func(t *testing.T) {
			c := coordinationObject("coordinator-7", "accept")
			c["request_id"] = id
			change(c)
			f.reject("reply", "w", "p", "worker", actor("worker", map[string]any{"message_id": id, "body": "invalid context", "operation_id": name, "coordination": c}))
		})
	}
	private := coordinationSend(f, "outsider", "not-worker-visible", coordinationObject("coordinator-7", "request"))
	privateID := stringField(t, private, "id")
	c := coordinationObject("coordinator-7", "accept")
	c["request_id"] = privateID
	f.reject("send", "w", "p", "worker", actor("worker", map[string]any{"to": "lead", "body": "unrelated context", "operation_id": "not-visible", "coordination": c}))
	c["request_id"] = id
	f.reject("send", "w", "other", "worker", actor("worker", map[string]any{"to": "lead", "body": "cross-project context", "operation_id": "cross-project-context", "coordination": c}))
	// A higher-looking uncorrelated revision remains peer context, not a server
	// assignment update, metadata suppression or authorization grant.
	coordinationSend(f, "worker", "unconfirmed-progress", coordinationObject("999999", "progress"))
}
