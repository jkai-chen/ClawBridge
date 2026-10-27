package server

import (
	"context"
	"errors"
	"regexp"
	"strings"
	"time"

	"clawbridge/internal/bus"
	"clawbridge/internal/linearboard"
	"clawbridge/internal/linearprovider"
)

// Reading and setting a bound Linear project's status as the ClawBridge
// app (live/PROJECT-STATUS-PLAN.md). The bus authorizes and charges each
// call (project_status_begin), records a set before it is sent
// (project_status_claim) and what became of it (project_status_settle);
// this side reads Linear, maps the type to the workspace's status, checks
// the preview still holds, sends and reads back.

// The GraphQL reads, constant and read-only. Status names are free text
// others write: they are only ever returned as JSON fields (M6).
const (
	projectStatusQuery         = `query ProjectStatus($id: String!) { project(id: $id) { id status { id name type } } projectStatuses(first: 50) { nodes { id name type archivedAt } pageInfo { hasNextPage } } }`
	projectStatusIssuesQuery   = `query ProjectStatusIssues($id: ID!, $after: String) { issues(first: 100, after: $after, filter: { project: { id: { eq: $id } } }) { nodes { state { type } } pageInfo { hasNextPage endCursor } } }`
	projectStatusReadBackQuery = `query ProjectStatusReadBack($id: String!) { project(id: $id) { id status { id type } } }`
)

// ProjectStatus is one Linear project status.
type ProjectStatus struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Type string `json:"type"`
}

// projectIssues counts a project's issues by state type. Open are triage,
// backlog, unstarted and started (L2: backlog is open). Partial means the
// pages ran out before the issues did.
type projectIssues struct {
	Total    int            `json:"total"`
	Open     int            `json:"open"`
	ByType   map[string]int `json:"by_type"`
	Complete bool           `json:"complete"`
}

type projectStatusFacts struct {
	Status   ProjectStatus
	Statuses []ProjectStatus
	// More is a status list past its first page: a type can't be mapped.
	More   bool
	Issues projectIssues
}

var openIssueTypes = map[string]bool{"triage": true, "backlog": true, "unstarted": true, "started": true}

// projectStatusArgs are each operation's arguments.
var projectStatusArgs = map[string]map[string]bool{
	"linear_app_project_status":     {"instance": true, "linear_project": true, "suggest": true},
	"linear_app_project_status_set": {"instance": true, "linear_project": true, "type": true, "status_id": true, "operation_id": true, "confirm": true, "expected_status_id": true, "allow_open": true},
}

var projectStatusCode = regexp.MustCompile(`^[a-z_]{1,40}$`)

// projectStatusLinear is the app's Linear calls; tests replace it.
type projectStatusLinear interface {
	read(ctx context.Context, lp string) (projectStatusFacts, error)
	set(ctx context.Context, svc *linearboard.Service, scope bus.Request, lp, status string) (outcome, code string)
}

type appProjectStatus struct{ w *AppWriter }

func (w *AppWriter) projectStatusLinear() projectStatusLinear {
	if w.StatusLinear != nil {
		return w.StatusLinear
	}
	return appProjectStatus{w}
}

// read is the project's status and the status list, and its issues'
// state types over at most bus.ProjectStatusIssuePages pages.
func (a appProjectStatus) read(ctx context.Context, lp string) (projectStatusFacts, error) {
	type status struct {
		ID         string  `json:"id"`
		Name       string  `json:"name"`
		Type       string  `json:"type"`
		ArchivedAt *string `json:"archivedAt"`
	}
	var project struct {
		Project *struct {
			ID     string  `json:"id"`
			Status *status `json:"status"`
		} `json:"project"`
		ProjectStatuses *struct {
			Nodes    []status `json:"nodes"`
			PageInfo struct {
				HasNextPage bool `json:"hasNextPage"`
			} `json:"pageInfo"`
		} `json:"projectStatuses"`
	}
	unreadable := bus.PublicError("unavailable", "the ClawBridge Linear app could not read that project's status; nothing was changed")
	if err := a.w.App.GraphQL(ctx, projectStatusQuery, map[string]any{"id": lp}, &project); err != nil || project.Project == nil || project.Project.Status == nil || project.ProjectStatuses == nil || !strings.EqualFold(project.Project.ID, lp) {
		return projectStatusFacts{}, unreadable
	}
	f := projectStatusFacts{More: project.ProjectStatuses.PageInfo.HasNextPage, Issues: projectIssues{ByType: map[string]int{}}}
	st := project.Project.Status
	f.Status = ProjectStatus{strings.ToLower(st.ID), st.Name, st.Type}
	for _, n := range project.ProjectStatuses.Nodes {
		if n.ArchivedAt == nil {
			f.Statuses = append(f.Statuses, ProjectStatus{strings.ToLower(n.ID), n.Name, n.Type})
		}
	}
	var after any
	for page := 0; page < bus.ProjectStatusIssuePages; page++ {
		var out struct {
			Issues struct {
				Nodes []struct {
					State struct {
						Type string `json:"type"`
					} `json:"state"`
				} `json:"nodes"`
				PageInfo struct {
					HasNextPage bool   `json:"hasNextPage"`
					EndCursor   string `json:"endCursor"`
				} `json:"pageInfo"`
			} `json:"issues"`
		}
		if err := a.w.App.GraphQL(ctx, projectStatusIssuesQuery, map[string]any{"id": lp, "after": after}, &out); err != nil {
			// What was read stays, marked partial.
			return f, nil
		}
		for _, n := range out.Issues.Nodes {
			f.Issues.Total++
			f.Issues.ByType[n.State.Type]++
			if openIssueTypes[n.State.Type] {
				f.Issues.Open++
			}
		}
		if !out.Issues.PageInfo.HasNextPage {
			f.Issues.Complete = true
			break
		}
		after = out.Issues.PageInfo.EndCursor
	}
	return f, nil
}

// set sends the move as the app and reads the project back by status ID
// (M5), as project flow's moves are: accepted only when the read shows
// that status; refused when nothing reached Linear, the write scope
// checked first; uncertain otherwise.
func (a appProjectStatus) set(ctx context.Context, svc *linearboard.Service, scope bus.Request, lp, to string) (string, string) {
	granted, err := a.w.App.HasScope(ctx, "write")
	if err != nil {
		return "refused", "app_unavailable"
	}
	if !granted {
		return "refused", "app_scope"
	}
	outcome, code := "uncertain", ""
	if err = svc.SetProjectStatusV2(ctx, scope, lp, to); err != nil {
		outcome, code = linearboard.StatusUpdateOutcome("", err)
	}
	if outcome != "uncertain" {
		return outcome, code
	}
	var read struct {
		Project *struct {
			Status *struct {
				ID string `json:"id"`
			} `json:"status"`
		} `json:"project"`
	}
	switch e := a.w.App.GraphQL(ctx, projectStatusReadBackQuery, map[string]any{"id": lp}, &read); {
	case e != nil || read.Project == nil || read.Project.Status == nil:
		code = "read_back_failed"
	case strings.EqualFold(read.Project.Status.ID, to):
		return "accepted", ""
	case code == "":
		code = "read_back_mismatch"
	}
	return "uncertain", code
}

// projectStatusTarget is the status a set goes to: the workspace's one
// unarchived status of the type, or the one status_id names (L1). None or
// several is ambiguous: nothing is guessed.
func projectStatusTarget(f projectStatusFacts, typ, statusID string) (ProjectStatus, error) {
	if f.More {
		return ProjectStatus{}, bus.PublicError("invalid", "ambiguous: this workspace lists more than 50 project statuses; ClawBridge can't map a type among them, so nothing was changed")
	}
	var found []ProjectStatus
	for _, s := range f.Statuses {
		if s.Type == typ && (statusID == "" || s.ID == statusID) {
			found = append(found, s)
		}
	}
	switch {
	case len(found) == 1:
		return found[0], nil
	case statusID != "":
		return ProjectStatus{}, bus.PublicError("invalid", "status_id is not an unarchived project status of type "+typ)
	case len(found) == 0:
		return ProjectStatus{}, bus.PublicError("invalid", "ambiguous: this workspace has no unarchived project status of type "+typ)
	}
	return ProjectStatus{}, bus.PublicError("invalid", "ambiguous: this workspace has several unarchived project statuses of type "+typ+"; name one with status_id (see statuses in the preview)")
}

// suggestProjectStatus is today's rule over the counts: no issues,
// backlog; every issue closed, completed (canceled when none was
// completed, as project flow never calls that done); one started,
// started; else planned. A partial read proposes nothing (L5).
func suggestProjectStatus(n projectIssues) string {
	switch {
	case !n.Complete:
		return ""
	case n.Total == 0:
		return "backlog"
	case n.Open == 0 && n.ByType["completed"] == 0 && n.ByType["canceled"] == n.Total:
		return "canceled"
	case n.Open == 0:
		return "completed"
	case n.ByType["started"] > 0:
		return "started"
	}
	return "planned"
}

// projectStatus serves linear_app_project_status (read, any member or
// session) and linear_app_project_status_set (the designated coordinator
// session, or the project owner or a lead without a session).
func (w *AppWriter) projectStatus(parent context.Context, identity bus.Identity, r bus.Request) (bus.Result, error) {
	allowed := projectStatusArgs[r.Operation]
	for k := range r.Args {
		if !allowed[k] {
			return bus.Result{}, appRefusal(r.Operation + " does not take " + k)
		}
	}
	set := r.Operation == "linear_app_project_status_set"
	str := func(k string) string { v, _ := r.Args[k].(string); return v }
	flag := func(k string) (bool, error) {
		v, ok := r.Args[k]
		if !ok {
			return false, nil
		}
		b, isBool := v.(bool)
		if !isBool {
			return false, appRefusal(k + " must be true or false")
		}
		return b, nil
	}
	lp := strings.ToLower(str("linear_project"))
	if !linearprovider.ValidID(lp) {
		return bus.Result{}, appRefusal("linear_project must be a Linear project UUID")
	}
	suggest, err := flag("suggest")
	if err != nil {
		return bus.Result{}, err
	}
	confirm, err := flag("confirm")
	if err != nil {
		return bus.Result{}, err
	}
	allowOpen, err := flag("allow_open")
	if err != nil {
		return bus.Result{}, err
	}
	typ, statusID, expected := str("type"), strings.ToLower(str("status_id")), strings.ToLower(str("expected_status_id"))
	if set {
		if !bus.ProjectStatusTypes[typ] {
			return bus.Result{}, appRefusal("type must be backlog, planned, started, completed or canceled")
		}
		for k, v := range map[string]string{"status_id": statusID, "expected_status_id": expected} {
			if v != "" && !linearprovider.ValidID(v) {
				return bus.Result{}, appRefusal(k + " must be a Linear status UUID")
			}
		}
		if confirm && expected == "" {
			return bus.Result{}, appRefusal("confirm needs expected_status_id: the current status the preview showed")
		}
	}
	// G9 F1: membership before anything reaches the app.
	if err = w.member(parent, identity, r.Workspace); err != nil {
		return bus.Result{}, err
	}
	// A set finishes once started, even if the client goes away.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(parent), appWriteTimeout)
	defer cancel()
	step := func(op string, args map[string]any) (map[string]any, error) {
		args["instance"], args["linear_project"] = str("instance"), lp
		if set {
			args["to_type"], args["operation_id"] = typ, str("operation_id")
			for k, v := range map[string]string{"status_id": statusID, "expected_status_id": expected} {
				if v != "" {
					args[k] = v
				}
			}
		}
		out, e := identityExecutor{w.Store, identity}.Execute(bus.WithAppStep(ctx), bus.Request{Operation: op, Workspace: r.Workspace, Project: r.Project, Session: r.Session, Args: args})
		m, _ := out.Data.(map[string]any)
		return m, policyRefusal(e)
	}
	begin := map[string]any{}
	if set {
		begin["set"], begin["confirm"] = true, confirm
	}
	began, err := step("project_status_begin", begin)
	if err != nil {
		return bus.Result{}, err
	}
	if recorded, ok := began["recorded"].(map[string]any); ok {
		return bus.Result{Data: map[string]any{"linear_project": lp, "recorded": recorded, "flow": began["flow"]}}, nil
	}
	f, err := w.projectStatusLinear().read(ctx, lp)
	if err != nil {
		return bus.Result{}, err
	}
	out := map[string]any{"linear_project": lp, "status": f.Status, "issues": f.Issues, "flow": began["flow"]}
	if !set {
		if suggest {
			if s := suggestProjectStatus(f.Issues); s != "" {
				out["suggestion"] = s
			} else {
				out["suggestion_note"] = "the issue read was partial: no proposal"
			}
		}
		return bus.Result{Data: out}, nil
	}
	out["statuses"] = f.Statuses
	target, err := projectStatusTarget(f, typ, statusID)
	if err != nil {
		if !confirm {
			// The preview still shows the statuses to choose from.
			out["preview"], out["refused"] = true, err.Error()
			return bus.Result{Data: out}, nil
		}
		return bus.Result{}, err
	}
	out["to"] = target
	// M2: a closing set with issues open, or over a partial read, needs
	// allow_open on top of the confirm.
	needsAllowOpen := typ == "completed" && (f.Issues.Open > 0 || !f.Issues.Complete)
	if !confirm {
		out["preview"], out["expected_status_id"], out["needs_allow_open"] = true, f.Status.ID, needsAllowOpen
		return bus.Result{Data: out}, nil
	}
	// M2: the confirm is a compare-and-set on the status the preview saw.
	if f.Status.ID != expected {
		return bus.Result{}, bus.PublicError("conflict", "the project's status changed since the preview; preview again")
	}
	if target.ID == f.Status.ID {
		out["state"] = "unchanged"
		return bus.Result{Data: out}, nil
	}
	if needsAllowOpen && !allowOpen {
		return bus.Result{}, bus.PublicError("conflict", "issues of this project are open (or not all were read); add allow_open to set it completed anyway")
	}
	claimed, err := step("project_status_claim", map[string]any{"from_type": f.Status.Type, "to_status_id": target.ID})
	if err != nil {
		return bus.Result{}, err
	}
	if recorded, ok := claimed["recorded"].(map[string]any); ok {
		return bus.Result{Data: map[string]any{"linear_project": lp, "recorded": recorded, "flow": claimed["flow"]}}, nil
	}
	service, _, err := w.service(ctx, identity)
	outcome, code := "refused", "app_unavailable"
	if err == nil {
		scope := bus.Request{Workspace: r.Workspace, Project: r.Project, Session: r.Session, Args: map[string]any{"instance": str("instance")}}
		outcome, code = w.projectStatusLinear().set(ctx, service, scope, lp, target.ID)
	}
	if outcome != "accepted" && !projectStatusCode.MatchString(code) {
		code = "failed"
	}
	// The record settles even past the write's deadline.
	settleCtx, done := context.WithTimeout(context.WithoutCancel(ctx), 20*time.Second)
	defer done()
	ctx = settleCtx
	settled, err := step("project_status_settle", map[string]any{"seq": claimed["seq"], "outcome": outcome, "error": code})
	if err != nil {
		return bus.Result{}, err
	}
	out["state"], out["flow"] = outcome, settled["flow"]
	if code != "" {
		out["error"] = code
	}
	return bus.Result{Data: out}, nil
}

// policyRefusal keeps the words of a project status refusal the bus wrote
// for the caller (who may set it, the project's close rule): an HTTP
// answer shows a forbidden refusal only as a generic line, so it goes as
// invalid, prefixed forbidden. Membership refusals never reach here.
func policyRefusal(err error) error {
	var coded interface{ Code() string }
	if err == nil || !errors.As(err, &coded) || coded.Code() != "forbidden" {
		return err
	}
	message, public := bus.PublicMessage(err)
	if !public {
		return err
	}
	return bus.PublicError("invalid", "forbidden: "+message)
}
