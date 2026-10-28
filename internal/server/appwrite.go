package server

import (
	"context"
	"encoding/base64"
	"errors"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"
	"sync/atomic"
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
	// Member, when set, replaces the membership check (tests).
	Member func(ctx context.Context, identity bus.Identity, workspace string) error
	// StatusLinear, when set, replaces the app's project status calls
	// (tests).
	StatusLinear projectStatusLinear

	typesOnce sync.Once
	types     *linearboard.StateTypeCache
	// attributionFailures counts the app writes whose intake attribution
	// this process failed to record; linear_app_status reports it.
	attributionFailures atomic.Int64
}

// stateTypeTTL is how long a team's workflow state types read as the app
// are kept: the binding proof's.
const stateTypeTTL = 60 * time.Second

// stateTypes is the app's per-team state type cache, shared by requests.
func (w *AppWriter) stateTypes() *linearboard.StateTypeCache {
	w.typesOnce.Do(func() {
		w.types = linearboard.NewStateTypeCache(linearboard.GraphQLStateTypes(w.App.GraphQL), stateTypeTTL, w.Now)
	})
	return w.types
}

func appRefusal(message string) error { return bus.PublicError("invalid", message) }

// relationScope refuses a relation write the app's token may not make:
// Linear answers it with success and drops the edge, so without the write
// scope nothing is sent. has reads the token's actual grant; when it cannot,
// the write is refused with its error.
func relationScope(ctx context.Context, operation string, has func(context.Context, string) (bool, error)) error {
	if operation != "relation_add" && operation != "relation_remove" {
		return nil
	}
	ok, err := has(ctx, "write")
	if err != nil {
		return err
	}
	if !ok {
		return appRefusal("capability_unavailable: this server's ClawBridge Linear app has no write scope, so it cannot add or remove issue relations; nothing was sent to Linear")
	}
	return nil
}

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
		IssueState: linearboard.GraphQLIssueState(w.App.GraphQL),
		StateType:  w.stateTypes().Type,
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
	case "linear_app_states":
		return w.states(parent, identity, r)
	case "linear_app_project_status", "linear_app_project_status_set":
		return w.projectStatus(parent, identity, r)
	case "linear_app_subscribers_backfill":
		return w.subscribersBackfill(parent, identity, r)
	case "linear_write":
	default:
		return bus.Result{}, appRefusal("unsupported operation")
	}
	for k := range r.Args {
		if k != "instance" && k != "operation" && k != "args" && k != "agent_marker" {
			return bus.Result{}, appRefusal("linear_write takes instance, operation, args and agent_marker only")
		}
	}
	marker, err := writeMarker(r.Args)
	if err != nil {
		return bus.Result{}, err
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
	// G9 F1: after the request's own checks, and before anything reaches
	// the app, the caller proves membership; status and states do the same.
	if err := w.member(parent, identity, r.Workspace); err != nil {
		return bus.Result{}, err
	}
	if err := relationScope(parent, operation, w.App.HasScope); err != nil {
		return bus.Result{}, err
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
	if err != nil {
		// A failed write may mean the workflow changed: read types afresh.
		w.stateTypes().Drop()
	}
	var receipt *linearboard.ReceiptError
	switch {
	case err == nil:
		w.attribute(ctx, identity, r, operation, args, data, marker)
		w.queueSubscribers(ctx, identity, scope, operation, data)
		return bus.Result{Data: map[string]any{"receipt": data}}, nil
	case data != nil && errors.As(err, &receipt):
		w.attribute(ctx, identity, r, operation, args, data, marker)
		return bus.Result{Data: map[string]any{"receipt": data, "outcome": receipt.Outcome}}, nil
	case linearboard.IsUncertain(err) && operation == "update":
		// It may have reached Linear: its labels are recorded from the
		// request, on the issue it named by UUID. An uncertain create or
		// comment records nothing: there is no issue or comment ID to
		// record it on, and none is needed. A create's issue is new, so no
		// other write in its window can be credited for its labels; a
		// comment is matched by its own ID, which nothing else can carry.
		id, _ := args["id"].(string)
		w.attribute(ctx, identity, r, operation, args, linearboard.ReceiptV2{Kind: "issue", ID: id}, marker)
	}
	return bus.Result{}, writeError(err)
}

// writeMarker is the agent marker a client sent with linear_write (A2):
// the environment variable that says an agent runs it, "" for none. A
// client that sends none is an older one, and its writes are never credited
// to a person.
func writeMarker(args map[string]any) (string, error) {
	v, present := args["agent_marker"]
	if !present {
		return bus.MarkerUnsent, nil
	}
	m, ok := v.(string)
	if !ok || len(m) > 64 {
		return "", appRefusal("agent_marker must be a short string")
	}
	return m, nil
}

// attribute records who asked for a write that reached Linear and added
// labels or posted a comment (intake attribution, A1): every such write,
// a session's too. It records more labels than were added rather than
// fewer: a label set replaced on update counts whole, and a create's
// labels count even when Linear's answer left them unconfirmed. An extra
// record can only make intake's credit ambiguous; a missing one could let
// a session's write pass as a person's. A record that fails is logged; the
// write itself stands.
func (w *AppWriter) attribute(ctx context.Context, identity bus.Identity, r bus.Request, operation string, args map[string]any, data any, marker string) {
	if w.Store == nil {
		return
	}
	// A board on a legacy mapping answers with the older receipts: the
	// provider's for a comment, a map for an issue.
	var receipt linearboard.ReceiptV2
	switch v := data.(type) {
	case linearboard.ReceiptV2:
		receipt = v
	case *linearboard.ReceiptV2:
		if v == nil {
			return
		}
		receipt = *v
	case linearprovider.Receipt:
		receipt = linearboard.ReceiptV2{Kind: "comment", ID: v.ID, IssueID: v.IssueID}
	case map[string]any:
		id, _ := v["id"].(string)
		receipt = linearboard.ReceiptV2{Kind: "issue", ID: id}
	default:
		return
	}
	write := bus.AppWrite{Workspace: r.Workspace, Project: r.Project, Identity: identity, Session: r.Session, AgentMarker: marker}
	write.OperationID, _ = args["operation_id"].(string)
	switch {
	case operation == "comment" && receipt.Kind == "comment":
		write.Issue, write.Comment = receipt.IssueID, receipt.ID
	case (operation == "create" || operation == "update") && receipt.Kind == "issue":
		write.Issue = receipt.ID
		write.Labels = append(write.Labels, receipt.LabelsAdded...)
		for _, key := range []string{"label_ids", "add_label_ids"} {
			write.Labels = append(write.Labels, labelArgs(args[key])...)
		}
	default:
		return
	}
	if err := w.Store.RecordAppWrite(ctx, write); err != nil {
		w.attributionFailures.Add(1)
		log.Printf("clawbridge: linear app: the write's intake attribution was not recorded: %v", err)
	}
}

// labelArgs reads a label argument as the board accepts it: a list of IDs.
func labelArgs(v any) []string {
	out := []string{}
	switch list := v.(type) {
	case []string:
		out = append(out, list...)
	case []any:
		for _, x := range list {
			if id, ok := x.(string); ok {
				out = append(out, id)
			}
		}
	}
	return out
}

// queueSubscribers queues the project's people as subscribers of an issue
// the app just created (ISSUE-SUBSCRIBERS-PLAN §3). The write has already
// succeeded, so a queue that fails is logged by code and never fails it.
func (w *AppWriter) queueSubscribers(ctx context.Context, identity bus.Identity, scope bus.Request, operation string, receipt any) {
	// Only a create: an agent's update or comment is on an issue that was
	// created or claimed already.
	if w.Store == nil || operation != "create" {
		return
	}
	issue := ""
	switch r := receipt.(type) {
	case linearboard.ReceiptV2:
		issue = r.ID
	case linearprovider.Receipt:
		issue = r.ID
	}
	if !linearprovider.ValidID(issue) {
		return
	}
	scope.Operation = "taskboard_access"
	scope.Args = map[string]any{"instance": scope.Args["instance"], "ledger": 1}
	access, err := identityExecutor{w.Store, identity}.Execute(ctx, scope)
	data, _ := access.Data.(map[string]any)
	pid, _ := data["project_id"].(string)
	if err == nil && pid != "" {
		_, err = w.Store.ExecuteSystem(ctx, pid, bus.Request{Operation: "subscribe_queue", Args: map[string]any{"issue_uuid": issue, "reason": "create"}})
	}
	if err != nil {
		var coded interface{ Code() string }
		code := "failed"
		if errors.As(err, &coded) {
			code = coded.Code()
		}
		log.Printf("clawbridge: issue subscribers: queue after create failed: %s", code)
	}
}

// member proves identity is a member of workspace, or, with none named, of
// some workspace on this server: a bus read as identity, which applies the
// membership, the domain gate and the account binding. Clients name no
// workspace when they ask whether the server writes as the app.
func (w *AppWriter) member(ctx context.Context, identity bus.Identity, workspace string) error {
	if w.Member != nil {
		return w.Member(ctx, identity, workspace)
	}
	if w.Store == nil {
		return bus.PublicError("unavailable", "the authority is unavailable; retry")
	}
	candidates := []string{workspace}
	if workspace == "" {
		var err error
		if candidates, err = w.Store.MemberWorkspaces(ctx, identity.Principal); err != nil {
			return bus.PublicError("unavailable", "the authority is unavailable; retry")
		}
	}
	// Every refusal reads the same (R120-L1): a missing workspace and one
	// the caller isn't a member of must not be told apart. Only an
	// unavailable authority says so, to be retried.
	refused := bus.PublicError("forbidden", "workspace membership required")
	for _, ws := range candidates {
		_, err := identityExecutor{w.Store, identity}.Execute(ctx, bus.Request{Operation: "list_projects", Workspace: ws, Args: map[string]any{}})
		if err == nil {
			return nil
		}
		var coded interface{ Code() string }
		if errors.As(err, &coded) && coded.Code() == "unavailable" {
			return bus.PublicError("unavailable", "the authority is unavailable; retry")
		}
	}
	return refused
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
	if err := w.member(ctx, identity, r.Workspace); err != nil {
		return bus.Result{}, err
	}
	service, id, err := w.service(ctx, identity)
	if err != nil {
		return bus.Result{}, err
	}
	out := map[string]any{"configured": true, "app_user_id": id.AppUserID, "linear_org_id": id.LinearOrgID, "attribution_record_failures": w.attributionFailures.Load()}
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

// workflowStatesQuery reads one team's workflow states with their types,
// which Linear's MCP status list may leave out.
const workflowStatesQuery = `query WorkflowStates($team: ID!) { workflowStates(first: 100, filter: { team: { id: { eq: $team } } }) { nodes { id name type team { id } } pageInfo { hasNextPage } } }`

// states reads a bound team's workflow states and types as the app, for a
// status policy. It reads only, only a team the caller's binding names, and
// only for a project lead or owner: setting the policy is the owner's, and
// every read spends the app's shared Linear rate limit.
func (w *AppWriter) states(parent context.Context, identity bus.Identity, r bus.Request) (bus.Result, error) {
	team, _ := r.Args["team"].(string)
	for k := range r.Args {
		if k != "instance" && k != "team" {
			return bus.Result{}, appRefusal("linear_app_states takes instance and team only")
		}
	}
	if !linearprovider.ValidID(team) {
		return bus.Result{}, appRefusal("linear_app_states requires a canonical team UUID")
	}
	ctx, cancel := context.WithTimeout(parent, 20*time.Second)
	defer cancel()
	if err := w.member(ctx, identity, r.Workspace); err != nil {
		return bus.Result{}, err
	}
	service, _, err := w.service(ctx, identity)
	if err != nil {
		return bus.Result{}, err
	}
	instance, _ := r.Args["instance"].(string)
	access, err := identityExecutor{w.Store, identity}.Execute(ctx, bus.Request{Operation: "taskboard_access", Workspace: r.Workspace, Project: r.Project, Session: r.Session, Args: map[string]any{"instance": instance, "ledger": 1}})
	if err != nil {
		return bus.Result{}, err
	}
	if role, _ := access.Data.(map[string]any)["project_role"].(string); role != "owner" && role != "lead" {
		return bus.Result{}, bus.PublicError("forbidden", "reading workflow states as the ClawBridge Linear app needs a project lead or owner")
	}
	b, err := service.BindingV2(ctx, bus.Request{Workspace: r.Workspace, Project: r.Project, Session: r.Session, Args: map[string]any{"instance": instance}})
	if err != nil {
		return bus.Result{}, err
	}
	bound := false
	for _, t := range b.LinearTeamIDs {
		bound = bound || strings.EqualFold(t, team)
	}
	if !bound {
		return bus.Result{}, appRefusal("the team is not in this project's binding")
	}
	var out struct {
		WorkflowStates struct {
			Nodes []struct {
				ID, Name, Type string
				Team           struct{ ID string }
			}
			PageInfo struct{ HasNextPage bool }
		}
	}
	if err = w.App.GraphQL(ctx, workflowStatesQuery, map[string]any{"team": team}, &out); err != nil || out.WorkflowStates.PageInfo.HasNextPage {
		return bus.Result{}, bus.PublicError("unavailable", "the ClawBridge Linear app could not read the team's workflow states")
	}
	states := []any{}
	for _, s := range out.WorkflowStates.Nodes {
		if !linearprovider.ValidID(strings.ToLower(s.ID)) || !strings.EqualFold(s.Team.ID, team) {
			return bus.Result{}, bus.PublicError("unavailable", "the ClawBridge Linear app read an unverified workflow state")
		}
		// A state of a type ClawBridge does not know (Linear's duplicate
		// type, say) is left out: no policy may target it or move from it,
		// and the bus refuses a workflow that names it.
		kind := linearprovider.StatusType(s.Type)
		if kind == "" {
			continue
		}
		states = append(states, map[string]any{"id": strings.ToLower(s.ID), "name": s.Name, "type": kind, "team_id": strings.ToLower(team)})
	}
	return bus.Result{Data: map[string]any{"states": states}}, nil
}

// appConfig reads the app's settings from the environment only. set is
// false when all three are empty: the app is off.
func appConfig(store linearapp.Store) (config linearapp.Config, set bool, err error) {
	id, secret, rawKey := os.Getenv("CLAWBRIDGE_LINEAR_APP_CLIENT_ID"), os.Getenv("CLAWBRIDGE_LINEAR_APP_CLIENT_SECRET"), os.Getenv("CLAWBRIDGE_LINEAR_APP_TOKEN_KEY")
	if id == "" && secret == "" && rawKey == "" {
		return linearapp.Config{}, false, nil
	}
	key, err := base64.StdEncoding.DecodeString(rawKey)
	if err != nil {
		return linearapp.Config{}, true, errors.New("CLAWBRIDGE_LINEAR_APP_TOKEN_KEY must be 32 bytes, base64-encoded")
	}
	return linearapp.Config{ClientID: id, Secret: secret, Key: key, Store: store}, true, nil
}

// appWriter configures the ClawBridge Linear app from the environment only:
// CLAWBRIDGE_LINEAR_APP_CLIENT_ID, CLAWBRIDGE_LINEAR_APP_CLIENT_SECRET and
// CLAWBRIDGE_LINEAR_APP_TOKEN_KEY (32 bytes, base64). Without them the app
// is off and agent writes stay with each client's own profile.
func appWriter(store *bus.Store) (*AppWriter, error) {
	config, set, err := appConfig(store)
	if err != nil || !set {
		return nil, err
	}
	app, err := linearapp.New(config)
	if err != nil {
		return nil, err
	}
	log.Printf("clawbridge: linear app: configured")
	return &AppWriter{App: app, Store: store}, nil
}
