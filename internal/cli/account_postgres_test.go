package cli

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"clawbridge/internal/bus"
	"clawbridge/internal/server"
	"github.com/zalando/go-keyring"
)

func pgUnique(prefix string) string {
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	return prefix + hex.EncodeToString(b)
}

// End to end on Postgres, through the CLI: the owner invites, the person
// joins with the code, the device signs whoami, and a device revoked
// through the server is refused at once.
func TestPostgresJoinThenSignedCLI(t *testing.T) {
	dsn := os.Getenv("CLAWBRIDGE_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("reviewer supplies disposable Postgres DSN")
	}
	isolated(t)
	keyring.MockInit()
	ctx := context.Background()
	owner := pgUnique("owner") + "@device-cli.example"
	store, err := bus.OpenPostgres(ctx, dsn, owner)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	google := bus.Identity{Principal: owner, Issuer: bus.GoogleIssuer, Subject: "sub-" + owner}
	ws := pgUnique("ws")
	for _, r := range []bus.Request{{Operation: "workspace_init", Workspace: ws, Args: map[string]any{"mode": "server"}}, {Operation: "project_create", Workspace: ws, Args: map[string]any{"slug": "p"}}} {
		if _, err = store.ExecuteAsIdentity(ctx, google, r); err != nil {
			t.Fatal(err)
		}
	}
	var handler http.Handler
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { handler.ServeHTTP(w, r) }))
	t.Cleanup(ts.Close)
	handler = server.NewHandlerWithOptions(store, nil, server.HandlerOptions{Auth: server.AuthOptions{Methods: []string{server.AuthDevice}, PublicOrigin: ts.URL, Devices: store}, Now: time.Now})
	ada := pgUnique("ada") + "@device-cli.example"
	invite, err := store.ExecuteAsIdentity(ctx, google, bus.Request{Operation: "invite_create", Workspace: ws, Args: map[string]any{"email": ada, "grants": map[string]any{"p": "member"}}})
	if err != nil {
		t.Fatal(err)
	}
	code := invite.Data.(map[string]any)["code"].(string)
	out, err := run("--server", ts.URL, "join", "--email", ada, "--code", strings.ToLower(code), "--label", "laptop")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, `"workspace":"`+ws+`"`) || !strings.Contains(out, `"principal":"`+ada+`"`) {
		t.Fatal(out)
	}
	out, err = run("--project", "p", "whoami")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"identity_provenance":"device-key"`, `"label":"laptop"`, `"key_store":"keyring"`, `"principal":"` + ada + `"`} {
		if !strings.Contains(out, want) {
			t.Fatalf("whoami lacks %s: %s", want, out)
		}
	}
	// The code is single use.
	other := t.TempDir() + "/again.toml"
	if _, err = run("--server", ts.URL, "--client-config", other, "join", "--email", ada, "--code", code); err == nil || !strings.Contains(err.Error(), "invalid or expired invite") {
		t.Fatal("a used code joined again:", err)
	}
	config, _, err := resolveConfig(ctx, Config{})
	if err != nil {
		t.Fatal(err)
	}
	if out, err = run("device", "list"); err != nil || !strings.Contains(out, config.DeviceID) {
		t.Fatal(out, err)
	}
	if _, err = run("device", "revoke", config.DeviceID, "--reason", "test"); err != nil {
		t.Fatal(err)
	}
	if _, err = run("device", "list"); err == nil || !strings.Contains(err.Error(), "revoked") {
		t.Fatal("a revoked device signed in:", err)
	}
}
