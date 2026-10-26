package watcher

import (
	"context"
	"time"

	"clawbridge/internal/bus"
	"clawbridge/internal/linearboard"
)

// Project health (live/PROJECT-HEALTH-DESIGN.md r2): after the comment
// watch, markers and intake, under the same lease, a tick runs the due
// health of at most HealthRunsPerTick Linear projects across all projects
// (HP-M3). A run reads Linear as the app; the bus computes the health from
// its typed facts. A health the bus decides to post is sent as one project
// status update and settled in the bus's ledger; anything else, and any
// refusal, goes to the coordinator session (HP-M2).
const HealthRunsPerTick = 2

// The GraphQL reads, constant and read-only. History is newest first
// (probe I4).
const (
	healthProjectQuery  = `query HealthProject($id: String!) { project(id: $id) { id targetDate startDate createdAt projectUpdates(first: 5, orderBy: createdAt) { nodes { id createdAt user { id } } } } }`
	healthIssuesQuery   = `query HealthIssues($id: ID!, $after: String) { issues(first: 50, after: $after, filter: { project: { id: { eq: $id } } }) { nodes { id identifier updatedAt state { type } inverseRelations(first: 20) { nodes { type issue { state { type } } } } } pageInfo { hasNextPage endCursor } } }`
	healthActivityQuery = `query HealthActivity($id: String!, $since: DateTimeOrDuration!) { issue(id: $id) { history(first: 20) { nodes { createdAt actorId botActor { id } } } comments(first: 20, filter: { createdAt: { gt: $since } }) { nodes { createdAt user { id } botActor { id } } } } }`
)

// HealthProjectQuery is the judgement's project read, exported unchanged
// for the operator's order probe (linear-app-probe --health-order).
const HealthProjectQuery = healthProjectQuery

// HealthRun is what one tick's health run did for a Linear project.
type HealthRun struct {
	LinearProject string `json:"linear_project"`
	Health        string `json:"health,omitempty"`
	Acted         string `json:"acted,omitempty"`
	// Posted is what became of a post to Linear: accepted, refused or
	// pending (uncertain, settled by a later run).
	Posted string `json:"posted,omitempty"`
	Calls  int    `json:"calls"`
	Error  string `json:"error,omitempty"`
}

// health runs the project's due health runs, at most left of them; it
// returns the runs it made.
func (w *Watcher) health(ctx context.Context, svc *linearboard.Service, scope bus.Request, pid string, fence map[string]any, left int) ([]HealthRun, error) {
	if left <= 0 {
		return nil, nil
	}
	due := map[string]any{"limit": left}
	for k, v := range fence {
		due[k] = v
	}
	r, err := w.Store.ExecuteSystem(ctx, pid, bus.Request{Operation: "health_due", Args: due})
	if err != nil {
		return nil, err
	}
	data, _ := r.Data.(map[string]any)
	list, _ := data["runs"].([]any)
	if len(list) == 0 {
		return nil, nil
	}
	app, err := w.App.Identity(ctx)
	runs := []HealthRun{}
	for _, item := range list {
		m, _ := item.(map[string]any)
		lp, _ := m["linear_project"].(string)
		days := 5
		if n, ok := m["stalled_days"].(int); ok && n > 0 {
			days = n
		}
		run := HealthRun{LinearProject: lp}
		report := map[string]any{"linear_project": lp}
		if err != nil {
			run.Error = "app_identity"
		} else {
			facts, calls, e := w.healthFacts(ctx, lp, app.AppUserID, days)
			run.Calls = calls
			if e != "" {
				run.Error = e
			} else {
				report["facts"] = facts
			}
		}
		report["calls"] = run.Calls
		if run.Error != "" {
			report["error"] = run.Error
		}
		for k, v := range fence {
			report[k] = v
		}
		out, e := w.Store.ExecuteSystem(ctx, pid, bus.Request{Operation: "health_report", Args: report})
		if e != nil {
			return runs, e
		}
		if d, ok := out.Data.(map[string]any); ok {
			run.Health, _ = d["health"].(string)
			run.Acted, _ = d["acted"].(string)
			if post, ok := d["post"].(map[string]any); ok {
				if run.Posted, e = w.postHealth(ctx, svc, scope, pid, lp, post, fence); e != nil {
					return append(runs, run), e
				}
			}
		}
		runs = append(runs, run)
	}
	return runs, nil
}

// postHealth sends the post the bus handed out, as one project status
// update, and settles it. Without a write grant it is refused before
// anything is sent, and the bus tells the coordinator session instead.
func (w *Watcher) postHealth(ctx context.Context, svc *linearboard.Service, scope bus.Request, pid, lp string, post, fence map[string]any) (string, error) {
	day, _ := post["day"].(string)
	body, _ := post["body"].(string)
	health, _ := post["health"].(string)
	outcome, code, id := "refused", "app_scope", ""
	if granted, err := w.App.HasScope(ctx, "write"); err != nil {
		code = "app_unavailable"
	} else if granted {
		var err error
		id, err = svc.PostStatusUpdateV2(ctx, scope, lp, body, health)
		outcome, code = linearboard.StatusUpdateOutcome(id, err)
	}
	settle := map[string]any{"linear_project": lp, "day": day, "outcome": outcome}
	if outcome == "accepted" {
		settle["update_id"] = id
	}
	if code != "" {
		settle["error"] = code
	}
	for k, v := range fence {
		settle[k] = v
	}
	r, err := w.Store.ExecuteSystem(ctx, pid, bus.Request{Operation: "health_post_settle", Args: settle})
	if err != nil {
		return "", err
	}
	state, _ := r.Data.(map[string]any)["state"].(string)
	return state, nil
}

type healthState struct {
	Type string `json:"type"`
}

// healthFacts reads one Linear project's facts: its dates, every issue's
// state type and open blockers, and for started issues updated within the
// stalled window, whether that activity was human (P9: the app's own
// writes bump updatedAt, so a human comment or a non-app history entry is
// required). At most HealthSampled issues are checked; the rest count as
// active and are reported as unverified. It returns a failure code, never
// Linear text, and its calls.
func (w *Watcher) healthFacts(ctx context.Context, lp, appUser string, stalledDays int) (map[string]any, int, string) {
	calls := 0
	var project struct {
		Project *struct {
			ID             string  `json:"id"`
			TargetDate     *string `json:"targetDate"`
			StartDate      *string `json:"startDate"`
			CreatedAt      string  `json:"createdAt"`
			ProjectUpdates *struct {
				Nodes []struct {
					ID        string `json:"id"`
					CreatedAt string `json:"createdAt"`
					User      *struct {
						ID string `json:"id"`
					} `json:"user"`
				} `json:"nodes"`
			} `json:"projectUpdates"`
		} `json:"project"`
	}
	calls++
	if err := w.App.GraphQL(ctx, healthProjectQuery, map[string]any{"id": lp}, &project); err != nil || project.Project == nil {
		return nil, calls, "project_unreadable"
	}
	start, from := "", "startDate"
	if project.Project.StartDate != nil && *project.Project.StartDate != "" {
		start = *project.Project.StartDate
	} else if created, err := time.Parse(time.RFC3339Nano, project.Project.CreatedAt); err == nil {
		start, from = created.UTC().Format(time.DateOnly), "createdAt"
	} else {
		return nil, calls, "project_unreadable"
	}
	target := ""
	if project.Project.TargetDate != nil {
		target = *project.Project.TargetDate
	}
	now := w.now()
	since := now.Add(-time.Duration(stalledDays) * 24 * time.Hour)
	total, completed, canceled, started := 0, 0, 0, 0
	stalled, blocked, recent := []string{}, []string{}, []struct{ id, identifier string }{}
	stalledCount, blockedCount := 0, 0
	var after any
	complete := false
	for page := 0; page < bus.HealthIssuePages; page++ {
		var out struct {
			Issues struct {
				Nodes []struct {
					ID               string      `json:"id"`
					Identifier       string      `json:"identifier"`
					UpdatedAt        string      `json:"updatedAt"`
					State            healthState `json:"state"`
					InverseRelations struct {
						Nodes []struct {
							Type  string `json:"type"`
							Issue *struct {
								State healthState `json:"state"`
							} `json:"issue"`
						} `json:"nodes"`
					} `json:"inverseRelations"`
				} `json:"nodes"`
				PageInfo pageInfo `json:"pageInfo"`
			} `json:"issues"`
		}
		calls++
		if err := w.App.GraphQL(ctx, healthIssuesQuery, map[string]any{"id": lp, "after": after}, &out); err != nil {
			return nil, calls, "issues_unreadable"
		}
		for _, n := range out.Issues.Nodes {
			total++
			switch n.State.Type {
			case "completed":
				completed++
				continue
			case "canceled":
				canceled++
				continue
			case "started":
				started++
				updated, err := time.Parse(time.RFC3339Nano, n.UpdatedAt)
				if err == nil && updated.Before(since) {
					// Nothing at all touched it in the window.
					stalledCount++
					stalled = appendListed(stalled, n.Identifier)
				} else {
					recent = append(recent, struct{ id, identifier string }{n.ID, n.Identifier})
				}
			}
			for _, rel := range n.InverseRelations.Nodes {
				if rel.Type == "blocks" && rel.Issue != nil && rel.Issue.State.Type != "completed" && rel.Issue.State.Type != "canceled" {
					blockedCount++
					blocked = appendListed(blocked, n.Identifier)
					break
				}
			}
		}
		if !out.Issues.PageInfo.HasNextPage {
			complete = true
			break
		}
		after = out.Issues.PageInfo.EndCursor
	}
	if !complete {
		// Counts over part of the project would misjudge it: no health.
		return nil, calls, "too_many_issues"
	}
	unverified := 0
	for i, issue := range recent {
		if i >= bus.HealthSampled {
			unverified += len(recent) - i
			break
		}
		calls++
		human, known := w.humanActivity(ctx, issue.id, appUser, since)
		switch {
		case !known:
			unverified++
		case !human:
			stalledCount++
			stalled = appendListed(stalled, issue.identifier)
		}
	}
	facts := map[string]any{"total": total, "completed": completed, "canceled": canceled, "started": started, "stalled": stalled, "stalled_count": stalledCount, "unverified": unverified, "blocked": blocked, "blocked_count": blockedCount, "target_date": target, "start_date": start, "start_from": from}
	// The newest updates and whether the app wrote each: the bus never
	// posts over a person's, and finds an uncertain post among them.
	if u := project.Project.ProjectUpdates; u != nil && len(u.Nodes) <= bus.HealthUpdatesRead {
		updates := []any{}
		for _, n := range u.Nodes {
			updates = append(updates, map[string]any{"id": n.ID, "created_at": n.CreatedAt, "by_app": n.User != nil && n.User.ID == appUser})
		}
		facts["updates"] = updates
	}
	return facts, calls, ""
}

// humanActivity reads one issue's newest history and its comments since:
// human is whether a person (not the app, not a bot) changed or commented
// on it since; known is false when that could not be told (the read
// failed, or every history entry read is newer than since).
func (w *Watcher) humanActivity(ctx context.Context, issue, appUser string, since time.Time) (human, known bool) {
	type actor struct {
		ID string `json:"id"`
	}
	var out struct {
		Issue *struct {
			History struct {
				Nodes []struct {
					CreatedAt string `json:"createdAt"`
					ActorID   string `json:"actorId"`
					BotActor  *actor `json:"botActor"`
				} `json:"nodes"`
			} `json:"history"`
			Comments struct {
				Nodes []struct {
					CreatedAt string `json:"createdAt"`
					User      *actor `json:"user"`
					BotActor  *actor `json:"botActor"`
				} `json:"nodes"`
			} `json:"comments"`
		} `json:"issue"`
	}
	if err := w.App.GraphQL(ctx, healthActivityQuery, map[string]any{"id": issue, "since": since.UTC().Format(time.RFC3339Nano)}, &out); err != nil || out.Issue == nil {
		return false, false
	}
	person := func(id string, bot *actor) bool { return id != "" && id != appUser && bot == nil }
	for _, c := range out.Issue.Comments.Nodes {
		created, err := time.Parse(time.RFC3339Nano, c.CreatedAt)
		if err == nil && created.After(since) && c.User != nil && person(c.User.ID, c.BotActor) {
			return true, true
		}
	}
	reachedOlder := false
	for _, h := range out.Issue.History.Nodes {
		created, err := time.Parse(time.RFC3339Nano, h.CreatedAt)
		if err != nil {
			continue
		}
		if !created.After(since) {
			reachedOlder = true
			break
		}
		if person(h.ActorID, h.BotActor) {
			return true, true
		}
	}
	// A full page of newer entries leaves older human changes unseen.
	return false, reachedOlder || len(out.Issue.History.Nodes) < 20
}

func appendListed(list []string, id string) []string {
	if len(list) < 10 {
		return append(list, id)
	}
	return list
}
