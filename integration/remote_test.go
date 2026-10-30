package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"clawbridge/internal/bus"
	"clawbridge/internal/remote"
	"clawbridge/internal/server"
)

func testToken() remote.TokenSource {
	return remote.TokenSourceFunc(func(context.Context) (string, error) { return "synthetic-bob", nil })
}
func fixtureVerifier() server.Verifier {
	return server.VerifierFunc(func(_ context.Context, token string) (server.Identity, error) {
		if token != "synthetic-bob" {
			return server.Identity{}, errors.New("test token rejected")
		}
		return server.Identity{Principal: pgPrincipal("bob"), Subject: pgSubject(pgPrincipal("bob"))}, nil
	})
}
func mustRemote(t *testing.T, c remote.Config) *remote.Client {
	t.Helper()
	client, err := remote.NewWithTokenSource(c, testToken())
	if err != nil {
		t.Fatal(err)
	}
	return client
}

func TestPostgresRemoteSyncOfflineAndDenial(t *testing.T) {
	f := newPGFixture(t)
	f.setup("p", "other")
	sent := f.send("p", "alice", "bob", "only-bob-snapshot-body", "to-bob")
	f.send("p", "alice", "reader", "never-bob-snapshot-body", "to-reader")
	own := f.send("p", "bob", "alice", "own-body-not-cached", "bob-sent")
	var mode atomic.Int32
	base := server.NewHandler(f.store, fixtureVerifier())
	endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch mode.Load() {
		case 1:
			w.WriteHeader(503)
			io.WriteString(w, `{"error":{"code":"unavailable"}}`)
			return
		case 2:
			w.WriteHeader(401)
			io.WriteString(w, `{"error":{"code":"unauthenticated"}}`)
			return
		case 3:
			w.WriteHeader(403)
			io.WriteString(w, `{"error":{"code":"forbidden"}}`)
			return
		case 4:
			raw, _ := io.ReadAll(r.Body)
			r.Body = io.NopCloser(bytes.NewReader(raw))
			var request bus.Request
			json.Unmarshal(raw, &request)
			if request.Operation == "snapshot" {
				io.WriteString(w, `{"source":"server","data":{"principal":"wrong@example.test","verified":true}}`)
				return
			}
		}
		base.ServeHTTP(w, r)
	}))
	defer endpoint.Close()
	config := remote.Config{URL: endpoint.URL, Audience: "https://bus.example.test", Principal: pgPrincipal("bob"), CachePath: filepath.Join(t.TempDir(), "mirror.db")}
	client := mustRemote(t, config)
	ctx := context.Background()
	wid, pid, err := client.ResolveProject(ctx, f.w, "p", "")
	if err != nil {
		t.Fatal(err)
	}
	summary, err := client.Sync(ctx, f.w, "p", "bob", "bob-instance")
	if err != nil {
		t.Fatal(err)
	}
	if summary.Source != "server" {
		t.Fatalf("sync source %q", summary.Source)
	}
	cachedBefore, err := os.ReadFile(config.CachePath)
	if err != nil {
		t.Fatal(err)
	}
	status := onlyDelivery(t, f.call(pgPrincipal("alice"), "message_status", "p", "alice", actor("alice", map[string]any{"message_id": stringField(t, sent, "id")})))
	if status["delivered_at"] != nil || status["read_at"] != nil {
		t.Fatalf("sync stamped delivery: %v", status)
	}
	request := bus.Request{Operation: "inbox", Workspace: wid, Project: pid, Session: "bob", Args: actor("bob", nil)}
	mode.Store(1)
	offline, err := client.Execute(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	if offline.Source != "offline-mirror" {
		t.Fatalf("outage source %q", offline.Source)
	}
	data := object(t, jsonData(t, offline.Data))
	if object(t, data["mirror"])["updated_at"] == nil {
		t.Fatalf("missing freshness: %v", data)
	}
	messages := pageItems(t, "messages", data)
	if len(messages) != 1 || object(t, messages[0])["body"] != "only-bob-snapshot-body" {
		t.Fatalf("mirror inbox leaked/lost bodies: %v", messages)
	}
	if object(t, messages[0])["delivered_at"] != nil || object(t, messages[0])["read_at"] != nil {
		t.Fatal("offline inbox stamped delivery")
	}
	for _, op := range []string{"list_sessions", "history", "whoami", "message_status"} {
		r := request
		r.Operation = op
		r.Args = actor("bob", nil)
		if op == "message_status" {
			r.Args["message_id"] = stringField(t, own, "id")
		}
		result, err := client.Execute(ctx, r)
		if err != nil || result.Source != "offline-mirror" {
			t.Fatalf("offline %s: %v %+v", op, err, result)
		}
	}
	for _, op := range []string{"send", "reply", "mark_read", "register_session", "update_session", "retire_session"} {
		r := request
		r.Operation = op
		r.Args = actor("bob", nil)
		switch op {
		case "send":
			r.Args["to"] = "alice"
			r.Args["body"] = "offline-must-not-queue"
			r.Args["operation_id"] = "offline-send"
		case "reply":
			r.Args["message_id"] = stringField(t, sent, "id")
			r.Args["body"] = "offline-must-not-queue"
			r.Args["operation_id"] = "offline-reply"
		case "mark_read":
			r.Args["ids"] = []string{stringField(t, sent, "id")}
		case "register_session":
			r.Args["resume"] = true
		case "update_session":
			r.Args["notes"] = "offline-must-not-queue"
		case "retire_session":
			r.Args["successor"] = "alice"
		}
		if op == "send" || op == "reply" || op == "mark_read" {
			// A message write waits in the client's outbox (ZERO-LOSS-DEPLOY
			// §1); the call's deadline ends its in-process retries.
			bounded, cancel := context.WithTimeout(ctx, 1500*time.Millisecond)
			result, err := client.Execute(bounded, r)
			cancel()
			if data, _ := result.Data.(map[string]any); err != nil || result.Source != "client_outbox" || data["outcome"] != "queued_local" {
				t.Errorf("offline %s wasn't queued locally: %v %+v", op, err, result)
			}
			continue
		}
		if _, err := client.Execute(ctx, r); err == nil {
			t.Errorf("offline write %s succeeded", op)
		}
	}
	for _, reject := range []int32{2, 3} {
		mode.Store(reject)
		if _, err := client.Execute(ctx, request); err == nil {
			t.Fatalf("HTTP auth denial %d silently used mirror", reject)
		}
		if _, _, err := client.ResolveProject(ctx, f.w, "p", ""); err == nil {
			t.Fatal("denied resolution used mirror")
		}
	}
	mode.Store(4)
	if _, err := client.Sync(ctx, f.w, "p", "bob", "bob-instance"); err == nil {
		t.Fatal("mismatched snapshot published")
	}
	cachedAfter, _ := os.ReadFile(config.CachePath)
	if !bytes.Equal(cachedBefore, cachedAfter) {
		t.Fatal("failed sync/offline read changed complete mirror bytes")
	}
	mode.Store(1)
	if _, err := client.Execute(ctx, request); err != nil {
		t.Fatalf("failed sync destroyed prior snapshot: %v", err)
	}
	for _, change := range []string{"principal", "audience", "server", "workspace", "project", "session", "instance"} {
		t.Run("binding-"+change, func(t *testing.T) {
			c := config
			r := request
			r.Args = actor("bob", nil)
			switch change {
			case "principal":
				c.Principal = pgPrincipal("alice")
			case "audience":
				c.Audience = "different"
			case "server":
				c.URL = endpoint.URL + "/different"
			case "workspace":
				r.Workspace = "different"
			case "project":
				r.Project = "different"
			case "session":
				r.Session = "alice"
			case "instance":
				r.Args["instance"] = "different"
			}
			if _, err := mustRemote(t, c).Execute(ctx, r); err == nil {
				t.Fatal("mismatched binding read another cached scope")
			}
		})
	}
	mode.Store(0)
	online, err := client.Execute(ctx, request)
	if err != nil || online.Source != "server" {
		t.Fatalf("recovery failed: %v", err)
	}
	delivered := onlyDelivery(t, f.call(pgPrincipal("alice"), "message_status", "p", "alice", actor("alice", map[string]any{"message_id": stringField(t, sent, "id")})))
	if delivered["delivered_at"] == nil || delivered["read_at"] != nil {
		t.Fatal("online recovery delivery/read distinction lost")
	}
}

func TestPostgresRemoteResolutionCannotMixAliases(t *testing.T) {
	f := newPGFixture(t)
	f.setup("first", "second")
	endpoint := httptest.NewServer(server.NewHandler(f.store, fixtureVerifier()))
	defer endpoint.Close()
	client := mustRemote(t, remote.Config{URL: endpoint.URL, Audience: "https://bus.example.test", Principal: pgPrincipal("bob"), CachePath: filepath.Join(t.TempDir(), "mirror.db")})
	ctx := context.Background()
	_, first, err := client.ResolveProject(ctx, f.w, "first", "")
	if err != nil {
		t.Fatal(err)
	}
	_, second, err := client.ResolveProject(ctx, f.w, "second", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = client.Sync(ctx, f.w, "second", "bob", "bob-instance"); err != nil {
		t.Fatal(err)
	}
	endpoint.Close()
	_, resolved, err := client.ResolveProject(ctx, f.w, "first", "")
	if err == nil && resolved != first {
		t.Fatalf("offline alias for first resolved second project: got=%s first=%s second=%s", resolved, first, second)
	}
}

func TestRemoteURLAndRedirectBoundary(t *testing.T) {
	for _, url := range []string{"http://example.test", "http://192.0.2.1", "https://user:password@example.test", "https://example.test?secret=value", "ftp://example.test"} {
		if _, err := remote.NewWithTokenSource(remote.Config{URL: url, Audience: "audience"}, testToken()); err == nil {
			t.Errorf("unsafe URL accepted: %s", url)
		}
	}
	var forwarded atomic.Int64
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { forwarded.Add(1); w.WriteHeader(200) }))
	defer target.Close()
	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, target.URL+"/v1/execute", 307) }))
	defer redirect.Close()
	client := mustRemote(t, remote.Config{URL: redirect.URL, Audience: "audience"})
	if _, err := client.Execute(context.Background(), bus.Request{Operation: "whoami"}); err == nil {
		t.Fatal("authenticated redirect accepted")
	}
	if forwarded.Load() != 0 {
		t.Fatal("bearer-bearing request followed redirect")
	}
	// Failure messages must not echo bearer or remote response secrets.
	noisy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(403)
		io.WriteString(w, `{"error":{"code":"forbidden","message":"synthetic-bob postgres://secret"}}`)
	}))
	defer noisy.Close()
	_, err := mustRemote(t, remote.Config{URL: noisy.URL, Audience: "aud"}).Execute(context.Background(), bus.Request{Operation: "whoami"})
	if err == nil || strings.Contains(err.Error(), "synthetic-bob") || strings.Contains(err.Error(), "postgres://") {
		t.Fatalf("unsafe client error: %v", err)
	}
}
