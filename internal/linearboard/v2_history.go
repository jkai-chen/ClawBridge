package linearboard

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	"clawbridge/internal/bus"
	"clawbridge/internal/linearapp"
	"clawbridge/internal/linearprovider"
)

// An issue's history (FR21) is read by the hosted server as the ClawBridge
// Linear app: Linear's MCP tools read none. The client proves the issue in
// its own scope first, as `linear issue` does, which also turns an
// identifier into the UUID; the server checks the issue against the
// stored binding again before it reads any history.

// IssueHistoryInputV2 is one page's request: Limit 1–50 (0 for the
// server's default, 20) and the previous page's next cursor.
type IssueHistoryInputV2 struct {
	Limit  int
	Cursor string
}

// IssueHistoryPageV2 is one page of an issue's history, newest first:
// typed fields only, never a title, description, comment text or email.
type IssueHistoryPageV2 struct {
	IssueUUID string `json:"issue_uuid"`
	// Identifier is the issue's display identifier, from the client's
	// own read, when Linear gave one.
	Identifier string                   `json:"identifier,omitempty"`
	Limit      int                      `json:"limit"`
	HasMore    bool                     `json:"has_more"`
	NextCursor string                   `json:"next_cursor,omitempty"`
	Entries    []linearapp.HistoryEntry `json:"entries"`
}

// historyOperation is the server's operation.
const historyOperation = "linear_issue_history"

// historyCursorMax bounds a cursor before it is sent: the server's bound.
const historyCursorMax = 8192

// errHistoryOldServer answers a server that predates the read.
var errHistoryOldServer = errors.New("the ClawBridge server is older than this client and has no issue history read; upgrade the server")

// historyOutOfScope are the server's refusals of an issue its binding
// doesn't cover: the board's own out-of-scope texts, so they are shown,
// and told apart, as the client's own scope check is.
var historyOutOfScope = map[string]bool{
	"issue is outside the selected task-board scope":                                                   true,
	"issue is outside the selected task-board scope: no issue UUID in this binding":                    true,
	"linked_only: this binding shows the issue's team only as linked issues; nothing was read in full": true,
	"issue project/team pair is outside the verified scope":                                            true,
}

// historyRefusal is the server's refusal, shown as its own text; the
// transport's error stays underneath for callers that look for it.
type historyRefusal struct {
	message string
	err     error
}

func (e historyRefusal) Error() string { return e.message }
func (e historyRefusal) Unwrap() error { return e.err }

// IssueHistoryV2 reads one page of an issue's history by UUID or
// identifier. The issue is read and judged against the verified binding
// first, by the same read as IssueV2; only its proven UUID is sent to the
// server.
func (s *Service) IssueHistoryV2(ctx context.Context, scope bus.Request, ref string, in IssueHistoryInputV2) (IssueHistoryPageV2, error) {
	empty := IssueHistoryPageV2{}
	if !linearprovider.ValidID(ref) && !linearprovider.ValidIssueIdentifier(ref) {
		return empty, errIssueRef
	}
	if in.Limit != 0 && (in.Limit < 1 || in.Limit > linearapp.HistoryMaxLimit) {
		return empty, errors.New("history limit must be 1–50")
	}
	if len(in.Cursor) > historyCursorMax {
		return empty, errors.New("history cursor must be up to 8192 characters")
	}
	// The proof is IssueV2 itself, the read `linear issue` makes, so the
	// two can't drift. It returns with the profile's connection released
	// (issueV2 defers it), before the server is asked.
	issue, e := s.IssueV2(ctx, scope, ref)
	if e != nil {
		return empty, e
	}
	request := bus.Request{Operation: historyOperation, Workspace: scope.Workspace, Project: scope.Project, Session: scope.Session, Args: map[string]any{"issue_uuid": strings.ToLower(issue.ID)}}
	if instance, _ := scope.Args["instance"].(string); instance != "" && scope.Session != "" {
		request.Args["instance"] = instance
	}
	if in.Limit != 0 {
		request.Args["limit"] = in.Limit
	}
	if in.Cursor != "" {
		request.Args["cursor"] = in.Cursor
	}
	r, e := s.cfg.Executor.Execute(ctx, request)
	if e != nil {
		if historyGenericRefusal(e) && s.historyOlderServer(ctx, request) {
			return empty, errHistoryOldServer
		}
		return empty, historyError(e)
	}
	if r.Source != "server" {
		return empty, errors.New("an issue's history is read by the ClawBridge server only")
	}
	raw, e := json.Marshal(r.Data)
	if e != nil {
		return empty, errors.New("the server's issue history is unreadable")
	}
	var page IssueHistoryPageV2
	if json.Unmarshal(raw, &page) != nil {
		return empty, errors.New("the server's issue history is unreadable")
	}
	if page.IssueUUID != strings.ToLower(issue.ID) {
		return empty, errors.New("the server answered with another issue's history")
	}
	if !page.HasMore {
		page.NextCursor = ""
	} else if page.NextCursor == "" {
		return empty, errors.New("the server's issue history says more follow but gives no cursor")
	}
	page.Identifier = ""
	if linearprovider.ValidIssueIdentifier(issue.Identifier) {
		page.Identifier = issue.Identifier
	}
	if page.Entries == nil {
		page.Entries = []linearapp.HistoryEntry{}
	}
	return page, nil
}

// historyGenericRefusal reports a hosted server's refusal that names no
// reason: what a server that predates the read answers for an operation
// it doesn't know.
func historyGenericRefusal(e error) bool {
	var rejected rejection
	if !errors.As(e, &rejected) {
		return false
	}
	status, code, message := rejected.Rejection()
	return status == 400 && code == "invalid" && (message == "" || strings.HasPrefix(message, "request rejected;"))
}

// historyOlderServer tells a server that predates the read from one that
// refused the request for a reason it doesn't show: a request with an
// argument the read never takes is refused by name by a server that has
// it, before anything is read or charged, and only generically by one
// that doesn't know the operation.
func (s *Service) historyOlderServer(ctx context.Context, request bus.Request) bool {
	probe := request
	probe.Args = map[string]any{"issue_uuid": request.Args["issue_uuid"], "probe": 1}
	if instance, ok := request.Args["instance"]; ok {
		probe.Args["instance"] = instance
	}
	_, e := s.cfg.Executor.Execute(ctx, probe)
	return e != nil && historyGenericRefusal(e)
}

// historyError is how a refused history read is shown: an older server
// says so; the server's out-of-scope refusals are the board's own; any
// other refusal the server explained is its text.
func historyError(e error) error {
	if strings.Contains(e.Error(), `unsupported operation "`+historyOperation+`"`) {
		return errHistoryOldServer
	}
	var rejected rejection
	if errors.As(e, &rejected) {
		_, code, message := rejected.Rejection()
		if code == "invalid" && historyOutOfScope[message] {
			return outOfScopeError(message)
		}
		if message != "" {
			return historyRefusal{message: message, err: e}
		}
	}
	return e
}
