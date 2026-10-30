package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"clawbridge/internal/clirender"
	"clawbridge/internal/faketool"
)

// Synthetic teams: one with a stored name, one without.
const (
	wsTeamA = "11111111-1111-4111-8111-1111111111a1"
	wsTeamB = "22222222-2222-4222-8222-2222222222b2"
)

// webhookSetupServer answers the reads webhook-setup makes and fails on
// any other operation: the helper writes nothing.
func webhookSetupServer(t *testing.T, ops *[]string) *httptest.Server {
	var mu sync.Mutex
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Operation string
			Project   string
			Args      map[string]any
		}
		if e := json.NewDecoder(r.Body).Decode(&req); e != nil {
			t.Error(e)
		}
		mu.Lock()
		*ops = append(*ops, req.Operation)
		mu.Unlock()
		var data any
		switch req.Operation {
		case "resolve_project":
			data = map[string]any{"workspace_id": "ws-cloud", "project_id": "prj-cloud"}
		case "webhook_status":
			if len(req.Args) != 0 {
				t.Errorf("webhook_status takes no arguments: %v", req.Args)
			}
			data = map[string]any{"teams": []any{
				map[string]any{"team_id": wsTeamA, "configured": true, "state": "inert"},
				map[string]any{"team_id": wsTeamB, "configured": false, "state": "unconfigured"},
			}}
		case "project_overview":
			data = map[string]any{"linear": map[string]any{"configured": true, "teams": []any{
				map[string]any{"id": strings.ToUpper(wsTeamA), "name": "Platform Probe", "default": true},
				map[string]any{"id": wsTeamB, "name": "", "default": false},
			}}}
		default:
			t.Errorf("unexpected cloud operation %s", req.Operation)
			w.WriteHeader(500)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"source": "server", "data": data})
	}))
}

func webhookSetupEnv(t *testing.T, server string) {
	t.Helper()
	dir := isolated(t)
	t.Setenv("HOME", dir)
	faketool.Install(t, dir, "gcloud", "print:synthetic-cli-token")
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	principal := "service@example.test"
	for k, v := range map[string]string{"MODE": "cloud", "SERVER": server, "AUDIENCE": "test-audience", "ACCOUNT": "human@example.test", "IMPERSONATE_SERVICE_ACCOUNT": principal, "PRINCIPAL": principal, "CACHE": filepath.Join(dir, "mirror.db"), "WORKSPACE": "w", "PROJECT": "p"} {
		t.Setenv("CLAWBRIDGE_"+k, v)
	}
}

const webhookSetupGoldenJSON = `{"source":"linear","data":{"teams":[` +
	`{"team_id":"11111111-1111-4111-8111-1111111111a1","name":"Platform Probe","url":"https://clawbridge.example.test/linear/webhook/11111111-1111-4111-8111-1111111111a1","resource_types":["Issues","Comments"],"team_scope":"choose this team, not All public teams","webhook_state":"inert"},` +
	`{"team_id":"22222222-2222-4222-8222-2222222222b2","name":"(no stored name; linear names-refresh stores it)","url":"https://clawbridge.example.test/linear/webhook/22222222-2222-4222-8222-2222222222b2","resource_types":["Issues","Comments"],"team_scope":"choose this team, not All public teams","webhook_state":"unconfigured"}],` +
	`"where":"Linear: Settings, API, Webhooks, New webhook, once per team above","env_line":"CLAWBRIDGE_LINEAR_WEBHOOK_SECRETS=<team UUID>=<secret>[,<team UUID>=<secret>...]",` +
	`"env_line_here":"CLAWBRIDGE_LINEAR_WEBHOOK_SECRETS=11111111-1111-4111-8111-1111111111a1=<secret>,22222222-2222-4222-8222-2222222222b2=<secret>",` +
	`"env_note":"set it in /etc/clawbridge/clawbridge.env in your own terminal; never paste it into an agent","then":"recreate the API so it reads the secrets; the webhooks gate stays off until the probe passes",` +
	`"linear_config_scope":"none (the owner creates team webhooks in Linear's UI)",` +
	`"warning":"this server is a loopback http URL: Linear cannot reach it; create the webhooks against the hosted server's public https URL"}}
`

const webhookSetupGoldenHuman = `Linear webhooks to create
WARN this server is a loopback http URL: Linear cannot reach it; create the webhooks against the
     hosted server's public https URL
Where                Linear: Settings, API, Webhooks, New webhook, once per team above
Linear config scope  none (the owner creates team webhooks in Linear's UI)

Platform Probe
  Team            11111111-1111-4111-8111-1111111111a1
  Resource types  Issues, Comments
  Webhook state   inert
  choose this team, not All public teams
  URL
    https://clawbridge.example.test/linear/webhook/11111111-1111-4111-8111-1111111111a1

(no stored name; linear names-refresh stores it)
  Team            22222222-2222-4222-8222-2222222222b2
  Resource types  Issues, Comments
  Webhook state   unconfigured
  choose this team, not All public teams
  URL
    https://clawbridge.example.test/linear/webhook/22222222-2222-4222-8222-2222222222b2

Signing secrets
  Shape
    CLAWBRIDGE_LINEAR_WEBHOOK_SECRETS=<team UUID>=<secret>[,<team UUID>=<secret>...]
  For this project
    CLAWBRIDGE_LINEAR_WEBHOOK_SECRETS=11111111-1111-4111-8111-1111111111a1=<secret>,22222222-2222-4222-8222-2222222222b2=<secret>
  set it in /etc/clawbridge/clawbridge.env in your own terminal; never paste it into an agent
  Then recreate the API so it reads the secrets; the webhooks gate stays off until the probe passes
`

// webhook-setup prints, for each bound write team, what the owner creates
// in Linear's UI and the env line's shape with <secret>, never a value. It
// reads only: webhook_status and the overview's names.
func TestLinearWebhookSetupGolden(t *testing.T) {
	var ops []string
	host := webhookSetupServer(t, &ops)
	defer host.Close()
	webhookSetupEnv(t, host.URL)
	var out, diag bytes.Buffer
	if err := Run(context.Background(), []string{"linear", "webhook-setup", "--project", "p"}, &out, &diag); err != nil {
		t.Fatal(err, diag.String())
	}
	got := strings.ReplaceAll(out.String(), host.URL, "https://clawbridge.example.test")
	// JSON mode is json.Encode's, which escapes < and >.
	want := strings.NewReplacer("<", `\u003c`, ">", `\u003e`).Replace(webhookSetupGoldenJSON)
	if got != want {
		t.Fatalf("got:\n%s\nwant:\n%s", got, want)
	}
	if strings.Join(ops, ",") != "resolve_project,webhook_status,project_overview" {
		t.Fatal(ops)
	}
	// For a person: the same, laid out.
	out.Reset()
	ctx := clirender.WithMode(context.Background(), clirender.Human)
	if err := runLinear(ctx, Config{}, []string{"webhook-setup"}, &out, &diag); err != nil {
		t.Fatal(err)
	}
	human := strings.ReplaceAll(out.String(), host.URL, "https://clawbridge.example.test")
	if human != webhookSetupGoldenHuman {
		t.Fatalf("got:\n%s\nwant:\n%s", human, webhookSetupGoldenHuman)
	}
	for _, want := range []string{"Platform Probe", "https://clawbridge.example.test/linear/webhook/" + wsTeamA, "Issues, Comments", "choose this team, not All public teams",
		"CLAWBRIDGE_LINEAR_WEBHOOK_SECRETS=<team UUID>=<secret>[,<team UUID>=<secret>...]", "never paste it into an agent", "(no stored name; linear names-refresh stores it)"} {
		if !strings.Contains(human, want) {
			t.Fatalf("the human output lacks %q:\n%s", want, human)
		}
	}
}

// A local store has no public route: webhook-setup says so and reads
// nothing.
func TestLinearWebhookSetupNeedsAHostedServer(t *testing.T) {
	dir := isolated(t)
	t.Setenv("HOME", dir)
	t.Setenv("CLAWBRIDGE_DB", filepath.Join(dir, "bus.db"))
	t.Setenv("CLAWBRIDGE_WORKSPACE", "w")
	t.Setenv("CLAWBRIDGE_PROJECT", "p")
	var out, diag bytes.Buffer
	err := Run(context.Background(), []string{"linear", "webhook-setup"}, &out, &diag)
	if err == nil || !strings.Contains(err.Error(), "hosted server") || out.Len() != 0 {
		t.Fatal(err, out.String())
	}
	if _, e := os.Stat(filepath.Join(dir, "bus.db")); e == nil {
		t.Fatal("a local store was opened")
	}
}

// Linear posts only to a public https URL: a plain http server is refused,
// naming nothing read. A loopback http server (a developer's own) is
// shown with a warning.
func TestLinearWebhookSetupOrigin(t *testing.T) {
	for _, c := range []struct{ server, want, warning string }{
		{"https://ClawBridge.example.test/some/path", "https://clawbridge.example.test", ""},
		{"http://127.0.0.1:8080", "http://127.0.0.1:8080", "Linear cannot reach"},
		{"http://localhost:8080", "http://localhost:8080", "Linear cannot reach"},
	} {
		got, warning, err := webhookSetupOrigin(Config{Mode: "cloud", Server: c.server})
		if err != nil || got != c.want || !strings.Contains(warning, c.warning) || (c.warning == "") != (warning == "") {
			t.Fatal(c.server, got, warning, err)
		}
	}
	for _, bad := range []string{"http://clawbridge.example.test", "ftp://clawbridge.example.test", "https://"} {
		if _, _, err := webhookSetupOrigin(Config{Mode: "cloud", Server: bad}); err == nil || !strings.Contains(err.Error(), "https") {
			t.Fatal(bad, err)
		}
	}
	var ops []string
	host := webhookSetupServer(t, &ops)
	defer host.Close()
	webhookSetupEnv(t, "http://clawbridge.example.test")
	var out, diag bytes.Buffer
	if err := Run(context.Background(), []string{"linear", "webhook-setup"}, &out, &diag); err == nil || !strings.Contains(err.Error(), "https") || out.Len() != 0 || len(ops) != 0 {
		t.Fatal(err, out.String(), ops)
	}
}

// The linear help lists webhook-setup.
func TestLinearHelpListsWebhookSetup(t *testing.T) {
	var out bytes.Buffer
	if err := runLinear(context.Background(), Config{}, []string{"--help"}, &out, &out); err != nil || !strings.Contains(out.String(), "webhook-setup") {
		t.Fatal(err, out.String())
	}
}
