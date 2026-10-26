package watcher

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"time"

	"clawbridge/internal/bus"
	"clawbridge/internal/linearboard"
)

// Intake reads (live/INTAKE-DESIGN.md §1, §12 probes I1–I4): S1 is the
// needs-agent label per bound team, S2 issues delegated to the app, S3
// mentions of the app. Each candidate is proven inside the binding, and its
// transition's actor is proven from Linear's own record, before it is
// queued. No issue title, description, comment or notification text is
// ever read for routing.
const (
	needsAgentLabel = "needs-agent"
	// intakeLag re-reads the last ten minutes, as comment reads do, so a
	// change visible late is not skipped; the queue dedupes repeats.
	intakeLag = 10 * time.Minute
	// intakePages bounds each source's read per poll.
	intakePages = 4
	// historyPages bounds the actor proof's history walk (20 entries a
	// page, newest first).
	historyPages = 3
	// labelCacheTTL is how long a team's needs-agent label ID is reused.
	labelCacheTTL = time.Hour
)

// The GraphQL reads, constant and read-only. History and notifications are
// newest first (probe I4): `first`, never `last`.
const (
	delegatedQuery = `query Delegated($app: ID!, $since: DateTimeOrDuration!, $after: String) { issues(first: 50, after: $after, filter: { delegate: { id: { eq: $app } }, updatedAt: { gt: $since } }) { nodes { id updatedAt } pageInfo { hasNextPage endCursor } } }`
	historyQuery   = `query History($id: String!, $first: Int!, $after: String) { issue(id: $id) { history(first: $first, after: $after) { nodes { createdAt actorId botActor { id } addedLabelIds toDelegate { id } } pageInfo { hasNextPage endCursor } } } }`
	mentionsQuery  = `query Mentions($first: Int!, $after: String) { notifications(first: $first, after: $after) { nodes { id type createdAt ... on IssueNotification { issue { id } actor { id } } } pageInfo { hasNextPage endCursor } } }`
)

// IntakePoll is what one tick's intake did for a project.
type IntakePoll struct {
	Queued     []string       `json:"queued,omitempty"`
	Skipped    map[string]int `json:"skipped,omitempty"`
	Calls      int            `json:"calls"`
	Off        bool           `json:"off,omitempty"`
	NoLabel    []string       `json:"no_label_teams,omitempty"`
	Unreadable []string       `json:"unreadable_sources,omitempty"`
	// Truncated sources had more than intakePages pages; their cursor
	// stays, so what was not reached is read again next poll.
	Truncated []string `json:"truncated_sources,omitempty"`
}

func (p *IntakePoll) skip(why string) {
	if p.Skipped == nil {
		p.Skipped = map[string]int{}
	}
	p.Skipped[why]++
}

type pageInfo struct {
	HasNextPage bool   `json:"hasNextPage"`
	EndCursor   string `json:"endCursor"`
}

// intakeRun is one project's intake poll.
type intakeRun struct {
	w     *Watcher
	svc   *linearboard.Service
	scope bus.Request
	pid   string
	app   AppIdentity
	state bus.IntakeState
	poll  *IntakePoll
}

// intake polls the project's due intake sources under the watch lease and
// charges their Linear calls to its budget.
func (w *Watcher) intake(ctx context.Context, svc *linearboard.Service, scope bus.Request, pid string, fence map[string]any) (*IntakePoll, error) {
	poll := &IntakePoll{}
	st, err := w.Store.IntakeSystemState(ctx, pid)
	if err != nil {
		return poll, err
	}
	if st.Mode == "off" {
		poll.Off = true
		return poll, nil
	}
	app, err := w.App.Identity(ctx)
	if err != nil {
		poll.Unreadable = append(poll.Unreadable, "app_identity")
		return poll, nil
	}
	run := &intakeRun{w: w, svc: svc, scope: scope, pid: pid, app: app, state: st, poll: poll}
	enabled := map[string]bool{}
	for _, s := range st.Signals {
		enabled[s] = true
	}
	if enabled["S1"] {
		b, err := svc.BindingV2(ctx, scope)
		if err == nil {
			for _, team := range b.LinearTeamIDs {
				run.source(ctx, "intake-s1", team, run.labelled)
			}
		}
	}
	if enabled["S2"] {
		run.source(ctx, "intake-s2", "app", run.delegated)
	}
	if enabled["S3"] {
		run.source(ctx, "intake-s3", "app", run.mentioned)
	}
	charge := map[string]any{"calls": poll.Calls}
	for k, v := range fence {
		charge[k] = v
	}
	if _, err = w.Store.ExecuteSystem(ctx, pid, bus.Request{Operation: "watch_charge", Args: charge}); err != nil {
		return poll, err
	}
	return poll, nil
}

// source polls one intake source when it is due. Its first poll only
// starts the cursor: history never triggers. A read that fails, or that
// stops at intakePages with more to read (W-L1), leaves the cursor, so
// nothing is skipped.
func (r *intakeRun) source(ctx context.Context, source, subject string, read func(ctx context.Context, subject string, since time.Time) (signal, complete bool, err error)) {
	key := source + "|" + subject
	cursor, seen := r.state.Cursors[key]
	if seen && !cursor.Due {
		return
	}
	polled := r.state.Now
	if !seen {
		_ = r.w.Store.IntakeCursorSet(ctx, r.pid, source, subject, polled, false)
		return
	}
	since, err := time.Parse(time.RFC3339Nano, cursor.Time)
	if err != nil {
		return
	}
	signal, complete, err := read(ctx, subject, since.Add(-intakeLag))
	if err != nil {
		r.poll.Unreadable = append(r.poll.Unreadable, source)
		return
	}
	if !complete {
		r.poll.Truncated = append(r.poll.Truncated, source)
		polled = cursor.Time
	}
	_ = r.w.Store.IntakeCursorSet(ctx, r.pid, source, subject, polled, signal)
}

// labelled is S1: the team's issues carrying needs-agent, updated since.
func (r *intakeRun) labelled(ctx context.Context, team string, since time.Time) (bool, bool, error) {
	label, err := r.needsAgent(ctx, team)
	if err != nil {
		return false, false, err
	}
	if label == "" {
		r.poll.NoLabel = append(r.poll.NoLabel, team)
		return false, true, nil
	}
	signal := false
	cursor := ""
	for page := 0; page < intakePages; page++ {
		r.poll.Calls++
		issues, err := r.svc.IssuesV2(ctx, r.scope, linearboard.IssuesInputV2{Limit: 50, Cursor: cursor, LabelID: label, UpdatedAfter: since.UTC().Format(time.RFC3339Nano)})
		if err != nil {
			return signal, false, err
		}
		for _, i := range issues.Items {
			if i.TeamID != team {
				continue
			}
			signal = r.candidate(ctx, "S1", i.ID, func(n historyEntry) bool { return contains(n.AddedLabelIDs, label) }, "", "") || signal
		}
		if issues.NextCursor == "" {
			return signal, true, nil
		}
		cursor = issues.NextCursor
	}
	return signal, false, nil
}

// needsAgent is the team's needs-agent label, by exact name among its
// labels, cached for an hour; "" when there is none or more than one.
func (r *intakeRun) needsAgent(ctx context.Context, team string) (string, error) {
	now := r.w.now()
	r.w.mu.Lock()
	if r.w.labels == nil {
		r.w.labels = map[string]labelCache{}
	}
	cached, ok := r.w.labels[r.pid+"|"+team]
	r.w.mu.Unlock()
	if ok && now.Sub(cached.checked) < labelCacheTTL {
		return cached.id, nil
	}
	found := []string{}
	cursor := ""
	for page := 0; page < intakePages; page++ {
		r.poll.Calls++
		labels, err := r.svc.LabelsV2(ctx, r.scope, team, 100, cursor)
		if err != nil {
			return "", err
		}
		for _, l := range labels.Items {
			if l.Name == needsAgentLabel && !l.Archived && !l.Group {
				found = append(found, l.ID)
			}
		}
		if labels.NextCursor == "" {
			cursor = ""
			break
		}
		cursor = labels.NextCursor
	}
	id := ""
	if len(found) == 1 && cursor == "" {
		id = found[0]
	}
	r.w.mu.Lock()
	r.w.labels[r.pid+"|"+team] = labelCache{id: id, checked: now}
	r.w.mu.Unlock()
	return id, nil
}

// delegated is S2: issues delegated to the app, updated since (probe I1:
// the app can be a delegate, never an assignee).
func (r *intakeRun) delegated(ctx context.Context, _ string, since time.Time) (bool, bool, error) {
	signal := false
	var after any
	for page := 0; page < intakePages; page++ {
		var out struct {
			Issues struct {
				Nodes []struct {
					ID string `json:"id"`
				} `json:"nodes"`
				PageInfo pageInfo `json:"pageInfo"`
			} `json:"issues"`
		}
		r.poll.Calls++
		if err := r.w.App.GraphQL(ctx, delegatedQuery, map[string]any{"app": r.app.AppUserID, "since": since.UTC().Format(time.RFC3339Nano), "after": after}, &out); err != nil {
			return signal, false, err
		}
		for _, n := range out.Issues.Nodes {
			signal = r.candidate(ctx, "S2", n.ID, func(e historyEntry) bool { return e.ToDelegate != nil && e.ToDelegate.ID == r.app.AppUserID }, "", "") || signal
		}
		if !out.Issues.PageInfo.HasNextPage {
			return signal, true, nil
		}
		after = out.Issues.PageInfo.EndCursor
	}
	return signal, false, nil
}

// mentioned is S3: the app's issueCommentMention notifications since
// (probe I2). The actor is the notification's typed actor; its title and
// subtitle are never read.
func (r *intakeRun) mentioned(ctx context.Context, _ string, since time.Time) (bool, bool, error) {
	signal := false
	var after any
	for page := 0; page < intakePages; page++ {
		var out struct {
			Notifications struct {
				Nodes []struct {
					ID        string `json:"id"`
					Type      string `json:"type"`
					CreatedAt string `json:"createdAt"`
					Issue     *struct {
						ID string `json:"id"`
					} `json:"issue"`
					Actor *struct {
						ID string `json:"id"`
					} `json:"actor"`
				} `json:"nodes"`
				PageInfo pageInfo `json:"pageInfo"`
			} `json:"notifications"`
		}
		r.poll.Calls++
		if err := r.w.App.GraphQL(ctx, mentionsQuery, map[string]any{"first": 50, "after": after}, &out); err != nil {
			return signal, false, err
		}
		older := false
		for _, n := range out.Notifications.Nodes {
			created, err := time.Parse(time.RFC3339Nano, n.CreatedAt)
			if err != nil || !created.After(since) {
				older = true
				continue
			}
			if n.Type != "issueCommentMention" || n.Issue == nil {
				continue
			}
			actor := ""
			if n.Actor != nil {
				actor = n.Actor.ID
			}
			signal = r.candidate(ctx, "S3", n.Issue.ID, nil, actor, n.CreatedAt) || signal
		}
		// Notifications are newest first: a page reaching past since ends
		// the walk.
		if older || !out.Notifications.PageInfo.HasNextPage {
			return signal, true, nil
		}
		after = out.Notifications.PageInfo.EndCursor
	}
	return signal, false, nil
}

// historyEntry is one issue history entry, as the actor proof reads it.
type historyEntry struct {
	CreatedAt string `json:"createdAt"`
	ActorID   string `json:"actorId"`
	BotActor  *struct {
		ID string `json:"id"`
	} `json:"botActor"`
	AddedLabelIDs []string `json:"addedLabelIds"`
	ToDelegate    *struct {
		ID string `json:"id"`
	} `json:"toDelegate"`
}

// candidate proves one issue inside the binding and its transition's
// actor, then queues it. matches finds the transition in the issue's
// history (S1, S2); S3 passes its actor and time instead. It reports
// whether the issue was a signal at all.
func (r *intakeRun) candidate(ctx context.Context, signal, issueID string, matches func(historyEntry) bool, actor, at string) bool {
	r.poll.Calls++
	issue, err := r.svc.IssueV2(ctx, r.scope, issueID)
	if err != nil {
		if linearboard.IsOutOfScope(err) {
			r.poll.skip("not_in_binding")
		} else {
			r.poll.skip("issue_unreadable")
		}
		return false
	}
	bot := false
	if matches != nil {
		entry, found, err := r.transition(ctx, issue.ID, matches)
		if err != nil {
			r.poll.skip("actor_unreadable")
			return true
		}
		if !found {
			r.poll.skip("transition_not_found")
			return true
		}
		actor, at, bot = entry.ActorID, entry.CreatedAt, entry.BotActor != nil
	}
	// A bot, the app itself, or an unknown actor is never trusted.
	trusted := !bot && actor != "" && actor != r.app.AppUserID && r.state.Trusted[actor]
	title := sha256.Sum256([]byte(issue.Title))
	ev := bus.IntakeEvent{Signal: signal, Actor: actor, Trusted: trusted, At: at, Issue: bus.IntakeIssue{UUID: issue.ID, Identifier: issue.Identifier, TitleSHA256: hex.EncodeToString(title[:]), DescriptionSHA256: issue.DescriptionSHA256, UpdatedAt: issue.UpdatedAt}}
	if issue.ProjectKnown {
		ev.Issue.ProjectID = issue.ProjectID
	}
	if issue.LabelsKnown {
		ev.Issue.Labels = issue.LabelNames
	}
	result, err := r.w.Store.IntakeEnqueueSystem(ctx, r.pid, ev)
	switch {
	case err != nil:
		r.poll.skip("queue_failed")
	case result == "queued":
		r.poll.Queued = append(r.poll.Queued, issue.ID)
	default:
		r.poll.skip(result)
	}
	return true
}

// transition finds the newest history entry that matches: the transition
// that made the issue a signal now (probe I4).
func (r *intakeRun) transition(ctx context.Context, issue string, matches func(historyEntry) bool) (historyEntry, bool, error) {
	var after any
	for page := 0; page < historyPages; page++ {
		var out struct {
			Issue struct {
				History struct {
					Nodes    []historyEntry `json:"nodes"`
					PageInfo pageInfo       `json:"pageInfo"`
				} `json:"history"`
			} `json:"issue"`
		}
		r.poll.Calls++
		if err := r.w.App.GraphQL(ctx, historyQuery, map[string]any{"id": issue, "first": 20, "after": after}, &out); err != nil {
			return historyEntry{}, false, err
		}
		for _, n := range out.Issue.History.Nodes {
			if matches(n) {
				return n, true, nil
			}
		}
		if !out.Issue.History.PageInfo.HasNextPage {
			break
		}
		after = out.Issue.History.PageInfo.EndCursor
	}
	return historyEntry{}, false, nil
}

func (w *Watcher) now() time.Time {
	if w.Now != nil {
		return w.Now()
	}
	return time.Now()
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}
