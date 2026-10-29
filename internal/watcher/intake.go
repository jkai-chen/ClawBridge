package watcher

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
	"time"

	"clawbridge/internal/bus"
	"clawbridge/internal/linearboard"
	"clawbridge/internal/linearprovider"
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
	labelsQuery    = `query IssueLabels($id: String!) { issue(id: $id) { id labels(first: 50) { nodes { id } pageInfo { hasNextPage } } } }`
	mentionsQuery  = `query Mentions($first: Int!, $after: String) { notifications(first: $first, after: $after) { nodes { id type createdAt ... on IssueNotification { issue { id } actor { id } comment { id } } } pageInfo { hasNextPage endCursor } } }`
)

// IntakePoll is what one tick's intake did for a project.
type IntakePoll struct {
	Queued  []string       `json:"queued,omitempty"`
	Skipped map[string]int `json:"skipped,omitempty"`
	Calls   int            `json:"calls"`
	Off     bool           `json:"off,omitempty"`
	NoLabel []string       `json:"no_label_teams,omitempty"`
	// Inert are write teams listed without a project that no bound project
	// carries yet: they read nothing, so they signal nothing.
	Inert      []string `json:"inert_teams,omitempty"`
	Unreadable []string `json:"unreadable_sources,omitempty"`
	// Truncated sources had more than intakePages pages; their cursor
	// stays, so what was not reached is read again next poll.
	Truncated []string `json:"truncated_sources,omitempty"`
	// Trust counts how each proven actor was judged: "list" (the trusted
	// list), "role" (team roles, eligible), "role_ineligible" (not an
	// active, non-guest user) and "role_unverified" (the read failed).
	Trust map[string]int `json:"trust,omitempty"`
}

func (p *IntakePoll) trust(how string) {
	if p.Trust == nil {
		p.Trust = map[string]int{}
	}
	p.Trust[how]++
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
	// resolved marks teams whose needs-agent set this run already resolved
	// afresh after a label stopped verifying: once, then it stops.
	resolved map[string]bool
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
	run := &intakeRun{w: w, svc: svc, scope: scope, pid: pid, app: app, state: st, poll: poll, resolved: map[string]bool{}}
	if st.RolesUnreadable {
		poll.Unreadable = append(poll.Unreadable, "team_roles")
	}
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
// It reads the team once per label of the needs-agent set (the team's own,
// and its bound parent's, CROSS-TEAM-BINDING §3.2), across all its
// projects, rather than one bound project at a time: a binding with many
// projects would otherwise never finish within intakePages. An issue whose
// project the page shows outside the binding is skipped unread; candidate
// proves every other one.
func (r *intakeRun) labelled(ctx context.Context, team string, since time.Time) (bool, bool, error) {
	set, err := r.needsAgent(ctx, team, false)
	if err != nil {
		return false, false, err
	}
	if len(set) == 0 {
		r.poll.NoLabel = append(r.poll.NoLabel, team)
		return false, true, nil
	}
	signal, complete := false, true
	seen := map[string]bool{}
	carries := func(n historyEntry) bool {
		for _, id := range set {
			if contains(n.AddedLabelIDs, id) {
				return true
			}
		}
		return false
	}
	for i := 0; i < len(set); i++ {
		done, err := r.labelledBy(ctx, team, set[i], set, since, seen, carries, &signal)
		// A team without a project reads nothing until one carries it; its
		// history then is not intake's, so the cursor moves on.
		if linearboard.IsTeamWithoutProject(err) {
			r.poll.Inert = append(r.poll.Inert, team)
			return signal, true, nil
		}
		// A label Linear no longer verifies for the team (merged or
		// removed): the set is resolved again once, then read afresh.
		if errors.Is(err, linearboard.ErrLabelNotVerified) && !r.resolved[team] {
			r.resolved[team] = true
			if set, err = r.needsAgent(ctx, team, true); err != nil {
				return signal, false, err
			}
			if len(set) == 0 {
				r.poll.NoLabel = append(r.poll.NoLabel, team)
				return signal, true, nil
			}
			i, complete = -1, true
			continue
		}
		if err != nil {
			return signal, false, err
		}
		complete = complete && done
	}
	return signal, complete, nil
}

// labelledBy reads the team's issues carrying label, one of set; an issue
// another label of the set already brought is not queued again.
func (r *intakeRun) labelledBy(ctx context.Context, team, label string, set []string, since time.Time, seen map[string]bool, carries func(historyEntry) bool, signal *bool) (bool, error) {
	cursor := ""
	for page := 0; page < intakePages; page++ {
		r.poll.Calls++
		issues, err := r.svc.TeamLabelledV2(ctx, r.scope, linearboard.TeamLabelledInputV2{TeamID: team, LabelID: label, UpdatedAfter: since.UTC().Format(time.RFC3339Nano), Cursor: cursor, Limit: 50})
		if err != nil {
			return false, err
		}
		for range issues.FilteredOtherTeam {
			r.poll.skip("other_team")
		}
		for _, i := range issues.Items {
			if seen[i.ID] {
				continue
			}
			seen[i.ID] = true
			if i.OutOfBinding {
				r.poll.skip("not_in_binding")
				continue
			}
			*signal = r.candidate(ctx, "S1", i.ID, carries, "", "", &attribution{kind: "label", target: label, set: set, since: since}) || *signal
		}
		if issues.Complete {
			return true, nil
		}
		cursor = issues.NextCursor
	}
	return false, nil
}

// needsAgent is the team's needs-agent label set (linearboard.LabelSetV2),
// by exact name, cached for an hour; empty when there is none or a team
// lists more than one. fresh drops the cached set first.
func (r *intakeRun) needsAgent(ctx context.Context, team string, fresh bool) ([]string, error) {
	now := r.w.now()
	key := r.pid + "|" + team
	r.w.mu.Lock()
	if r.w.labels == nil {
		r.w.labels = map[string]labelCache{}
	}
	cached, ok := r.w.labels[key]
	r.w.mu.Unlock()
	if ok && !fresh && now.Sub(cached.checked) < labelCacheTTL {
		return cached.ids, nil
	}
	set, err := r.svc.LabelSetV2(ctx, r.scope, team, needsAgentLabel)
	r.poll.Calls += set.Calls
	switch {
	case errors.Is(err, linearboard.ErrLabelAmbiguous), errors.Is(err, linearboard.ErrLabelWalk):
		set.IDs = nil
	case err != nil:
		return nil, err
	}
	r.w.mu.Lock()
	r.w.labels[key] = labelCache{ids: set.IDs, checked: now}
	r.w.mu.Unlock()
	return set.IDs, nil
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
			signal = r.candidate(ctx, "S2", n.ID, func(e historyEntry) bool { return e.ToDelegate != nil && e.ToDelegate.ID == r.app.AppUserID }, "", "", nil) || signal
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
					Comment *struct {
						ID string `json:"id"`
					} `json:"comment"`
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
			var attr *attribution
			if n.Comment != nil && n.Comment.ID != "" {
				attr = &attribution{kind: "comment", target: n.Comment.ID}
			}
			signal = r.candidate(ctx, "S3", n.Issue.ID, nil, actor, n.CreatedAt, attr) || signal
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

// attribution is what a signal's app write would carry (intake
// attribution): the label it added (S1) or the comment it posted (S3), and
// for S1 the start of the poll window, searched when Linear's history has
// no entry for the label at all.
type attribution struct {
	kind, target string
	// set is a needs-agent label's whole set (CROSS-TEAM-BINDING W2): the
	// label-on proof accepts any of its IDs; the credit stays by target.
	set   []string
	since time.Time
}

// candidate proves one issue inside the binding and its transition's
// actor, then queues it. matches finds the transition in the issue's
// history (S1, S2); S3 passes its actor and time instead. When Linear names
// this server's app as the actor, or (S1) has no entry, attr looks for the
// person who asked the app for that write. It reports whether the issue was
// a signal at all.
func (r *intakeRun) candidate(ctx context.Context, signal, issueID string, matches func(historyEntry) bool, actor, at string, attr *attribution) bool {
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
	var credit bus.AttributionCredit
	if matches != nil {
		entry, found, err := r.transition(ctx, issue.ID, matches)
		if err != nil {
			r.poll.skip("actor_unreadable")
			return true
		}
		if !found {
			// A label added just after the create leaves no history
			// entry. Only an attribution, the one write in the whole poll
			// window, with the label proven on the issue by ID now, can
			// stand for it.
			if attr == nil || attr.since.IsZero() || !r.labelOn(ctx, issue, append([]string{attr.target}, attr.set...)) {
				r.poll.skip("transition_not_found")
				return true
			}
			if credit = r.attributed(ctx, issue.ID, attr, attr.since, r.w.now()); credit.Result != "credited" {
				r.poll.skip(cmpOr(credit.Result, "transition_not_found"))
				return true
			}
		} else {
			actor, at, bot = entry.ActorID, entry.CreatedAt, entry.BotActor != nil
		}
	}
	if credit.Result == "" && !bot && actor != "" && actor == r.app.AppUserID && attr != nil {
		// A5: Linear's time follows the write's by up to 120 s, and may
		// read up to 10 s before it.
		if t, err := time.Parse(time.RFC3339Nano, at); err == nil {
			credit = r.attributed(ctx, issue.ID, attr, t.Add(-bus.AttributionAfter), t.Add(bus.AttributionBefore))
			if credit.Result != "" && credit.Result != "credited" {
				r.poll.skip(credit.Result)
			}
		}
	}
	// A bot, the app itself, or an unknown actor is never trusted (by
	// intake's actor class, §12 L2); otherwise the trusted list, then the
	// team roles for the issue's team.
	trusted := false
	if !bot && actor != "" && actor != r.app.AppUserID {
		trusted = r.state.Trusted[actor]
		if trusted {
			r.poll.trust("list")
		} else {
			trusted = r.trustedByRole(ctx, issue.TeamID, actor)
		}
	}
	via := ""
	if credit.Result == "credited" {
		actor, trusted, via = credit.Actor, true, credit.Principal
	}
	title := sha256.Sum256([]byte(issue.Title))
	ev := bus.IntakeEvent{Signal: signal, Actor: actor, Trusted: trusted, At: at, Via: via, Issue: bus.IntakeIssue{UUID: issue.ID, Identifier: issue.Identifier, TitleSHA256: hex.EncodeToString(title[:]), DescriptionSHA256: issue.DescriptionSHA256, UpdatedAt: issue.UpdatedAt}}
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

// labelOn proves one of labels (a needs-agent set, W2) is on the issue now, by ID: from the board's read
// when it carries label IDs, else from the app's own read of the issue's
// labels (native Linear's board read gives names only). A read that fails,
// or a list past one page, proves nothing.
func (r *intakeRun) labelOn(ctx context.Context, issue linearprovider.IssueV2, labels []string) bool {
	if issue.LabelsKnown {
		for _, l := range labels {
			if contains(issue.LabelIDs, l) {
				return true
			}
		}
		return false
	}
	var out struct {
		Issue *struct {
			ID     string `json:"id"`
			Labels struct {
				Nodes []struct {
					ID string `json:"id"`
				} `json:"nodes"`
				PageInfo pageInfo `json:"pageInfo"`
			} `json:"labels"`
		} `json:"issue"`
	}
	r.poll.Calls++
	if err := r.w.App.GraphQL(ctx, labelsQuery, map[string]any{"id": issue.ID}, &out); err != nil || out.Issue == nil || out.Issue.ID != issue.ID || out.Issue.Labels.PageInfo.HasNextPage {
		return false
	}
	for _, n := range out.Issue.Labels.Nodes {
		if contains(labels, n.ID) {
			return true
		}
	}
	return false
}

// attributed reads the app writes for attr on issue recorded between from
// and to. A read that fails credits nothing.
func (r *intakeRun) attributed(ctx context.Context, issue string, attr *attribution, from, to time.Time) bus.AttributionCredit {
	credit, err := r.w.Store.IntakeAttribution(ctx, r.pid, issue, attr.kind, attr.target, from, to)
	if err != nil {
		return bus.AttributionCredit{Result: "attribution_unreadable"}
	}
	return credit
}

func cmpOr(a, b string) string {
	if a != "" {
		return a
	}
	return b
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

// userCheckTTL is how old a user's eligibility read may be when a role
// credits them (T2').
const userCheckTTL = 15 * time.Minute

// userQuery reads one Linear user's active and guest flags.
const userQuery = `query User($id: String!) { user(id: $id) { id active guest } }`

// trustedByRole: the team roles trust actor for team's issues, and Linear
// says, in a read no older than userCheckTTL, that they are an active,
// non-guest user (T2'). A failed read only notifies, and isn't cached.
func (r *intakeRun) trustedByRole(ctx context.Context, team, actor string) bool {
	key := strings.ToLower(actor)
	if !r.state.RoleTrusted[strings.ToLower(team)+"|"+key] {
		return false
	}
	now := r.w.now()
	r.w.mu.Lock()
	cached, ok := r.w.users[key]
	r.w.mu.Unlock()
	if !ok || now.Sub(cached.checked) > userCheckTTL || now.Before(cached.checked) {
		var out struct {
			User *struct {
				ID     string `json:"id"`
				Active *bool  `json:"active"`
				Guest  *bool  `json:"guest"`
			} `json:"user"`
		}
		r.poll.Calls++
		if err := r.w.App.GraphQL(ctx, userQuery, map[string]any{"id": actor}, &out); err != nil || out.User == nil || !strings.EqualFold(out.User.ID, actor) || out.User.Active == nil || out.User.Guest == nil {
			r.poll.trust("role_unverified")
			return false
		}
		cached = userCheck{eligible: *out.User.Active && !*out.User.Guest, checked: now}
		r.w.mu.Lock()
		if r.w.users == nil {
			r.w.users = map[string]userCheck{}
		}
		r.w.users[key] = cached
		r.w.mu.Unlock()
	}
	if !cached.eligible {
		r.poll.trust("role_ineligible")
		return false
	}
	r.poll.trust("role")
	return true
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
