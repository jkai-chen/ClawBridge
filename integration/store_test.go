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

func (f *fixture) setup(workspace string, projects ...string) {
	f.t.Helper()
	f.call("workspace_init", workspace, "", "", map[string]any{"mode": "local"})
	for _, project := range projects {
		f.call("project_create", workspace, "", "", map[string]any{"slug": project})
		for _, name := range []string{"lead", "worker", "outsider"} {
			f.call("register_session", workspace, project, name, map[string]any{"instance": name + "-instance", "role": name})
		}
	}
}
func actor(session string, args map[string]any) map[string]any {
	out := map[string]any{"instance": session + "-instance"}
	for key, value := range args {
		out[key] = value
	}
	return out
}
func (f *fixture) send(w, p, from, to, body, operation string) any {
	f.t.Helper()
	return f.call("send", w, p, from, actor(from, map[string]any{"to": to, "body": body, "operation_id": operation}))
}

func TestRegistrationIdentityAndCallerOverrides(t *testing.T) {
	f := newFixture(t)
	f.setup("w", "p")
	first := f.call("register_session", "w", "p", "lead", map[string]any{"instance": "lead-instance", "role": "lead"})
	again := f.call("register_session", "w", "p", "lead", map[string]any{"instance": "lead-instance", "role": "lead"})
	if !reflect.DeepEqual(first, again) {
		t.Fatalf("identical registration changed identity: %v vs %v", first, again)
	}
	f.reject("register_session", "w", "p", "lead", map[string]any{"instance": "other-process"})
	f.reject("send", "w", "p", "lead", map[string]any{"to": "worker", "body": "hello", "operation_id": "missing-instance"})
	f.reject("send", "w", "p", "lead", map[string]any{"instance": "other-process", "to": "worker", "body": "hello", "operation_id": "wrong-instance"})
	for _, override := range []string{"created_at", "updated_at", "principal", "owner_principal", "read_at"} {
		t.Run(override, func(t *testing.T) {
			f.reject("send", "w", "p", "lead", actor("lead", map[string]any{"to": "worker", "body": "hello", "operation_id": "override-" + override, override: "2000-01-01T00:00:00Z"}))
		})
	}
	for _, op := range []string{"update_session", "retire_session"} {
		f.reject(op, "w", "p", "lead", actor("lead", map[string]any{"session": "worker", "notes": "peer mutation"}))
	}
}

func TestScopedMessageIDsAndReplay(t *testing.T) {
	f := newFixture(t)
	f.setup("w1", "p1", "p2")
	f.setup("w2", "p1")
	sent := f.send("w1", "p1", "lead", "worker", "private sentinel", "retry-1")
	id := stringField(t, sent, "id")
	replay := f.send("w1", "p1", "lead", "worker", "private sentinel", "retry-1")
	if !reflect.DeepEqual(sent, replay) {
		t.Fatalf("replay changed result: %v vs %v", sent, replay)
	}
	for _, change := range []map[string]any{{"body": "changed"}, {"to": "outsider"}, {"kind": "decision"}, {"thread_id": id}} {
		args := actor("lead", map[string]any{"to": "worker", "body": "private sentinel", "operation_id": "retry-1"})
		for key, value := range change {
			args[key] = value
		}
		f.reject("send", "w1", "p1", "lead", args)
	}
	scopes := []struct{ w, p string }{{"w1", "p2"}, {"w2", "p1"}}
	for _, scope := range scopes {
		t.Run(scope.w+"/"+scope.p, func(t *testing.T) {
			f.reject("message_status", scope.w, scope.p, "lead", actor("lead", map[string]any{"message_id": id}))
			f.reject("mark_read", scope.w, scope.p, "worker", actor("worker", map[string]any{"ids": []string{id}}))
			f.reject("reply", scope.w, scope.p, "worker", actor("worker", map[string]any{"message_id": id, "body": "foreign reply", "operation_id": "foreign"}))
			f.reject("send", scope.w, scope.p, "lead", actor("lead", map[string]any{"to": "worker", "body": "foreign thread", "operation_id": "foreign-thread", "thread_id": id}))
			for _, op := range []string{"inbox", "history"} {
				args := actor("worker", nil)
				if op == "history" {
					args = nil
				}
				data := f.call(op, scope.w, scope.p, "worker", args)
				raw, _ := json.Marshal(data)
				if strings.Contains(string(raw), id) || strings.Contains(string(raw), "private sentinel") {
					t.Fatalf("%s leaks foreign message: %s", op, raw)
				}
			}
		})
	}
	f.reject("message_status", "w1", "p1", "outsider", actor("outsider", map[string]any{"message_id": id}))
	f.reject("mark_read", "w1", "p1", "outsider", actor("outsider", map[string]any{"ids": []string{id}}))
	f.reject("reply", "w1", "p1", "outsider", actor("outsider", map[string]any{"message_id": id, "body": "not participant", "operation_id": "unrelated"}))
	f.reject("send", "w1", "p1", "lead", actor("lead", map[string]any{"to": "p2/worker", "body": "cross", "operation_id": "cross"}))
}

func TestSecretRejectionAndAtomicFailure(t *testing.T) {
	f := newFixture(t)
	f.setup("w", "p")
	before := f.call("history", "w", "p", "lead", nil)
	secrets := []string{
		"-----BEGIN PRIVATE KEY-----\nsynthetic-data\n-----END PRIVATE KEY-----",
		"ghp_" + strings.Repeat("a", 36),
		"AKIA" + strings.Repeat("A", 16),
		"sk-proj-" + strings.Repeat("a", 80),
	}
	for i, secret := range secrets {
		err := f.reject("send", "w", "p", "lead", actor("lead", map[string]any{"to": "worker", "body": secret, "operation_id": fmt.Sprint("secret-", i)}))
		if strings.Contains(err.Error(), secret) {
			t.Errorf("error echoes rejected synthetic secret")
		}
	}
	f.reject("send", "w", "p", "lead", actor("lead", map[string]any{"to": "missing", "body": "invalid destination", "operation_id": "bad-destination"}))
	after := f.call("history", "w", "p", "lead", nil)
	if !reflect.DeepEqual(before, after) {
		t.Fatalf("rejected sends changed audit history: before=%v after=%v", before, after)
	}
	f.send("w", "p", "lead", "worker", "Use os.Getenv(\"API_KEY\") rather than embedding credentials.", "ordinary-code")
}

func TestRetirementAndSuccessors(t *testing.T) {
	f := newFixture(t)
	f.setup("w", "p", "other")
	for _, successor := range []string{"lead", "missing", "other/worker"} {
		f.reject("retire_session", "w", "p", "lead", actor("lead", map[string]any{"successor": successor}))
	}
	f.call("retire_session", "w", "p", "lead", actor("lead", map[string]any{"successor": "worker"}))
	f.reject("send", "w", "p", "lead", actor("lead", map[string]any{"to": "worker", "body": "retired sender", "operation_id": "retired-from"}))
	f.reject("send", "w", "p", "worker", actor("worker", map[string]any{"to": "lead", "body": "retired recipient", "operation_id": "retired-to"}))
	f.reject("retire_session", "w", "p", "worker", actor("worker", map[string]any{"successor": "lead"}))
	f.reject("register_session", "w", "p", "lead", map[string]any{"instance": "replacement-process"})
}

func TestConcurrentIndependentStores(t *testing.T) {
	f := newFixture(t)
	f.setup("w", "p")
	const writers = 6
	const perWriter = 8
	stores := make([]*bus.Store, writers)
	for i := range stores {
		s, err := bus.Open(f.path)
		if err != nil {
			t.Fatal(err)
		}
		stores[i] = s
		t.Cleanup(func() {
			if err := s.Close(); err != nil {
				t.Error(err)
			}
		})
	}
	var wg sync.WaitGroup
	errs := make(chan error, writers*perWriter)
	ids := make(chan string, writers*perWriter)
	for worker, s := range stores {
		wg.Add(1)
		go func(worker int, s *bus.Store) {
			defer wg.Done()
			for n := 0; n < perWriter; n++ {
				token := fmt.Sprintf("writer-%d-message-%d", worker, n)
				result, err := s.Execute(context.Background(), f.request("send", "w", "p", "lead", actor("lead", map[string]any{"to": "worker", "body": token, "operation_id": token})))
				if err != nil {
					errs <- fmt.Errorf("%s: %w", token, err)
					continue
				}
				if result.Source != "local-authority" {
					errs <- fmt.Errorf("%s source %q", token, result.Source)
					continue
				}
				raw, _ := json.Marshal(result.Data)
				var data map[string]any
				if err := json.Unmarshal(raw, &data); err != nil {
					errs <- err
					continue
				}
				id, ok := data["id"].(string)
				if !ok || id == "" {
					errs <- fmt.Errorf("%s missing id: %s", token, raw)
					continue
				}
				ids <- id
			}
		}(worker, s)
	}
	wg.Wait()
	close(errs)
	close(ids)
	for err := range errs {
		t.Error(err)
	}
	seen := map[string]bool{}
	for id := range ids {
		if seen[id] {
			t.Errorf("duplicate ID %s", id)
		}
		seen[id] = true
	}
	if len(seen) != writers*perWriter {
		t.Fatalf("got %d unique successful messages, want %d", len(seen), writers*perWriter)
	}
	inbox := pageItems(t, "messages", f.call("inbox", "w", "p", "worker", actor("worker", map[string]any{"limit": 1000})))
	if len(inbox) != writers*perWriter {
		t.Fatalf("got %d deliveries, want %d", len(inbox), writers*perWriter)
	}
	bodies := map[string]bool{}
	for _, item := range inbox {
		id := stringField(t, item, "id")
		if !seen[id] {
			t.Errorf("unexpected delivery %s", id)
		}
		delete(seen, id)
		body := stringField(t, item, "body")
		if bodies[body] {
			t.Errorf("duplicate delivery body %s", body)
		}
		bodies[body] = true
	}
	if len(seen) != 0 {
		t.Errorf("successful sends missing deliveries: %v", seen)
	}
}

func TestConcurrentIdenticalOperationReplay(t *testing.T) {
	f := newFixture(t)
	f.setup("w", "p")
	const contenders = 8
	stores := make([]*bus.Store, contenders)
	for i := range stores {
		s, err := bus.Open(f.path)
		if err != nil {
			t.Fatal(err)
		}
		stores[i] = s
		t.Cleanup(func() {
			if err := s.Close(); err != nil {
				t.Error(err)
			}
		})
	}
	var wg sync.WaitGroup
	results := make(chan bus.Result, contenders)
	errs := make(chan error, contenders)
	for _, store := range stores {
		wg.Add(1)
		go func(s *bus.Store) {
			defer wg.Done()
			result, err := s.Execute(context.Background(), f.request("send", "w", "p", "lead", actor("lead", map[string]any{"to": "worker", "body": "one logical send", "operation_id": "shared-operation"})))
			if err != nil {
				errs <- err
				return
			}
			results <- result
		}(store)
	}
	wg.Wait()
	close(results)
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	ids := map[string]bool{}
	count := 0
	for result := range results {
		count++
		raw, _ := json.Marshal(result.Data)
		var data map[string]any
		if err := json.Unmarshal(raw, &data); err != nil {
			t.Fatal(err)
		}
		ids[stringField(t, data, "id")] = true
	}
	if count != contenders || len(ids) != 1 {
		t.Fatalf("safe retries produced %d successful calls and %d distinct messages", count, len(ids))
	}
	inbox := pageItems(t, "messages", f.call("inbox", "w", "p", "worker", actor("worker", nil)))
	if len(inbox) != 1 || !ids[stringField(t, inbox[0], "id")] {
		t.Fatalf("replay allocated duplicate/missing delivery: %v", inbox)
	}
	events := pageItems(t, "events", f.call("history", "w", "p", "", map[string]any{"entity": "message"}))
	if len(events) != 1 {
		t.Fatalf("replay appended %d message audit events", len(events))
	}
}
