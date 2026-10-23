package remote

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"clawbridge/internal/bus"
	"clawbridge/internal/server"
)

type errorExecutor struct{ err error }

func (e errorExecutor) ExecuteAs(context.Context, string, bus.Request) (bus.Result, error) {
	return bus.Result{}, e.err
}

func publicTestClient(t *testing.T, url string) *Client {
	t.Helper()
	client, err := NewWithTokenSource(Config{URL: url, Audience: "audience", Principal: "alice@example.test", CachePath: filepath.Join(t.TempDir(), "cache.db")}, TokenSourceFunc(func(context.Context) (string, error) { return "synthetic-token", nil }))
	if err != nil {
		t.Fatal(err)
	}
	return client
}

// A validation message the bus wrote for the caller reaches the CLI through
// the real server; an unknown failure behind the same 400 stays generic.
func TestValidationMessageReachesTheClientAndInternalsDoNot(t *testing.T) {
	store, err := bus.Open(filepath.Join(t.TempDir(), "bus.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	for _, r := range []bus.Request{{Operation: "workspace_init", Workspace: "w", Args: map[string]any{"mode": "local"}}, {Operation: "project_create", Workspace: "w", Args: map[string]any{"slug": "p"}}} {
		if _, err = store.Execute(ctx, r); err != nil {
			t.Fatal(err)
		}
	}
	_, refusal := store.Execute(ctx, bus.Request{Operation: "register_session", Workspace: "w", Project: "p", Session: "s", Args: map[string]any{"instance": "i", "agent_kind": "not-an-agent"}})
	if refusal == nil {
		t.Fatal("no validation refusal to forward")
	}
	verifier := server.VerifierFunc(func(context.Context, string) (server.Identity, error) {
		return server.Identity{Principal: "alice@example.test", Subject: "sub"}, nil
	})
	for name, c := range map[string]struct {
		err  error
		want string
		deny string
	}{
		"bus validation": {refusal, ": unsupported agent_kind; no write queued", ""},
		"unknown":        {errors.New(`pq: violates check constraint "secret_internal"`), "request rejected; verify scope", "secret_internal"},
	} {
		host := httptest.NewServer(server.NewHandler(errorExecutor{c.err}, verifier))
		_, err := publicTestClient(t, host.URL).Execute(ctx, bus.Request{Operation: "whoami", Workspace: "w", Project: "p", Session: "s", Args: map[string]any{"instance": "i"}})
		host.Close()
		var rejected *HTTPError
		if !errors.As(err, &rejected) || rejected.Status != 400 || !strings.Contains(err.Error(), c.want) || (c.deny != "" && strings.Contains(err.Error(), c.deny)) {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestClientKeepsOnlyABoundedOneLineReasonForA400(t *testing.T) {
	for name, c := range map[string]struct {
		status int
		body   string
		want   string
	}{
		"multi-line":      {400, `{"error":{"code":"invalid","message":"first\nsecond"}}`, ""},
		"long":            {400, `{"error":{"code":"invalid","message":"` + strings.Repeat("x", 400) + `"}}`, strings.Repeat("x", 300)},
		"forbidden":       {403, `{"error":{"code":"forbidden","message":"kept private"}}`, ""},
		"other 400 codes": {400, `{"error":{"code":"conflict","message":"kept private"}}`, ""},
	} {
		host := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(c.status)
			_, _ = w.Write([]byte(c.body))
		}))
		_, err := publicTestClient(t, host.URL).Execute(context.Background(), bus.Request{Operation: "whoami", Workspace: "w", Project: "p", Session: "s", Args: map[string]any{"instance": "i"}})
		host.Close()
		var rejected *HTTPError
		if !errors.As(err, &rejected) || rejected.Message != c.want {
			t.Errorf("%s: %+v %v", name, rejected, err)
		}
	}
}

// An unreadable task board binding reaches the CLI as its own code with the
// repair path, not as a generic conflict.
func TestUnreadableBindingRefusalReachesTheClientWithItsRepairPath(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bus.db")
	store, err := bus.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	for _, r := range []bus.Request{{Operation: "workspace_init", Workspace: "w", Args: map[string]any{"mode": "local"}}, {Operation: "project_create", Workspace: "w", Args: map[string]any{"slug": "p"}}, {Operation: "taskboard_set_v2", Workspace: "w", Project: "p", Args: map[string]any{"expected_revision": 0, "mode": "team_board", "linear_team_ids": []string{"33333333-3333-3333-3333-333333333333"}, "linear_default_team_id": "33333333-3333-3333-3333-333333333333", "linear_project_ids": []string{}}}} {
		if _, err = store.Execute(ctx, r); err != nil {
			t.Fatal(err)
		}
	}
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = raw.Exec(`UPDATE taskboard_bindings SET mapping='{not json'`); err != nil {
		t.Fatal(err)
	}
	raw.Close()
	_, refusal := store.Execute(ctx, bus.Request{Operation: "taskboard_get_v2", Workspace: "w", Project: "p", Args: map[string]any{}})
	if refusal == nil {
		t.Fatal("unreadable binding was read")
	}
	verifier := server.VerifierFunc(func(context.Context, string) (server.Identity, error) {
		return server.Identity{Principal: "alice@example.test", Subject: "sub"}, nil
	})
	host := httptest.NewServer(server.NewHandler(errorExecutor{refusal}, verifier))
	defer host.Close()
	_, err = publicTestClient(t, host.URL).Execute(ctx, bus.Request{Operation: "taskboard_get_v2", Workspace: "w", Project: "p", Args: map[string]any{}})
	var rejected *HTTPError
	if !errors.As(err, &rejected) || rejected.Status != 409 || rejected.Code != "binding_unreadable" || !strings.Contains(err.Error(), "--expected-revision 1 --confirm --repair") {
		t.Fatal(err)
	}
}
