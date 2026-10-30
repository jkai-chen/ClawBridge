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
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"clawbridge/internal/bus"
	"clawbridge/internal/mirror"
)

// An issue's history is a read the server makes as the app (FR21): a
// server that is unavailable for it, or can't be reached, never reads as a
// failed write, and nothing is mirrored.
func TestRemoteIssueHistoryUnavailable(t *testing.T) {
	ctx := context.Background()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer server.Close()
	config := Config{URL: server.URL, Audience: "audience", Principal: "alice@example.test", CachePath: filepath.Join(t.TempDir(), "cache.db")}
	client, err := NewWithTokenSource(config, TokenSourceFunc(func(context.Context) (string, error) { return "synthetic-token", nil }))
	if err != nil {
		t.Fatal(err)
	}
	history := bus.Request{Operation: "linear_issue_history", Workspace: "ws_1", Project: "prj_1", Args: map[string]any{"issue_uuid": "d7000000-0000-4000-8000-0000000000d1"}}
	_, err = client.Execute(ctx, history)
	var httpErr *HTTPError
	if err == nil || !strings.HasPrefix(err.Error(), "the issue's history could not be read: the ClawBridge server could not be reached or could not read Linear; nothing was written; retry") || strings.Contains(err.Error(), "cloud write failed") || !errors.As(err, &httpErr) || httpErr.Status != 503 {
		t.Fatalf("unavailable history: %v", err)
	}
	server.Close()
	if _, err = client.Execute(ctx, history); err == nil || !strings.HasPrefix(err.Error(), "the issue's history could not be read") {
		t.Fatalf("unreachable history: %v", err)
	}
}

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
	// With the outbox switched off, no write waits (outbox_test.go has it on).
	t.Setenv("CLAWBRIDGE_CLIENT_OUTBOX", "off")
	for _, op := range []string{"send", "reply", "mark_read", "register_session", "retire_session", "project_role_set"} {
		request.Operation = op
		if _, err = client.Execute(ctx, request); err == nil || !strings.Contains(err.Error(), "no write queued") {
			t.Fatalf("offline %s was queued/succeeded: %v", op, err)
		}
	}
	// The project overview is read from the authority only: offline it says
	// so, and never reads as a failed write.
	overview := bus.Request{Operation: "project_overview", Workspace: wid, Project: pid, Args: map[string]any{}}
	if _, err = client.Execute(ctx, overview); !errors.Is(err, ErrOverviewOffline) {
		t.Fatalf("offline overview: %v", err)
	}
	// So are the claim markers.
	markers := bus.Request{Operation: "marker_status", Workspace: wid, Project: pid, Args: map[string]any{"issue_uuid": "00000000-0000-4000-8000-000000000001"}}
	if _, err = client.Execute(ctx, markers); !errors.Is(err, ErrMarkersOffline) {
		t.Fatalf("offline markers: %v", err)
	}
	// The project's threads are read from the authority only: offline it
	// says so, and never reads as a failed write.
	for _, op := range []string{"project_threads", "project_thread"} {
		threads := bus.Request{Operation: op, Workspace: wid, Project: pid, Args: map[string]any{}}
		if _, err = client.Execute(ctx, threads); !errors.Is(err, ErrThreadsOffline) {
			t.Fatalf("offline %s: %v", op, err)
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

// v1.0 removed Google sign-in: a route that still names oauth, gcloud or
// adc, or none (which was gcloud), is refused with how to join instead.
func TestGoogleRoutesRemoved(t *testing.T) {
	for _, source := range []string{"", "oauth", "gcloud", "adc", "metadata"} {
		c := Config{URL: "https://cb.example.test", Principal: "alice@example.test", CachePath: filepath.Join(t.TempDir(), "cache.db"), CredentialSource: source}
		if _, err := ValidateRoute(c); err == nil || err.Error() != GoogleRouteRemoved {
			t.Errorf("ValidateRoute %q: %v", source, err)
		}
		if _, err := New(c); err == nil || !strings.Contains(err.Error(), GoogleRouteRemoved) {
			t.Errorf("New %q: %v", source, err)
		}
	}
	if GoogleRouteRemoved != "this profile signs in with Google, which was removed in v1.0: ask an owner for an invite (or a recovery invite) and run clawbridge join; see docs/operations/google-sign-in-removal.md" {
		t.Fatal(GoogleRouteRemoved)
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

func TestPlainTokenSourceFailureCannotReadMatchingMirror(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cache.db")
	config := Config{
		URL:       "http://127.0.0.1:1",
		Audience:  "audience",
		Principal: "alice@example.test",
		CachePath: path,
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	err := mirror.Save(context.Background(), path, mirror.Snapshot{
		Binding: mirror.Binding{
			Server: config.URL, Audience: config.Audience, Principal: config.Principal,
			Workspace: "w", Project: "p", Session: "worker", Instance: "instance-1",
		},
		UpdatedAt: now,
		Data: map[string]any{
			"workspace": map[string]any{"id": "w"}, "project": map[string]any{"id": "p"},
			"session":   map[string]any{"id": "worker", "name": "worker", "instance": "instance-1"},
			"principal": config.Principal, "verified": true, "storage_mode": "cloud",
			"identity_provenance": "google-id-token", "snapshot_at": now,
			"sessions":      []any{map[string]any{"sequence": 1, "id": "worker", "name": "worker", "status": "active"}},
			"messages":      []any{map[string]any{"sequence": 1, "id": "msg_1", "body": "matching-cached-body", "expires_at": time.Now().Add(time.Hour).UTC().Format(time.RFC3339Nano), "delivered_at": nil, "read_at": nil}},
			"sent_messages": []any{}, "events": []any{},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	client, err := NewWithTokenSource(config, TokenSourceFunc(func(context.Context) (string, error) {
		return "", errors.New("synthetic-plain-source-secret")
	}))
	if err != nil {
		t.Fatal(err)
	}
	result, err := client.Execute(context.Background(), bus.Request{
		Operation: "inbox", Workspace: "w", Project: "p", Session: "worker",
		Args: map[string]any{"instance": "instance-1"},
	})
	if err == nil {
		t.Fatalf("plain token-source failure returned %q from mirror", result.Source)
	}
	if strings.Contains(strings.ToLower(err.Error()), "mirror") || strings.Contains(err.Error(), "synthetic-plain-source-secret") {
		t.Fatalf("credential failure consulted or exposed mirror/source details: %v", err)
	}
	after, readErr := os.ReadFile(path)
	if readErr != nil || !bytes.Equal(before, after) {
		t.Fatal("plain token-source failure mutated the mirror")
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	client, err = NewWithTokenSource(config, TokenSourceFunc(func(context.Context) (string, error) {
		return "", errors.New("must not replace the cancellation")
	}))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = client.Execute(canceled, bus.Request{Operation: "inbox", Workspace: "w", Project: "p", Session: "worker", Args: map[string]any{"instance": "instance-1"}}); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation was not preserved: %v", err)
	}
}

// R85-L1: a bearer route behind a path prefix keeps it, as before device
// signing changed how the request target is built.
func TestBearerRouteKeepsTheServerPath(t *testing.T) {
	var got string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.RequestURI
		_ = json.NewEncoder(w).Encode(bus.Result{Source: "server", Data: map[string]any{}})
	}))
	defer server.Close()
	client, err := NewWithTokenSource(Config{URL: server.URL + "/bus", Audience: "audience", Principal: "alice@example.test"}, TokenSourceFunc(func(context.Context) (string, error) { return "synthetic-token", nil }))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = client.Execute(context.Background(), bus.Request{Operation: "list_projects", Workspace: "w"}); err != nil || got != "/bus/v1/execute" {
		t.Fatalf("sent %q: %v", got, err)
	}
}
