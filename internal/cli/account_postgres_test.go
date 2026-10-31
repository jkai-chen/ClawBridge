package cli

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"io"
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

// v1.0 end to end with Google sign-in removed, on Postgres and through the
// CLI: the host operator's bootstrap invite makes the first owner, who
// joins with a device key, creates a project and invites a member; the
// member's old Google profile switches to a device key on join; each signs
// whoami with its key; and a Bearer (Google ID token) request is refused.
func TestPostgresGoogleRemovedOwnerToMember(t *testing.T) {
	dsn := os.Getenv("CLAWBRIDGE_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("reviewer supplies disposable Postgres DSN")
	}
	dir := isolated(t)
	keyring.MockInit()
	ctx := context.Background()
	owner := pgUnique("owner") + "@device-cli.example"
	store, err := bus.OpenPostgres(ctx, dsn, owner)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	var handler http.Handler
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { handler.ServeHTTP(w, r) }))
	t.Cleanup(ts.Close)
	// The server Run builds: device sign-in only, no verifier.
	handler = server.NewHandlerWithOptions(store, nil, server.HandlerOptions{Auth: server.AuthOptions{Methods: []string{server.AuthDevice}, PublicOrigin: ts.URL, Devices: store}, Now: time.Now})
	ws := pgUnique("ws")
	boot, err := store.OperatorBootstrapInvite(ctx, ws, owner, "")
	if err != nil {
		t.Fatal(err)
	}
	ownerProfile := dir + "/owner.toml"
	if _, err = run("--server", ts.URL, "--client-config", ownerProfile, "join", "--email", owner, "--code", boot["code"].(string), "--label", "owner-laptop"); err != nil {
		t.Fatal("the bootstrap owner could not join:", err)
	}
	if _, err = run("--client-config", ownerProfile, "project", "create", "p"); err != nil {
		t.Fatal(err)
	}
	// The hosted owner's first project is the profile's default, as a local
	// one's is (first-deployment step 7): whoami needs no --project, and a
	// second project does not replace it.
	if raw, _ := os.ReadFile(ownerProfile); !strings.Contains(strings.ReplaceAll(string(raw), "'", `"`), `project = "p"`) {
		t.Fatal("the first project is not the hosted profile's default:\n" + string(raw))
	}
	if _, err = run("--client-config", ownerProfile, "whoami"); err != nil {
		t.Fatal("whoami without --project:", err)
	}
	if _, err = run("--client-config", ownerProfile, "project", "create", "p2"); err != nil {
		t.Fatal(err)
	}
	if raw, _ := os.ReadFile(ownerProfile); strings.Contains(string(raw), "p2") {
		t.Fatal("a second project replaced the default:\n" + string(raw))
	}
	ada := pgUnique("ada") + "@device-cli.example"
	onTerminal(t)
	out, err := run("--client-config", ownerProfile, "--json", "invite", "--email", ada, "--grant", "p=member")
	if err != nil {
		t.Fatal(err)
	}
	var code string
	for _, f := range strings.FieldsFunc(out, func(r rune) bool { return r == '"' }) {
		if strings.HasPrefix(f, "CBJ-") {
			code = f
			break
		}
	}
	if code == "" {
		t.Fatal("no invite code shown:", out)
	}
	// The member's profile from before v1.0 signed in with Google; the join
	// switches it to the new device key.
	adaProfile := dir + "/ada.toml"
	old := "mode = \"cloud\"\nserver = \"" + ts.URL + "\"\nprincipal = \"" + ada + "\"\ncache = \"cache.db\"\ncredential_source = \"oauth\"\naudience = \"desktop.example.test\"\noauth_client_id = \"desktop.example.test\"\n"
	if err = os.WriteFile(adaProfile, []byte(old), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err = run("--client-config", adaProfile, "--project", "p", "whoami"); err == nil || !strings.Contains(err.Error(), "removed in v1.0") {
		t.Fatal("an old Google profile signed in:", err)
	}
	if _, err = run("--server", ts.URL, "--client-config", adaProfile, "join", "--email", ada, "--code", code, "--label", "ada-laptop"); err != nil {
		t.Fatal(err)
	}
	for who, path := range map[string]string{owner: ownerProfile, ada: adaProfile} {
		out, err = run("--client-config", path, "--project", "p", "whoami")
		if err != nil || !strings.Contains(out, `"identity_provenance":"device-key"`) || !strings.Contains(out, `"principal":"`+who+`"`) {
			t.Fatal(who, out, err)
		}
	}
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/v1/execute", strings.NewReader(`{"operation":"whoami","workspace":"`+ws+`"}`))
	req.Header.Set("Authorization", "Bearer eyJhbGciOiJSUzI1NiJ9.e30.c2ln")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(res.Body)
	res.Body.Close()
	if res.StatusCode != 401 || !strings.Contains(string(body), server.BearerRefused) {
		t.Fatal("a Bearer request was answered:", res.StatusCode, string(body))
	}
}
