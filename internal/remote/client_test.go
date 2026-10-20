package remote

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"clawbridge/internal/bus"
)

func TestScopedSyncOutageAndDefinitiveDenials(t *testing.T) {
	ctx := context.Background()
	var status atomic.Int64
	var foreign atomic.Bool
	principal := "alice@example.test"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer synthetic-token" {
			t.Error("missing credential")
		}
		if code := status.Load(); code != 0 {
			w.WriteHeader(int(code))
			return
		}
		var request bus.Request
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
			return
		}
		data := map[string]any{}
		switch request.Operation {
		case "resolve_project":
			data = map[string]any{"workspace_id": "ws_1", "project_id": "prj_1"}
		case "snapshot":
			owner := principal
			if foreign.Load() {
				owner = "other@example.test"
			}
			data = map[string]any{"workspace": map[string]any{"id": "ws_1"}, "project": map[string]any{"id": "prj_1"}, "session": map[string]any{"id": "ses_1", "name": "worker", "instance": "instance-1"}, "principal": owner, "verified": true, "snapshot_at": time.Now().UTC().Format(time.RFC3339Nano), "sessions": []any{map[string]any{"sequence": 1, "id": "ses_1", "name": "worker", "status": "active"}}, "messages": []any{map[string]any{"sequence": 1, "id": "msg_1", "body": "cached private message", "expires_at": time.Now().Add(time.Hour).UTC().Format(time.RFC3339Nano), "delivered_at": nil, "read_at": nil}}, "sent_messages": []any{}, "events": []any{}}
		default:
			t.Errorf("unexpected online operation: %s", request.Operation)
		}
		_ = json.NewEncoder(w).Encode(bus.Result{Source: "server", Data: data})
	}))
	defer server.Close()
	config := Config{URL: server.URL, Audience: "audience", Principal: principal, CachePath: filepath.Join(t.TempDir(), "cache.db")}
	tokens := TokenSourceFunc(func(context.Context) (string, error) { return "synthetic-token", nil })
	client, err := NewWithTokenSource(config, tokens)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = client.Sync(ctx, "team", "app", "worker", "instance-1"); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(config.CachePath)
	if err != nil {
		t.Fatal(err)
	}
	foreign.Store(true)
	if _, err = client.Sync(ctx, "team", "app", "worker", "instance-1"); err == nil {
		t.Fatal("accepted foreign-principal snapshot")
	}
	after, _ := os.ReadFile(config.CachePath)
	if !bytes.Equal(before, after) {
		t.Fatal("failed sync altered mirror")
	}
	status.Store(503)
	wid, pid, err := client.ResolveProject(ctx, "team", "app", "")
	if err != nil || wid != "ws_1" || pid != "prj_1" {
		t.Fatalf("cached resolution: %q %q %v", wid, pid, err)
	}
	request := bus.Request{Operation: "inbox", Workspace: wid, Project: pid, Session: "worker", Args: map[string]any{"instance": "instance-1"}}
	result, err := client.Execute(ctx, request)
	if err != nil || result.Source != "offline-mirror" {
		t.Fatalf("offline read: %+v %v", result, err)
	}
	encoded, _ := json.Marshal(result)
	if !bytes.Contains(encoded, []byte("cached private message")) || !bytes.Contains(encoded, []byte(`"delivered_at":null`)) {
		t.Fatal("cached inbox changed content or stamped delivery")
	}
	for _, op := range []string{"send", "reply", "mark_read", "register_session", "retire_session", "project_role_set"} {
		request.Operation = op
		if _, err = client.Execute(ctx, request); err == nil || !strings.Contains(err.Error(), "no write queued") {
			t.Fatalf("offline %s was queued/succeeded: %v", op, err)
		}
	}
	request.Operation = "inbox"
	for _, denial := range []int64{400, 401, 403, 404, 409, 429} {
		status.Store(denial)
		if _, err = client.Execute(ctx, request); err == nil {
			t.Fatalf("HTTP %d fell back to mirror", denial)
		}
	}
	status.Store(503)
	request.Args["instance"] = "different-instance"
	if _, err = client.Execute(ctx, request); err == nil {
		t.Fatal("mirror crossed instance")
	}
	request.Args["instance"] = "instance-1"
	request.Project = "different-project"
	if _, err = client.Execute(ctx, request); err == nil {
		t.Fatal("mirror crossed project")
	}
	after, _ = os.ReadFile(config.CachePath)
	if !bytes.Equal(before, after) {
		t.Fatal("offline reads/writes changed mirror bytes")
	}
}

func TestURLAndRedirectCredentialBoundary(t *testing.T) {
	tokens := TokenSourceFunc(func(context.Context) (string, error) { return "synthetic-token", nil })
	for _, u := range []string{"http://example.com", "https://user:pass@example.com", "https://example.com?token=value", "https://example.com#fragment", "file:///tmp/cache"} {
		if _, err := NewWithTokenSource(Config{URL: u, Audience: "aud"}, tokens); err == nil {
			t.Fatalf("accepted %s", u)
		}
	}
	var received atomic.Bool
	target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { received.Store(true) }))
	defer target.Close()
	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, target.URL, 307) }))
	defer redirect.Close()
	client, err := NewWithTokenSource(Config{URL: redirect.URL, Audience: "aud"}, tokens)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = client.Execute(context.Background(), bus.Request{Operation: "whoami"}); err == nil {
		t.Fatal("redirect reported success")
	}
	if received.Load() {
		t.Fatal("authenticated redirect was followed")
	}
}

func TestGcloudUsesExplicitAccountAudienceAndImpersonation(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX test executable")
	}
	dir := t.TempDir()
	capture := filepath.Join(dir, "args")
	script := "#!/bin/sh\nprintf '%s\\n' \"$@\" > \"$CLAWBRIDGE_TEST_CAPTURE\"\nprintf '%s' 'synthetic-token'\n"
	if err := os.WriteFile(filepath.Join(dir, "gcloud"), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("CLAWBRIDGE_TEST_CAPTURE", capture)
	source := &GcloudTokenSource{Account: "human@example.test", Audience: "https://bus.example.test", ImpersonateServiceAccount: "agent@example.iam.gserviceaccount.com"}
	if _, err := source.Token(context.Background()); err != nil {
		t.Fatal(err)
	}
	args, _ := os.ReadFile(capture)
	for _, want := range []string{"--account\nhuman@example.test", "--audiences\nhttps://bus.example.test", "--impersonate-service-account\nagent@example.iam.gserviceaccount.com", "--include-email"} {
		if !strings.Contains(string(args), want) {
			t.Fatalf("missing explicit routing %s", want)
		}
	}
	if _, err := (&GcloudTokenSource{Account: "human@example.test", Audience: "aud"}).Token(context.Background()); err == nil {
		t.Fatal("generic development token path accepted")
	}
}

func TestCredentialFailureDoesNotFallBackToMirror(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cache.db")
	tokens := TokenSourceFunc(func(context.Context) (string, error) {
		return "", &credentialError{errors.New("local credential unavailable")}
	})
	client, err := NewWithTokenSource(Config{
		URL:       "http://127.0.0.1:1",
		Audience:  "audience",
		Principal: "alice@example.test",
		CachePath: path,
	}, tokens)
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.Execute(context.Background(), bus.Request{Operation: "inbox", Workspace: "w", Project: "p", Session: "worker", Args: map[string]any{"instance": "instance-1"}})
	if err == nil || strings.Contains(err.Error(), "mirror") {
		t.Fatalf("credential failure attempted mirror fallback: %v", err)
	}
	var credential *credentialError
	if !errors.As(err, &credential) {
		t.Fatalf("credential error classification lost: %v", err)
	}
}
