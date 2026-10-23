package remote

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"clawbridge/internal/bus"
	"clawbridge/internal/server"
)

// v023Rejection is the v0.2.3 client's handling of a refused request
// (internal/remote/client.go:439-456 and HTTPError.Error at 3b41576), pinned
// so the upgrade refusal is tested as the clients it targets will show it.
func v023Rejection(status int, encoded []byte) string {
	code, message := "rejected", ""
	var body struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal(encoded, &body) == nil {
		switch body.Error.Code {
		case "unauthenticated", "forbidden", "invalid", "conflict", "not_found", "rate_limited", "unavailable", "binding_unreadable":
			code = body.Error.Code
		}
		if (status == http.StatusBadRequest && code == "invalid") || (status == http.StatusConflict && code == "binding_unreadable") {
			message, _ = bus.SafePublicText(body.Error.Message)
		}
	}
	if message != "" {
		return fmt.Sprintf("cloud request rejected (%d, %s): %s; no write queued", status, code, message)
	}
	return fmt.Sprintf("cloud request rejected (%d, %s); no write queued", status, code)
}

// A pre-v0.2.5 client's write access request (instance only) is refused, and
// that client prints the upgrade instruction; a current client is not.
func TestAnOldClientIsToldToUpgradeBeforeLinearWrites(t *testing.T) {
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
	_, refusal := store.Execute(ctx, bus.Request{Operation: "taskboard_access", Workspace: "w", Project: "p", Args: map[string]any{}})
	if refusal == nil {
		t.Fatal("access without ledger:1 was not refused")
	}
	verifier := server.VerifierFunc(func(context.Context, string) (server.Identity, error) {
		return server.Identity{Principal: "alice@example.test", Subject: "sub"}, nil
	})
	host := httptest.NewServer(server.NewHandler(errorExecutor{refusal}, verifier))
	defer host.Close()
	payload, _ := json.Marshal(bus.Request{Operation: "taskboard_access", Workspace: "w", Project: "p", Args: map[string]any{}})
	request, _ := http.NewRequest(http.MethodPost, host.URL+"/v1/execute", bytes.NewReader(payload))
	request.Header.Set("Authorization", "Bearer synthetic-token")
	request.Header.Set("Content-Type", "application/json")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	encoded, _ := io.ReadAll(response.Body)
	response.Body.Close()
	shown := v023Rejection(response.StatusCode, encoded)
	if !strings.Contains(shown, "upgrade ClawBridge (v0.2.5 or later)") {
		t.Fatalf("a v0.2.3 client would show %q (HTTP %d %s)", shown, response.StatusCode, encoded)
	}
	// This client shows it too.
	_, err = publicTestClient(t, host.URL).Execute(ctx, bus.Request{Operation: "taskboard_access", Workspace: "w", Project: "p", Args: map[string]any{"ledger": 1}})
	if err == nil || !strings.Contains(err.Error(), "upgrade ClawBridge") {
		t.Fatal(err)
	}
}
