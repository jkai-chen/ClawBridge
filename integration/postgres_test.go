package integration

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"reflect"
	"strings"
	"sync"
	"testing"

	"clawbridge/internal/bus"
)

const pgOwner = "owner@example.test"

func pgPrincipal(name string) string {
	if name == "owner" {
		return pgOwner
	}
	return name + "@example.test"
}

type pgFixture struct {
	t     *testing.T
	store *bus.Store
	w     string
}

func newPGFixture(t *testing.T) *pgFixture {
	t.Helper()
	dsn := os.Getenv("CLAWBRIDGE_TEST_POSTGRES_DSN")
	if dsn == "" {
		if os.Getenv("CLAWBRIDGE_REQUIRE_POSTGRES") == "1" {
			t.Fatal("real PostgreSQL explicitly required but CLAWBRIDGE_TEST_POSTGRES_DSN is absent")
		}
		t.Skip("real PostgreSQL not configured; run integration/run-postgres.sh")
	}
	s, err := bus.OpenPostgres(context.Background(), dsn, pgOwner)
	if err != nil {
		t.Fatalf("OpenPostgres: %v", err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Error(err)
		}
	})
	var seed [8]byte
	if _, err := rand.Read(seed[:]); err != nil {
		t.Fatal(err)
	}
	return &pgFixture{t: t, store: s, w: "review-" + hex.EncodeToString(seed[:])}
}
func (f *pgFixture) request(op, p, session string, args map[string]any) bus.Request {
	return bus.Request{Operation: op, Workspace: f.w, Project: p, Session: session, Args: args}
}
func jsonData(t *testing.T, value any) any {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	var data any
	if err = json.Unmarshal(raw, &data); err != nil {
		t.Fatal(err)
	}
	return data
}
func (f *pgFixture) call(principal, op, p, session string, args map[string]any) any {
	f.t.Helper()
	result, err := f.store.ExecuteAs(context.Background(), principal, f.request(op, p, session, args))
	if err != nil {
		f.t.Fatalf("%s principal=%s scope=%s/%s: %v", op, principal, p, session, err)
	}
	if result.Source != "server" {
		f.t.Fatalf("%s source=%q, want server", op, result.Source)
	}
	return jsonData(f.t, result.Data)
}
func (f *pgFixture) reject(principal, op, p, session string, args map[string]any) error {
	f.t.Helper()
	result, err := f.store.ExecuteAs(context.Background(), principal, f.request(op, p, session, args))
	if err == nil {
		f.t.Fatalf("%s principal=%s unexpectedly succeeded: %+v", op, principal, result)
	}
	return err
}
func (f *pgFixture) role(p, name, role string) {
	f.t.Helper()
	f.call(pgOwner, "project_role_set", p, "", map[string]any{"principal": pgPrincipal(name), "role": role})
}
func (f *pgFixture) setup(projects ...string) {
	f.t.Helper()
	f.call(pgOwner, "workspace_init", "", "", map[string]any{"mode": "server"})
	for _, p := range projects {
		f.call(pgOwner, "project_create", "", "", map[string]any{"slug": p, "git_remote": "https://example.test/" + f.w + "/" + p + ".git"})
		for _, name := range []string{"owner", "alice", "bob", "lead", "reader"} {
			if name != "owner" {
				role := "member"
				if name == "lead" {
					role = "lead"
				}
				f.role(p, name, role)
			}
			f.call(pgPrincipal(name), "register_session", p, name, map[string]any{"instance": name + "-instance", "role": "lead"})
		}
		f.role(p, "reader", "reader")
	}
}
func (f *pgFixture) send(p, from, to, body, id string) any {
	return f.call(pgPrincipal(from), "send", p, from, actor(from, map[string]any{"to": to, "body": body, "operation_id": id}))
}

func TestPostgresBootstrapAndAuthorization(t *testing.T) {
	f := newPGFixture(t)
	f.reject(pgPrincipal("alice"), "workspace_init", "", "", map[string]any{"mode": "server"})
	f.reject(pgOwner, "workspace_init", "", "", map[string]any{"mode": "local"})
	f.setup("p")
	if _, err := f.store.Execute(context.Background(), f.request("whoami", "p", "owner", actor("owner", nil))); err == nil {
		t.Fatal("Postgres trusted store accepted unverified Execute")
	}
	for _, op := range []string{"list_sessions", "history", "whoami", "inbox", "snapshot", "resolve_project"} {
		session := ""
		var args map[string]any
		if op == "inbox" || op == "snapshot" {
			session = "owner"
			args = actor("owner", nil)
		}
		f.reject("stranger@example.test", op, "p", session, args)
	}
	f.call(pgOwner, "project_create", "", "", map[string]any{"slug": "restricted"})
	for _, op := range []string{"list_sessions", "history", "whoami", "resolve_project"} {
		f.reject(pgPrincipal("alice"), op, "restricted", "", nil)
	}
	// Changing the configured bootstrap principal cannot adopt an existing workspace.
	other, err := bus.OpenPostgres(context.Background(), os.Getenv("CLAWBRIDGE_TEST_POSTGRES_DSN"), pgPrincipal("alice"))
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	if _, err := other.ExecuteAs(context.Background(), pgPrincipal("alice"), f.request("workspace_init", "", "", map[string]any{"mode": "server"})); err == nil {
		t.Fatal("changed bootstrap adopted an existing workspace")
	}

	for _, op := range []string{"send", "inbox", "whoami", "snapshot", "update_session", "retire_session"} {
		args := actor("bob", nil)
		switch op {
		case "send":
			args["to"] = "alice"
			args["body"] = "not mine"
			args["operation_id"] = "wrong-owner"
		case "update_session":
			args["notes"] = "not mine"
		}
		f.reject(pgPrincipal("alice"), op, "p", "bob", args)
	}
	// All registered sessions route as lead, but Alice is still only a project member.
	f.reject(pgPrincipal("alice"), "project_role_set", "p", "", map[string]any{"principal": "stranger@example.test", "role": "owner"})
	f.reject(pgPrincipal("alice"), "update_session", "p", "alice", actor("alice", map[string]any{"target_session": "bob", "notes": "routing is not authority"}))
	f.reject(pgPrincipal("alice"), "retire_session", "p", "alice", actor("alice", map[string]any{"target_session": "bob"}))
	f.reject(pgPrincipal("reader"), "register_session", "p", "new-reader", map[string]any{"instance": "new-reader"})
	for _, op := range []string{"send", "update_session", "retire_session"} {
		args := actor("reader", map[string]any{"notes": "forbidden"})
		if op == "send" {
			args = actor("reader", map[string]any{"to": "alice", "body": "reader cannot send", "operation_id": "reader-send"})
		}
		if op == "retire_session" {
			args = actor("reader", nil)
		}
		f.reject(pgPrincipal("reader"), op, "p", "reader", args)
	}
	f.call(pgPrincipal("reader"), "list_sessions", "p", "reader", actor("reader", nil))
	f.call(pgPrincipal("reader"), "history", "p", "reader", actor("reader", nil))
	f.call(pgPrincipal("reader"), "inbox", "p", "reader", actor("reader", nil))
	actualActor := object(t, f.call(pgPrincipal("lead"), "whoami", "p", "lead", actor("lead", nil)))["session"]
	updated := f.call(pgPrincipal("lead"), "update_session", "p", "lead", actor("lead", map[string]any{"target_session": "bob", "notes": "authorized peer change"}))
	if object(t, updated)["notes"] != "authorized peer change" {
		t.Fatalf("peer update failed: %v", updated)
	}
	events := pageItems(t, "events", f.call(pgOwner, "history", "p", "", map[string]any{"entity": "session", "key": stringField(t, updated, "id")}))
	found := false
	for _, event := range events {
		e := object(t, event)
		if e["action"] == "updated" {
			found = true
			if e["changed_by_session"] != stringField(t, actualActor, "id") {
				t.Errorf("peer audit attributes target instead of actor: %v", e)
			}
			if e["changed_by_principal"] != pgPrincipal("lead") {
				t.Fatalf("peer change falsely attributed: %v", e)
			}
		}
	}
	if !found {
		t.Fatal("peer change missing audit")
	}
}

func TestPostgresSnapshotPrivacyAndScope(t *testing.T) {
	f := newPGFixture(t)
	f.setup("p", "other")
	received := f.send("p", "alice", "bob", "bob-only-private-body", "for-bob")
	unrelated := f.send("p", "alice", "reader", "reader-only-private-body", "for-reader")
	ownSent := f.send("p", "bob", "alice", "own-sent-body-should-be-omitted", "from-bob")
	foreign := f.send("other", "alice", "bob", "foreign-project-private-body", "foreign")
	before := f.call(pgPrincipal("alice"), "message_status", "p", "alice", actor("alice", map[string]any{"message_id": stringField(t, received, "id")}))
	snapshot := object(t, f.call(pgPrincipal("bob"), "snapshot", "p", "bob", actor("bob", nil)))
	if snapshot["verified"] != true || snapshot["principal"] != pgPrincipal("bob") || snapshot["storage_mode"] != "cloud" || snapshot["identity_provenance"] != "google-id-token" {
		t.Fatalf("bad snapshot identity: %v", snapshot)
	}
	messages := array(t, snapshot["messages"])
	if len(messages) != 1 || stringField(t, messages[0], "id") != stringField(t, received, "id") {
		t.Fatalf("snapshot received set wrong: %v", messages)
	}
	raw, _ := json.Marshal(snapshot)
	for _, peer := range []string{"owner", "alice", "lead", "reader"} {
		if strings.Contains(string(raw), peer+"-instance") {
			t.Errorf("snapshot exports another principal's instance: %s", peer)
		}
	}
	for _, secret := range []string{"reader-only-private-body", "foreign-project-private-body", "own-sent-body-should-be-omitted", stringField(t, unrelated, "id"), stringField(t, foreign, "id")} {
		// Audit may reference other message IDs, but must never include their bodies.
		if strings.HasPrefix(secret, "msg_") {
			continue
		}
		if strings.Contains(string(raw), secret) {
			t.Errorf("snapshot leaked %q", secret)
		}
	}
	sent := array(t, snapshot["sent_messages"])
	if len(sent) != 1 || stringField(t, sent[0], "id") != stringField(t, ownSent, "id") {
		t.Fatalf("sent metadata wrong: %v", sent)
	}
	if _, has := object(t, sent[0])["body"]; has {
		t.Fatal("snapshot sent metadata includes body")
	}
	after := f.call(pgPrincipal("alice"), "message_status", "p", "alice", actor("alice", map[string]any{"message_id": stringField(t, received, "id")}))
	if !reflect.DeepEqual(before, after) {
		t.Fatalf("snapshot stamped delivery/read: before=%v after=%v", before, after)
	}
	for _, op := range []string{"message_status", "reply", "mark_read"} {
		args := actor("bob", map[string]any{"message_id": stringField(t, foreign, "id")})
		if op == "reply" {
			args["body"] = "foreign reply"
			args["operation_id"] = "foreign-reply"
		}
		if op == "mark_read" {
			args = actor("bob", map[string]any{"ids": []string{stringField(t, foreign, "id")}})
		}
		f.reject(pgPrincipal("bob"), op, "p", "bob", args)
	}
}

func TestPostgresConcurrentPrincipalsAndReplay(t *testing.T) {
	f := newPGFixture(t)
	f.setup("p")
	const writers = 4
	const each = 8
	var wg sync.WaitGroup
	errs := make(chan error, writers*each)
	results := make(chan bus.Result, writers*each)
	for worker := 0; worker < writers; worker++ {
		wg.Add(1)
		go func(worker int) {
			defer wg.Done()
			name := "alice"
			if worker%2 == 1 {
				name = "bob"
			}
			for i := 0; i < each; i++ {
				token := fmt.Sprintf("%d-%d", worker, i)
				r, e := f.store.ExecuteAs(context.Background(), pgPrincipal(name), f.request("send", "p", name, actor(name, map[string]any{"to": "owner", "body": token, "operation_id": token})))
				if e != nil {
					errs <- e
				} else {
					results <- r
				}
			}
		}(worker)
	}
	wg.Wait()
	close(errs)
	close(results)
	for e := range errs {
		t.Error(e)
	}
	ids := map[string]bool{}
	for r := range results {
		m := object(t, jsonData(t, r.Data))
		id := stringField(t, m, "id")
		if ids[id] {
			t.Error("duplicate message ID")
		}
		ids[id] = true
		principal := m["created_by_principal"]
		var worker, index int
		if _, err := fmt.Sscanf(stringField(t, m, "body"), "%d-%d", &worker, &index); err != nil {
			t.Fatal(err)
		}
		expected := pgPrincipal("alice")
		if worker%2 == 1 {
			expected = pgPrincipal("bob")
		}
		if principal != expected {
			t.Errorf("principal bleed: body from %s attributed to %v", expected, principal)
		}
	}
	if len(ids) != writers*each {
		t.Fatalf("lost sends: %d", len(ids))
	}
	inbox := pageItems(t, "messages", f.call(pgOwner, "inbox", "p", "owner", actor("owner", map[string]any{"limit": 1000})))
	if len(inbox) != writers*each {
		t.Fatalf("lost deliveries: %d", len(inbox))
	}
	// Transactional conflict resolution must also deduplicate simultaneous retries.
	var retry sync.WaitGroup
	retryResults := make(chan bus.Result, 8)
	retryErrors := make(chan error, 8)
	for i := 0; i < 8; i++ {
		retry.Add(1)
		go func() {
			defer retry.Done()
			r, e := f.store.ExecuteAs(context.Background(), pgPrincipal("alice"), f.request("send", "p", "alice", actor("alice", map[string]any{"to": "bob", "body": "one concurrent operation", "operation_id": "same"})))
			if e != nil {
				retryErrors <- e
			} else {
				retryResults <- r
			}
		}()
	}
	retry.Wait()
	close(retryResults)
	close(retryErrors)
	for e := range retryErrors {
		t.Error(e)
	}
	one := map[string]bool{}
	count := 0
	for r := range retryResults {
		count++
		one[stringField(t, jsonData(t, r.Data), "id")] = true
	}
	if count != 8 || len(one) != 1 {
		t.Fatalf("concurrent replay successful calls=%d distinct IDs=%d", count, len(one))
	}
	f.reject(pgPrincipal("alice"), "send", "p", "alice", actor("alice", map[string]any{"to": "bob", "body": "changed", "operation_id": "same"}))
}

func TestPostgresWorkspaceIsolationAndResume(t *testing.T) {
	first := newPGFixture(t)
	first.setup("p")
	second := newPGFixture(t)
	second.setup("p")
	a := first.send("p", "alice", "bob", "first-workspace-body", "workspace-send")
	b := second.send("p", "alice", "bob", "second-workspace-body", "workspace-send")
	for _, fixture := range []*pgFixture{first, second} {
		foreign := b
		expected := "first-workspace-body"
		if fixture == second {
			foreign = a
			expected = "second-workspace-body"
		}
		inbox := pageItems(t, "messages", fixture.call(pgPrincipal("bob"), "inbox", "p", "bob", actor("bob", nil)))
		if len(inbox) != 1 || object(t, inbox[0])["body"] != expected {
			t.Fatalf("workspace mailbox crossed: %v", inbox)
		}
		id := stringField(t, foreign, "id")
		fixture.reject(pgPrincipal("alice"), "message_status", "p", "alice", actor("alice", map[string]any{"message_id": id}))
		fixture.reject(pgPrincipal("bob"), "mark_read", "p", "bob", actor("bob", map[string]any{"ids": []string{id}}))
		fixture.reject(pgPrincipal("bob"), "reply", "p", "bob", actor("bob", map[string]any{"message_id": id, "body": "foreign", "operation_id": "foreign"}))
		fixture.reject(pgPrincipal("alice"), "send", "p", "alice", actor("alice", map[string]any{"to": "bob", "body": "foreign", "operation_id": "foreign-thread", "thread_id": id}))
	}
	first.role("p", "only-first", "member")
	resolve := bus.Request{Operation: "resolve_project", Project: "p"}
	if _, err := first.store.ExecuteAs(context.Background(), pgPrincipal("alice"), resolve); err == nil {
		t.Fatal("ambiguous authorized slug silently selected a workspace")
	}
	only, err := first.store.ExecuteAs(context.Background(), pgPrincipal("only-first"), resolve)
	if err != nil {
		t.Fatal(err)
	}
	selected := object(t, jsonData(t, only.Data))
	if selected["workspace_id"] != object(t, object(t, first.call(pgOwner, "whoami", "p", "", nil))["workspace"])["id"] {
		t.Fatal("resolution included unauthorized workspace")
	}
	resumed := first.call(pgPrincipal("alice"), "register_session", "p", "alice", map[string]any{"instance": "alice-instance", "resume": true})
	instance := stringField(t, resumed, "instance")
	if instance == "alice-instance" {
		t.Fatal("resume did not rotate instance")
	}
	for _, op := range []string{"inbox", "snapshot", "whoami", "list_sessions", "history"} {
		first.reject(pgPrincipal("alice"), op, "p", "alice", actor("alice", nil))
	}
	replay := first.call(pgPrincipal("alice"), "send", "p", "alice", map[string]any{"instance": instance, "to": "bob", "body": "first-workspace-body", "operation_id": "workspace-send"})
	if stringField(t, replay, "id") != stringField(t, a, "id") {
		t.Fatal("resume lost immutable retry identity")
	}
	first.call(pgPrincipal("alice"), "inbox", "p", "alice", map[string]any{"instance": instance})
}
