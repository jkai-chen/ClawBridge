package cli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"io"
	"net/url"
	"strings"
	"time"

	"clawbridge/internal/bus"
	"clawbridge/internal/clirender"
)

// linear webhook-setup (FR14 §3 G3): what the owner creates in Linear's UI
// for each bound write team, read-only. ClawBridge writes no Linear
// configuration: no webhookCreate, no admin scope. It prints the env line's
// shape with <secret>, never a value.

const (
	webhookSetupScope     = "choose this team, not All public teams"
	webhookSetupWhere     = "Linear: Settings, API, Webhooks, New webhook, once per team above"
	webhookSetupEnvShape  = "CLAWBRIDGE_LINEAR_WEBHOOK_SECRETS=<team UUID>=<secret>[,<team UUID>=<secret>...]"
	webhookSetupEnvNote   = "set it in /etc/clawbridge/clawbridge.env in your own terminal; never paste it into an agent"
	webhookSetupThen      = "recreate the API so it reads the secrets; the webhooks gate stays off until the probe passes"
	webhookSetupConfig    = "none (the owner creates team webhooks in Linear's UI)"
	webhookSetupNoName    = "(no stored name; linear names-refresh stores it)"
	webhookSetupRoutePath = "/linear/webhook/"
)

var webhookSetupResources = []string{"Issues", "Comments"}

type webhookSetupTeam struct {
	TeamID        string   `json:"team_id"`
	Name          string   `json:"name"`
	URL           string   `json:"url"`
	ResourceTypes []string `json:"resource_types"`
	TeamScope     string   `json:"team_scope"`
	WebhookState  string   `json:"webhook_state"`
}

type webhookSetup struct {
	Teams             []webhookSetupTeam `json:"teams"`
	Where             string             `json:"where"`
	EnvLine           string             `json:"env_line"`
	EnvLineHere       string             `json:"env_line_here"`
	EnvNote           string             `json:"env_note"`
	Then              string             `json:"then"`
	LinearConfigScope string             `json:"linear_config_scope"`
	// Warning is set for a loopback http server, which Linear can't reach.
	Warning string `json:"warning,omitempty"`
}

func init() { clirender.Register("linear_webhook-setup", webhookSetupView) }

func runLinearWebhookSetup(ctx context.Context, flags Config, args []string, stdout, stderr io.Writer) error {
	f := flag.NewFlagSet("linear webhook-setup", flag.ContinueOnError)
	f.SetOutput(stderr)
	project := f.String("project", "", "the ClawBridge project whose bound write teams are shown (default: the configured project)")
	if err := f.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if f.NArg() > 0 {
		return errors.New("usage: clawbridge linear webhook-setup [--project SLUG]")
	}
	if *project != "" {
		flags.Project = *project
	}
	bounded, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	c, _, err := resolveConfig(bounded, flags)
	if err != nil {
		return err
	}
	origin, warning, err := webhookSetupOrigin(c)
	if err != nil {
		return err
	}
	executor, closeExecutor, scope, err := linearExecutor(bounded, flags)
	if err != nil {
		return err
	}
	defer closeExecutor()
	read := func(op string, args map[string]any, out any) error {
		r, e := executor.Execute(bounded, bus.Request{Operation: op, Workspace: scope.Workspace, Project: scope.Project, Session: scope.Session, Args: args})
		if e != nil {
			return e
		}
		raw, e := json.Marshal(r.Data)
		if e != nil {
			return e
		}
		return json.Unmarshal(raw, out)
	}
	var status struct {
		Teams []struct {
			TeamID string `json:"team_id"`
			State  string `json:"state"`
		} `json:"teams"`
	}
	if err = read("webhook_status", map[string]any{}, &status); err != nil {
		return err
	}
	overviewArgs := map[string]any{}
	if inst, ok := scope.Args["instance"]; ok && scope.Session != "" {
		overviewArgs["instance"] = inst
	}
	var overview struct {
		Linear struct {
			Teams []struct {
				ID   string `json:"id"`
				Name string `json:"name"`
			} `json:"teams"`
		} `json:"linear"`
	}
	if err = read("project_overview", overviewArgs, &overview); err != nil {
		return err
	}
	names := map[string]string{}
	for _, t := range overview.Linear.Teams {
		names[strings.ToLower(t.ID)] = t.Name
	}
	out := webhookSetup{Teams: []webhookSetupTeam{}, Where: webhookSetupWhere, EnvLine: webhookSetupEnvShape, EnvNote: webhookSetupEnvNote, Then: webhookSetupThen, LinearConfigScope: webhookSetupConfig, Warning: warning}
	here := []string{}
	for _, t := range status.Teams {
		id := strings.ToLower(t.TeamID)
		name := clirender.Safe(names[id])
		if name == "" {
			name = webhookSetupNoName
		}
		out.Teams = append(out.Teams, webhookSetupTeam{TeamID: id, Name: name, URL: origin + webhookSetupRoutePath + id,
			ResourceTypes: webhookSetupResources, TeamScope: webhookSetupScope, WebhookState: t.State})
		here = append(here, id+"=<secret>")
	}
	if len(here) == 0 {
		return errors.New("the project has no bound write team; link the task board first (linear link)")
	}
	out.EnvLineHere = "CLAWBRIDGE_LINEAR_WEBHOOK_SECRETS=" + strings.Join(here, ",")
	return emit(ctx, stdout, "linear_webhook-setup", bus.Result{Source: "linear", Data: out})
}

// webhookSetupOrigin is the hosted server's origin, where Linear posts:
// https, as a Linear webhook needs a public https URL. A loopback http
// server (a developer's own) is allowed with a warning; any other http is
// refused. Any path in the configured server is dropped on purpose: the
// route is always at the origin's root, /linear/webhook/<team>.
func webhookSetupOrigin(c Config) (origin, warning string, err error) {
	if c.Mode != "cloud" || c.Server == "" {
		return "", "", errors.New("linear webhook-setup is for a hosted server: Linear posts to its public URL; select the hosted server first")
	}
	u, err := url.Parse(c.Server)
	if err != nil || u.Host == "" {
		return "", "", errors.New("the configured server is not an https URL; a Linear webhook needs a public https URL")
	}
	host := strings.ToLower(u.Host)
	switch {
	case u.Scheme == "https":
		return "https://" + host, "", nil
	case u.Scheme == "http" && (u.Hostname() == "localhost" || u.Hostname() == "127.0.0.1" || u.Hostname() == "::1"):
		return "http://" + host, "this server is a loopback http URL: Linear cannot reach it; create the webhooks against the hosted server's public https URL", nil
	}
	return "", "", errors.New("the configured server is not an https URL; a Linear webhook needs a public https URL")
}

func webhookSetupView(v any) (clirender.Doc, bool) {
	r, ok := v.(bus.Result)
	if !ok {
		return clirender.Doc{}, false
	}
	s, ok := r.Data.(webhookSetup)
	if !ok {
		return clirender.Doc{}, false
	}
	var d clirender.Doc
	d.Title("Linear webhooks to create")
	if s.Warning != "" {
		d.Status(clirender.Warn, s.Warning)
	}
	d.KV("Where", s.Where)
	d.KV("Linear config scope", s.LinearConfigScope)
	for _, t := range s.Teams {
		d.Section(t.Name)
		d.KV("Team", t.TeamID)
		d.KV("Resource types", strings.Join(t.ResourceTypes, ", "))
		d.KV("Webhook state", t.WebhookState)
		d.Note(t.TeamScope)
		// Whole on its own line, so it copies in one piece; not a secret.
		d.Secret("URL", t.URL)
		d.End()
	}
	d.Section("Signing secrets")
	d.Secret("Shape", s.EnvLine)
	d.Secret("For this project", s.EnvLineHere)
	d.Note(s.EnvNote)
	d.Note("Then " + s.Then)
	d.End()
	return d, true
}
