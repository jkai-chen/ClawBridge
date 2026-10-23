package server

import (
	"context"
	"encoding/base64"
	"errors"
	"log"
	"net/http"
	"os"
	"time"

	"clawbridge/internal/bus"
	"clawbridge/internal/linearapp"
	"clawbridge/internal/linearboard"
	"clawbridge/internal/linearconn"
	"clawbridge/internal/linearprovider"
)

// appWriteTimeout bounds a Linear write the server runs as the app. Once it
// starts it finishes even if the client disconnects, so its record settles;
// the server's write timeout leaves room for it.
const appWriteTimeout = 60 * time.Second

// appWriteOperations are the only board operations linear_write runs: the
// agent writes. Binding and admin operations never run as the app.
var appWriteOperations = map[string]bool{"comment": true, "create": true, "update": true, "relation_add": true, "relation_remove": true}

// identityArgs are refused in linear_write's arguments: the server alone
// chooses the identity a write uses.
var identityArgs = []string{"profile", "profile_id", "access", "as", "identity", "token", "write_identity"}

// AppWriter runs agent writes to Linear as the ClawBridge app, on the server:
// clients send a typed write and never hold the app token.
type AppWriter struct {
	App   *linearapp.App
	Store *bus.Store
	Now   func() time.Time
}

func appRefusal(message string) error { return bus.PublicError("invalid", message) }

// identityExecutor runs bus operations as the verified caller.
type identityExecutor struct {
	store    *bus.Store
	identity bus.Identity
}

func (e identityExecutor) Execute(ctx context.Context, r bus.Request) (bus.Result, error) {
	result, err := e.store.ExecuteAsIdentity(ctx, e.identity, r)
	result.Source = "server"
	return result, err
}

// appConnections presents the app as the board's one write profile.
type appConnections struct{ org string }

func (c appConnections) Status(context.Context, string) (linearconn.Profile, error) {
	return linearconn.Profile{ID: "app:" + c.org, Name: "ClawBridge app", Access: linearconn.Write, WorkspaceID: c.org}, nil
}
func (appConnections) HTTPClient(context.Context, string) (*http.Client, error) {
	return &http.Client{}, nil
}

func (w *AppWriter) service(ctx context.Context, identity bus.Identity) (*linearboard.Service, linearapp.Identity, error) {
	id, err := w.App.Identity(ctx)
	if err != nil {
		return nil, id, bus.PublicError("unavailable", "the ClawBridge Linear app is unavailable")
	}
	now := w.Now
	if now == nil {
		now = time.Now
	}
	return linearboard.New(linearboard.Config{
		Authority:   "hosted-app",
		Executor:    identityExecutor{w.Store, identity},
		Connections: appConnections{id.LinearOrgID},
		ProfileID:   "app:" + id.LinearOrgID,
		Ledger:      linearboard.SharedLedger(w.Store, now),
		Workspace:   id.LinearOrgID,
		Now:         now,
		Open: func(ctx context.Context, _ *http.Client, readonly bool) (linearboard.Provider, error) {
			return w.App.Provider(ctx, readonly)
		},
	}), id, nil
}

// Execute serves linear_write and linear_app_status for a verified caller.
func (w *AppWriter) Execute(parent context.Context, identity bus.Identity, r bus.Request) (bus.Result, error) {
	if w == nil || w.App == nil {
		return bus.Result{}, appRefusal("app_unavailable: this server has no ClawBridge Linear app configured")
	}
	switch r.Operation {
	case "linear_app_status":
		return w.status(parent, identity, r)
	case "linear_write":
	default:
		return bus.Result{}, appRefusal("unsupported operation")
	}
	for k := range r.Args {
		if k != "instance" && k != "operation" && k != "args" {
			return bus.Result{}, appRefusal("linear_write takes instance, operation and args only")
		}
	}
	operation, _ := r.Args["operation"].(string)
	if !appWriteOperations[operation] {
		return bus.Result{}, appRefusal("linear_write runs comment, create, update, relation_add or relation_remove only; binding and admin operations never run as the app")
	}
	args, ok := r.Args["args"].(map[string]any)
	if !ok {
		return bus.Result{}, appRefusal("linear_write requires args")
	}
	for _, k := range identityArgs {
		if _, present := args[k]; present {
			return bus.Result{}, appRefusal("linear_write cannot choose an identity; the server writes as the ClawBridge app")
		}
	}
	// The write finishes once started, even if the client goes away.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(parent), appWriteTimeout)
	defer cancel()
	service, _, err := w.service(ctx, identity)
	if err != nil {
		return bus.Result{}, err
	}
	instance, _ := r.Args["instance"].(string)
	scope := bus.Request{Workspace: r.Workspace, Project: r.Project, Session: r.Session, Args: map[string]any{"instance": instance}}
	data, err := service.ExecuteV2(ctx, scope, operation, args)
	var receipt *linearboard.ReceiptError
	switch {
	case err == nil:
		return bus.Result{Data: map[string]any{"receipt": data}}, nil
	case data != nil && errors.As(err, &receipt):
		return bus.Result{Data: map[string]any{"receipt": data, "outcome": receipt.Outcome}}, nil
	}
	return bus.Result{}, writeError(err)
}

// writeError gives the caller the board's own message: ClawBridge's text,
// never Linear's. An operation another request is still sending is
// in_progress (retry it shortly), one with an uncertain outcome a conflict
// (inspect Linear first), anything else a refusal.
func writeError(err error) error {
	if _, public := bus.PublicMessage(err); public {
		return err
	}
	message := err.Error()
	if safe, ok := bus.SafePublicText(message); ok {
		message = safe
	} else {
		message = "the Linear write was refused"
	}
	if linearboard.IsInProgress(err) {
		return bus.PublicError("in_progress", message)
	}
	if linearboard.IsUncertain(err) {
		return bus.PublicError("conflict", message)
	}
	return bus.PublicError("invalid", message)
}

// status reports the app's identity and, for the caller's project, which
// bound teams the app can see.
func (w *AppWriter) status(parent context.Context, identity bus.Identity, r bus.Request) (bus.Result, error) {
	ctx, cancel := context.WithTimeout(parent, 20*time.Second)
	defer cancel()
	service, id, err := w.service(ctx, identity)
	if err != nil {
		return bus.Result{}, err
	}
	out := map[string]any{"configured": true, "app_user_id": id.AppUserID, "linear_org_id": id.LinearOrgID}
	if r.Project == "" {
		return bus.Result{Data: out}, nil
	}
	instance, _ := r.Args["instance"].(string)
	b, err := service.BindingV2(ctx, bus.Request{Workspace: r.Workspace, Project: r.Project, Session: r.Session, Args: map[string]any{"instance": instance}})
	if err != nil {
		out["binding"] = err.Error()
		return bus.Result{Data: out}, nil
	}
	p, err := w.App.Provider(ctx, true)
	if err != nil {
		return bus.Result{}, bus.PublicError("unavailable", "the ClawBridge Linear app could not connect")
	}
	defer p.Close()
	teams := map[string]bool{}
	for _, team := range b.LinearTeamIDs {
		t, err := p.GetTeam(ctx, team)
		teams[team] = err == nil && t.ID == team
		var pe *linearprovider.Error
		if err != nil && !errors.As(err, &pe) {
			return bus.Result{}, bus.PublicError("unavailable", "the ClawBridge Linear app could not read the bound teams")
		}
	}
	out["teams_visible"] = teams
	return bus.Result{Data: out}, nil
}

// appWriter configures the ClawBridge Linear app from the environment only:
// CLAWBRIDGE_LINEAR_APP_CLIENT_ID, CLAWBRIDGE_LINEAR_APP_CLIENT_SECRET and
// CLAWBRIDGE_LINEAR_APP_TOKEN_KEY (32 bytes, base64). Without them the app
// is off and agent writes stay with each client's own profile.
func appWriter(store *bus.Store) (*AppWriter, error) {
	id, secret, rawKey := os.Getenv("CLAWBRIDGE_LINEAR_APP_CLIENT_ID"), os.Getenv("CLAWBRIDGE_LINEAR_APP_CLIENT_SECRET"), os.Getenv("CLAWBRIDGE_LINEAR_APP_TOKEN_KEY")
	if id == "" && secret == "" && rawKey == "" {
		return nil, nil
	}
	key, err := base64.StdEncoding.DecodeString(rawKey)
	if err != nil {
		return nil, errors.New("CLAWBRIDGE_LINEAR_APP_TOKEN_KEY must be 32 bytes, base64-encoded")
	}
	app, err := linearapp.New(linearapp.Config{ClientID: id, Secret: secret, Key: key, Store: store})
	if err != nil {
		return nil, err
	}
	log.Printf("clawbridge: linear app: configured")
	return &AppWriter{App: app, Store: store}, nil
}
