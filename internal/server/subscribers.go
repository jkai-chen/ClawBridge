package server

import (
	"context"
	"slices"
	"strings"
	"time"

	"clawbridge/internal/bus"
	"clawbridge/internal/linearprovider"
)

// The issue subscribers backfill (live/ISSUE-SUBSCRIBERS-PLAN.md §10): the
// issues the project's agents created as the app, or claimed, since a
// time, found as the app; a dry run lists them and an apply queues them,
// for the watcher to subscribe under the hour's budget like any other row.

// backfillPages bounds the read of the app's created issues.
const backfillPages = 3

// backfillIssuesQuery lists issues the app created in the bound teams since
// a time, newest first.
const backfillIssuesQuery = `query BackfillIssues($app: ID!, $teams: [ID!], $since: DateTimeOrDuration!, $after: String) { issues(first: 50, after: $after, orderBy: createdAt, filter: { creator: { id: { eq: $app } }, team: { id: { in: $teams } }, createdAt: { gte: $since } }) { nodes { id identifier team { id } project { id } } pageInfo { hasNextPage endCursor } } }`

// backfillProjectIssuesQuery is backfillIssuesQuery for a project_set
// binding: another binding can share its teams with other projects, so
// only the binding's own projects count.
const backfillProjectIssuesQuery = `query BackfillProjectIssues($app: ID!, $teams: [ID!], $projects: [ID!], $since: DateTimeOrDuration!, $after: String) { issues(first: 50, after: $after, orderBy: createdAt, filter: { creator: { id: { eq: $app } }, team: { id: { in: $teams } }, project: { id: { in: $projects } }, createdAt: { gte: $since } }) { nodes { id identifier team { id } project { id } } pageInfo { hasNextPage endCursor } } }`

func (w *AppWriter) subscribersBackfill(parent context.Context, identity bus.Identity, r bus.Request) (bus.Result, error) {
	for k := range r.Args {
		if k != "instance" && k != "since" && k != "apply" {
			return bus.Result{}, appRefusal("linear_app_subscribers_backfill takes since and apply only")
		}
	}
	if r.Session != "" {
		return bus.Result{}, bus.PublicError("forbidden", "a session can't backfill issue subscribers; the project owner or a lead runs linear subscribers backfill in their own terminal")
	}
	raw, _ := r.Args["since"].(string)
	since, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		return bus.Result{}, appRefusal("since must be an RFC 3339 time")
	}
	apply, _ := r.Args["apply"].(bool)
	ctx, cancel := context.WithTimeout(parent, 30*time.Second)
	defer cancel()
	if err := w.member(ctx, identity, r.Workspace); err != nil {
		return bus.Result{}, err
	}
	as := identityExecutor{w.Store, identity}
	scope := bus.Request{Workspace: r.Workspace, Project: r.Project, Args: map[string]any{}}
	accessScope := withOp(scope, "taskboard_access")
	accessScope.Args["ledger"] = 1
	access, err := as.Execute(ctx, accessScope)
	if err != nil {
		return bus.Result{}, err
	}
	a, _ := access.Data.(map[string]any)
	if role, _ := a["project_role"].(string); role != "owner" && role != "lead" {
		return bus.Result{}, bus.PublicError("forbidden", "backfilling issue subscribers needs a project lead or owner")
	}
	pid, _ := a["project_id"].(string)
	setting, err := as.Execute(ctx, withOp(scope, "issue_subscribers_get"))
	if err != nil {
		return bus.Result{}, err
	}
	// The dry run is a plain read, so the owner can see what a backfill would
	// do before turning the setting on; --apply queues, so it needs the
	// setting on and the server proven.
	if on, _ := setting.Data.(map[string]any)["on"].(bool); apply && !on {
		return bus.Result{}, appRefusal("issue subscribers are off for this project, or not yet proven on this server; nothing was read or queued (a dry run, without --apply, still lists the issues)")
	}
	service, id, err := w.service(ctx, identity)
	if err != nil {
		return bus.Result{}, err
	}
	b, err := service.BindingV2(ctx, scope)
	if err != nil {
		return bus.Result{}, err
	}
	type found struct{ id, identifier, reason string }
	issues, seen := []found{}, map[string]bool{}
	reads, after, more := 0, any(nil), false
	query, vars := backfillIssuesQuery, map[string]any{"app": id.AppUserID, "teams": b.LinearTeamIDs, "since": since.UTC().Format(time.RFC3339)}
	if b.Mode == "project_set" {
		query, vars["projects"] = backfillProjectIssuesQuery, b.LinearProjectIDs
	}
	for page := 0; page < backfillPages; page++ {
		var out struct {
			Issues struct {
				Nodes []struct {
					ID, Identifier string
					Team           struct{ ID string }
					Project        *struct{ ID string }
				}
				PageInfo struct {
					HasNextPage bool
					EndCursor   string
				}
			}
		}
		reads++
		vars["after"] = after
		if err = w.App.GraphQL(ctx, query, vars, &out); err != nil {
			return bus.Result{}, bus.PublicError("unavailable", "the ClawBridge Linear app could not list the issues it created")
		}
		for _, n := range out.Issues.Nodes {
			issue := strings.ToLower(n.ID)
			// Linear's filter decides, and each row is checked again: an issue
			// outside the binding's pairs is never listed.
			if !inBinding(b, n.Team.ID, n.Project) {
				continue
			}
			if linearprovider.ValidID(issue) && !seen[issue] {
				seen[issue] = true
				issues = append(issues, found{issue, n.Identifier, "create"})
			}
		}
		if more = out.Issues.PageInfo.HasNextPage; !more {
			break
		}
		after = out.Issues.PageInfo.EndCursor
	}
	claims, err := w.Store.ExecuteSystem(ctx, pid, bus.Request{Operation: "subscribe_claims", Args: map[string]any{"since": since.UTC().Format("2006-01-02T15:04:05.000000000Z07:00")}})
	if err != nil {
		return bus.Result{}, err
	}
	rows, _ := claims.Data.(map[string]any)["claims"].([]map[string]any)
	for _, c := range rows {
		issue, _ := c["issue_uuid"].(string)
		identifier, _ := c["issue_identifier"].(string)
		if linearprovider.ValidID(issue) && !seen[issue] {
			seen[issue] = true
			issues = append(issues, found{issue, identifier, "claim"})
		}
	}
	if _, err = w.Store.ExecuteSystem(ctx, pid, bus.Request{Operation: "subscribe_charge", Args: map[string]any{"calls": reads}}); err != nil {
		return bus.Result{}, err
	}
	people := 0
	if s, _ := setting.Data.(map[string]any); s != nil {
		if s["owner"] == true {
			people++
		}
		switch ids := s["user_ids"].(type) {
		case []string:
			people += len(ids)
		case []any:
			people += len(ids)
		}
	}
	list, queued := []any{}, 0
	for _, f := range issues {
		item := map[string]any{"identifier": f.identifier, "reason": f.reason}
		if apply {
			q, err := w.Store.ExecuteSystem(ctx, pid, bus.Request{Operation: "subscribe_queue", Args: map[string]any{"issue_uuid": f.id, "reason": f.reason}})
			if err != nil {
				return bus.Result{}, err
			}
			isNew, _ := q.Data.(map[string]any)["queued"].(bool)
			item["queued"] = isNew
			if isNew {
				queued++
			}
		}
		list = append(list, item)
	}
	return bus.Result{Data: map[string]any{"issues": list, "count": len(issues), "applied": apply, "queued": queued, "read_calls": reads,
		"subscribe_calls": len(issues) * bus.SubscribeRowCalls(people), "more": more}}, nil
}

func withOp(r bus.Request, op string) bus.Request {
	r.Operation = op
	args := map[string]any{}
	for k, v := range r.Args {
		args[k] = v
	}
	r.Args = args
	return r
}

// inBinding says an issue's team and project are within the binding: a
// bound team, and for a project_set binding one of its projects.
func inBinding(b bus.TaskboardBindingV2, team string, project *struct{ ID string }) bool {
	if !slices.ContainsFunc(b.LinearTeamIDs, func(t string) bool { return strings.EqualFold(t, team) }) {
		return false
	}
	if b.Mode != "project_set" {
		return true
	}
	return project != nil && slices.ContainsFunc(b.LinearProjectIDs, func(p string) bool { return strings.EqualFold(p, project.ID) })
}
