package server

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"log"
	"math"
	"strings"
	"sync/atomic"
	"time"

	"clawbridge/internal/bus"
	"clawbridge/internal/linearapp"
	"clawbridge/internal/linearpace"
	"clawbridge/internal/linearprovider"
)

// linear_issue_history (FR21): an issue's history, read as the ClawBridge
// Linear app on the hosted server, for any project role, from a session or
// the person. The client proves the issue in its own scope first; the
// server reads the issue's team and project as the app and checks them
// against the stored binding before it reads any history. Linear's MCP
// tools read no history, so without the app there is none.

// historyCallCap bounds the GraphQL calls one request asks for: the scope
// read and one history page, with one to spare.
const historyCallCap = 3

// historyChargeCap bounds one request's charge: its GraphQL calls with a
// re-send each (a token re-mint or a 429 inside App.GraphQL). A request
// that made more is charged the cap, never nothing.
const historyChargeCap = 2 * historyCallCap

// historyTimeout bounds one request. It stays below the remote client's
// 20-second HTTP timeout, so the client hears the server's answer (and the
// charge is made) instead of giving up while the read still runs.
const historyTimeout = 15 * time.Second

// History cursor bounds: the same as the board's V2 cursors.
const (
	historyCursorMax       = 8192
	historyCursorRawMax    = 6144
	historyLinearCursorMax = 4096
)

// Refusals a client may show as they are. Out of scope, not found and a
// linked-only team are told apart on purpose: issue UUIDs are unguessable,
// so the texts say nothing a caller could probe for, and they are the
// board's own, which the client already shows for its own scope check.
const (
	historyOutsideScope  = "issue is outside the selected task-board scope"
	historyLinkedOnly    = "linked_only: this binding shows the issue's team only as linked issues; nothing was read in full"
	historyPairOutside   = "issue project/team pair is outside the verified scope"
	historyNotInBinding  = "issue is outside the selected task-board scope: no issue UUID in this binding"
	historyBadCursor     = "invalid_cursor: the cursor is not for this issue or this binding revision; read the history again without one"
	historyNotLinked     = "task-board is not linked; nothing was read"
	historyArgs          = "linear_issue_history takes instance, issue_uuid, limit and cursor only"
	historyIssueRequired = "linear_issue_history requires a canonical issue UUID"
	historyLimitRange    = "limit must be 1–50"
	historyCursorType    = "cursor must be a string of up to 8192 characters"
)

// The causes of an unavailable read. Callers never see them: an
// unavailable answer carries responseMessage's generic text, so these
// reach the server's logs and the tests only.
const (
	historyUnreadable = "the ClawBridge Linear app could not read the issue's history"
	historyCallsSpent = "the issue's history needs more than 3 Linear calls; nothing more was read"
)

// historyCursor is ClawBridge's own cursor: Linear's, bound to the issue,
// the project and the binding revision it was read under.
type historyCursor struct {
	Version  int    `json:"v"`
	Issue    string `json:"issue"`
	Project  string `json:"project"`
	Revision int64  `json:"revision"`
	After    string `json:"after"`
}

func encodeHistoryCursor(c historyCursor) (string, error) {
	c.Version = 1
	raw, err := json.Marshal(c)
	if err != nil || len(raw) > historyCursorRawMax || c.After == "" || len(c.After) > historyLinearCursorMax {
		return "", errors.New("history cursor exceeds its bound")
	}
	s := base64.RawURLEncoding.EncodeToString(raw)
	if len(s) > historyCursorMax {
		return "", errors.New("history cursor exceeds its bound")
	}
	return s, nil
}

// decodeHistoryCursor accepts only a cursor encodeHistoryCursor wrote for
// this issue, project and binding revision; "" is the newest page.
func decodeHistoryCursor(opaque, issue, project string, revision int64) (string, error) {
	if opaque == "" {
		return "", nil
	}
	refused := appRefusal(historyBadCursor)
	if len(opaque) > historyCursorMax {
		return "", refused
	}
	raw, err := base64.RawURLEncoding.DecodeString(opaque)
	if err != nil || len(raw) > historyCursorRawMax {
		return "", refused
	}
	var c historyCursor
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if d.Decode(&c) != nil || d.Decode(new(any)) != io.EOF {
		return "", refused
	}
	if c.Version != 1 || c.Issue != issue || c.Project != project || c.Revision != revision || c.After == "" || len(c.After) > historyLinearCursorMax {
		return "", refused
	}
	return c.After, nil
}

// historyRequest reads the request's arguments; nothing is read before they
// pass.
func historyRequest(args map[string]any) (issue string, limit int, cursor string, err error) {
	for k := range args {
		if k != "instance" && k != "issue_uuid" && k != "limit" && k != "cursor" {
			return "", 0, "", appRefusal(historyArgs)
		}
	}
	issue, _ = args["issue_uuid"].(string)
	if !linearprovider.ValidID(issue) {
		return "", 0, "", appRefusal(historyIssueRequired)
	}
	limit = linearapp.HistoryDefaultLimit
	if v, present := args["limit"]; present {
		n, ok := wholeNumber(v)
		if !ok || n < 1 || n > linearapp.HistoryMaxLimit {
			return "", 0, "", appRefusal(historyLimitRange)
		}
		limit = n
	}
	if v, present := args["cursor"]; present {
		s, ok := v.(string)
		if !ok || len(s) > historyCursorMax {
			return "", 0, "", appRefusal(historyCursorType)
		}
		cursor = s
	}
	return issue, limit, cursor, nil
}

// wholeNumber reads a JSON number that is a whole number.
func wholeNumber(v any) (int, bool) {
	switch n := v.(type) {
	case int:
		return n, true
	case int64:
		if n < math.MinInt32 || n > math.MaxInt32 {
			return 0, false
		}
		return int(n), true
	case float64:
		if n != math.Trunc(n) || n < math.MinInt32 || n > math.MaxInt32 {
			return 0, false
		}
		return int(n), true
	case json.Number:
		i, err := n.Int64()
		if err != nil || i < math.MinInt32 || i > math.MaxInt32 {
			return 0, false
		}
		return int(i), true
	}
	return 0, false
}

// historyScope checks an issue's team and project, as the app read them,
// against the stored binding: a readable team (write, read or comment)
// and, in a project set, one of the binding's projects. The texts are the
// board's own.
func historyScope(b bus.TaskboardBindingV2, s linearapp.IssueScope) error {
	if !s.Found {
		return appRefusal(historyNotInBinding)
	}
	if !b.TeamReadable(s.TeamID) {
		if b.TeamAccess(s.TeamID) == bus.TeamAccessLinked {
			return appRefusal(historyLinkedOnly)
		}
		return appRefusal(historyOutsideScope)
	}
	if b.Mode == "project_set" {
		for _, p := range b.LinearProjectIDs {
			if s.ProjectID != "" && strings.EqualFold(p, s.ProjectID) {
				return nil
			}
		}
		return appRefusal(historyPairOutside)
	}
	return nil
}

// historyCalls bounds a request's GraphQL calls by what it asks, not by
// the HTTP requests they take: a token re-mint or a 429 re-send inside
// App.GraphQL is not counted here (the pacer counts those, for charging).
func historyCalls(gql linearapp.GraphQLFunc, max int) linearapp.GraphQLFunc {
	var n atomic.Int64
	return func(ctx context.Context, query string, vars map[string]any, out any) error {
		if n.Add(1) > int64(max) {
			return errHistoryCalls
		}
		return gql(ctx, query, vars, out)
	}
}

// historyLinearError is a failed app read as the caller sees it: rate
// limited with when to retry, else unavailable. No Linear text is kept.
func historyLinearError(err error) error {
	if at, ok := linearapp.RetryAt(err); ok {
		return bus.PublicError("rate_limited", "the ClawBridge Linear app is rate limited; nothing more was read; retry at "+at.UTC().Format(time.RFC3339))
	}
	if errors.Is(err, errHistoryCalls) {
		return bus.PublicError("unavailable", historyCallsSpent)
	}
	return bus.PublicError("unavailable", historyUnreadable)
}

var errHistoryCalls = errors.New("history call cap reached")

// issueHistory serves linear_issue_history.
func (w *AppWriter) issueHistory(parent context.Context, identity bus.Identity, r bus.Request) (bus.Result, error) {
	// historyRequest must stay the first check: the client tells an older
	// server apart by a probe with an unknown argument, which only a server
	// with this read refuses by name, before membership, Linear or a charge.
	issue, limit, opaque, err := historyRequest(r.Args)
	if err != nil {
		return bus.Result{}, err
	}
	ctx, cancel := context.WithTimeout(parent, historyTimeout)
	defer cancel()
	if err := w.member(ctx, identity, r.Workspace); err != nil {
		return bus.Result{}, err
	}
	if w.Store == nil {
		return bus.Result{}, bus.PublicError("unavailable", "the authority is unavailable; retry")
	}
	// Any project role may read, from a session (its instance checked) or
	// as the person.
	access, err := w.access(ctx, identity, r)
	if err != nil {
		return bus.Result{}, err
	}
	// Every Linear call from here on is counted, and charged under the
	// caller once the request is done, answered or not: the project is
	// known now, before any Linear call.
	var calls atomic.Int64
	ctx = linearpace.WithCounter(ctx, &calls)
	defer w.chargeHistory(context.WithoutCancel(parent), access.ProjectID, r.Session != "", &calls)
	service, id, err := w.service(ctx, identity)
	if err != nil {
		return bus.Result{}, err
	}
	instance, _ := r.Args["instance"].(string)
	b, err := service.BindingV2(ctx, bus.Request{Workspace: r.Workspace, Project: r.Project, Session: r.Session, Args: map[string]any{"instance": instance}})
	if err != nil {
		return bus.Result{}, writeError(err)
	}
	if !b.Configured {
		return bus.Result{}, appRefusal(historyNotLinked)
	}
	after, err := decodeHistoryCursor(opaque, issue, b.ProjectID, b.Revision)
	if err != nil {
		return bus.Result{}, err
	}
	gql := historyCalls(w.App.GraphQL, historyCallCap)
	scope, err := linearapp.ReadIssueScope(ctx, gql, issue)
	if err != nil {
		// Linear answers an issue the app cannot see with its
		// entity-not-found error, not an empty issue; any other failure
		// is no answer at all.
		if linearapp.IsNotFound(err) {
			return bus.Result{}, appRefusal(historyNotInBinding)
		}
		return bus.Result{}, historyLinearError(err)
	}
	if err := historyScope(b, scope); err != nil {
		return bus.Result{}, err
	}
	page, err := linearapp.ReadIssueHistory(ctx, gql, issue, limit, after)
	if err != nil {
		return bus.Result{}, historyLinearError(err)
	}
	// The issue may have moved since the scope read: checked again.
	if err := historyScope(b, page.Scope); err != nil {
		return bus.Result{}, err
	}
	names, err := w.Store.LinearUserNames(ctx, b.ProjectID, page.UserIDs())
	if err != nil {
		// Linear's names stand in for ClawBridge's.
		names = map[string]string{}
	}
	out := map[string]any{"issue_uuid": issue, "limit": limit, "entries": historyOutside(b, page.Entries(id.AppUserID, names)), "has_more": false}
	if page.HasNextPage {
		next, err := encodeHistoryCursor(historyCursor{Issue: issue, Project: b.ProjectID, Revision: b.Revision, After: page.EndCursor})
		if err != nil {
			return bus.Result{}, bus.PublicError("unavailable", historyUnreadable)
		}
		out["has_more"], out["next_cursor"] = true, next
	}
	return bus.Result{Data: out}, nil
}

// historyOutside hides the teams and projects outside the binding: a team
// side whose team the binding doesn't list, or a project side whose
// project isn't one of the binding's (a team board binds none), reads as
// outside, with no ID, key or name. In-binding sides and every other field
// stand; a parent is not checked, since the read doesn't give its team.
func historyOutside(b bus.TaskboardBindingV2, entries []linearapp.HistoryEntry) []linearapp.HistoryEntry {
	teams, projects := b.ListedTeamIDs(), b.LinearProjectIDs
	team := func(t *linearapp.HistoryTeam) *linearapp.HistoryTeam {
		if t == nil || containsFold(teams, t.ID) {
			return t
		}
		return &linearapp.HistoryTeam{Outside: true}
	}
	project := func(p *linearapp.HistoryNamed) *linearapp.HistoryNamed {
		if p == nil || containsFold(projects, p.ID) {
			return p
		}
		return &linearapp.HistoryNamed{Outside: true}
	}
	for i := range entries {
		if c := entries[i].Team; c != nil {
			entries[i].Team = &linearapp.HistoryChange[linearapp.HistoryTeam]{From: team(c.From), To: team(c.To)}
		}
		if c := entries[i].Project; c != nil {
			entries[i].Project = &linearapp.HistoryChange[linearapp.HistoryNamed]{From: project(c.From), To: project(c.To)}
		}
	}
	return entries
}

// containsFold matches IDs ignoring case on purpose: Linear's UUIDs are
// lower case, but a binding may have stored one as typed.
func containsFold(list []string, id string) bool {
	for _, x := range list {
		if id != "" && strings.EqualFold(x, id) {
			return true
		}
	}
	return false
}

// chargeHistory counts the request's Linear calls into the project's hour
// under the caller, as app_read.history: to agents from a session, else to
// people, as their writes are. The charge is the server's alone; no client
// can send it. A charge that fails is logged; the read stands.
func (w *AppWriter) chargeHistory(ctx context.Context, pid string, session bool, calls *atomic.Int64) {
	n := calls.Load()
	if n <= 0 {
		return
	}
	by := "people"
	if session {
		by = "agents"
	}
	if err := w.Store.ChargeLinearBudgetBy(ctx, pid, int(min(n, historyChargeCap)), by, "app_read.history"); err != nil {
		log.Printf("clawbridge: linear budget: issue history charge failed")
	}
}
