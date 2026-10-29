package watcher

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"sort"
	"strings"
	"time"

	"clawbridge/internal/bus"
	"clawbridge/internal/linearapp"
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
	// labelledQuery is S1: a team's issues carrying any label of its
	// needs-agent set, as labels.some (as view filters record a label
	// collection). labelledOneQuery carries one label in the other shape
	// Linear's label collection filter takes (labels.id), for when Linear
	// refuses the first: between them, either shape serves.
	labelledQuery    = `query Labelled($team: ID!, $labels: [ID!], $since: DateTimeOrDuration!, $after: String) { issues(first: 50, after: $after, filter: { team: { id: { eq: $team } }, labels: { some: { id: { in: $labels } } }, updatedAt: { gt: $since } }) { nodes { id updatedAt team { id } project { id } labels(first: 50) { nodes { id } pageInfo { hasNextPage } } } pageInfo { hasNextPage endCursor } } }`
	labelledOneQuery = `query LabelledOne($team: ID!, $label: ID!, $since: DateTimeOrDuration!, $after: String) { issues(first: 50, after: $after, filter: { team: { id: { eq: $team } }, labels: { id: { eq: $label } }, updatedAt: { gt: $since } }) { nodes { id updatedAt team { id } project { id } labels(first: 50) { nodes { id } pageInfo { hasNextPage } } } pageInfo { hasNextPage endCursor } } }`
	historyQuery     = `query History($id: String!, $first: Int!, $after: String) { issue(id: $id) { history(first: $first, after: $after) { nodes { createdAt actorId botActor { id } addedLabelIds toDelegate { id } } pageInfo { hasNextPage endCursor } } } }`
	labelsQuery      = `query IssueLabels($id: String!) { issue(id: $id) { id labels(first: 50) { nodes { id } pageInfo { hasNextPage } } } }`
	mentionsQuery    = `query Mentions($first: Int!, $after: String) { notifications(first: $first, after: $after) { nodes { id type createdAt ... on IssueNotification { issue { id } actor { id } comment { id } } } pageInfo { hasNextPage endCursor } } }`
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
	// Deferred sources weren't read, or stopped between pages, because the
	// project's share had less than intakeRoom left; their cursor stays,
	// and the next poll reads the oldest cursors first.
	Deferred []string `json:"deferred_sources,omitempty"`
	// TookMS is each signal's time (S1, S2, S3) in milliseconds.
	TookMS map[string]int64 `json:"took_ms,omitempty"`
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
	// binding is the project's binding, read once per intake run for S1.
	binding bus.TaskboardBindingV2
	// inert are the binding's teams without a project, as Linear shows
	// them, read once per run when a due team is listed so.
	inert map[string]bool
	// perLabel: Linear refused the needs-agent set's filter once this run,
	// so S1 reads each team once per label for the rest of it.
	perLabel bool
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
	if st.RolesUnreadable {
		poll.Unreadable = append(poll.Unreadable, "team_roles")
	}
	enabled := map[string]bool{}
	for _, s := range st.Signals {
		enabled[s] = true
	}
	sources := []intakeSource{}
	if enabled["S1"] {
		b, err := svc.BindingV2(ctx, scope)
		if err == nil {
			run.binding = b
			for _, team := range b.LinearTeamIDs {
				sources = append(sources, intakeSource{"intake-s1", team, run.labelled})
			}
		}
	}
	if enabled["S2"] {
		sources = append(sources, intakeSource{"intake-s2", "app", run.delegated})
	}
	if enabled["S3"] {
		sources = append(sources, intakeSource{"intake-s3", "app", run.mentioned})
	}
	// The oldest cursors first, so a source a short share deferred is read
	// before the ones that were, and none waits behind the same others
	// every tick. A source with no cursor yet only starts one: first.
	at := func(src intakeSource) string { return st.Cursors[src.source+"|"+src.subject].Time }
	sort.SliceStable(sources, func(i, j int) bool { return at(sources[i]) < at(sources[j]) })
	for _, src := range sources {
		run.source(ctx, src.source, src.subject, src.read)
	}
	// The calls made are charged even when the share has run out.
	cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), intakeChargeTimeout)
	defer cancel()
	charge := map[string]any{"calls": poll.Calls}
	for k, v := range fence {
		charge[k] = v
	}
	if _, err = w.Store.ExecuteSystem(cctx, pid, bus.Request{Operation: "watch_charge", Args: charge}); err != nil {
		return poll, err
	}
	return poll, nil
}

// intakeSource is one source to poll: S1 per team, S2, S3.
type intakeSource struct {
	source, subject string
	read            func(ctx context.Context, subject string, since time.Time) (signal, complete bool, err error)
}

var (
	// intakeRoom is the time a source, or its next page, needs left in
	// the project's share to start: less, and it waits for the next poll
	// with its cursor where it was, leaving the rest of the share to the
	// steps after intake and the markers.
	intakeRoom = 5 * time.Second
	// intakeChargeTimeout bounds the charge of intake's calls once the
	// share has run out.
	intakeChargeTimeout = 5 * time.Second
)

// deferred reports whether ctx has too little left to start a source or
// a page.
func deferred(ctx context.Context) bool {
	deadline, ok := ctx.Deadline()
	return ok && time.Until(deadline) < intakeRoom
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
	if deferred(ctx) {
		r.poll.Deferred = append(r.poll.Deferred, source)
		return
	}
	start := time.Now()
	signal, complete, err := read(ctx, subject, since.Add(-intakeLag))
	if r.poll.TookMS == nil {
		r.poll.TookMS = map[string]int64{}
	}
	r.poll.TookMS[strings.ToUpper(strings.TrimPrefix(source, "intake-"))] += time.Since(start).Milliseconds()
	if err != nil {
		r.poll.Unreadable = append(r.poll.Unreadable, source)
		return
	}
	if !complete {
		if deferred(ctx) {
			r.poll.Deferred = append(r.poll.Deferred, source)
		} else {
			r.poll.Truncated = append(r.poll.Truncated, source)
		}
		polled = cursor.Time
	}
	_ = r.w.Store.IntakeCursorSet(ctx, r.pid, source, subject, polled, signal)
}

// labelled is S1: the team's issues carrying a label of its needs-agent
// set (the team's own, and its bound parent's, CROSS-TEAM-BINDING §3.2),
// updated since. It reads the team with one app GraphQL query for the whole
// set, across all its projects, rather than one bound project or one label
// at a time: a binding with many projects would otherwise never finish
// within intakePages. Every row is proven locally as the provider read
// proved it: the team's own, carrying a label of the set, updated after
// since; a row whose project is outside the binding is skipped unread, and
// candidate proves every other one.
func (r *intakeRun) labelled(ctx context.Context, team string, since time.Time) (bool, bool, error) {
	set, err := r.needsAgent(ctx, team)
	if err != nil {
		return false, false, err
	}
	if len(set) == 0 {
		r.poll.NoLabel = append(r.poll.NoLabel, team)
		return false, true, nil
	}
	// A team without a project reads nothing until one carries it; its
	// history then is not intake's, so the cursor moves on.
	inert, err := r.inertTeam(ctx, team)
	if err != nil {
		return false, false, err
	}
	if inert {
		r.poll.Inert = append(r.poll.Inert, team)
		return false, true, nil
	}
	signal := false
	seen := map[string]bool{}
	carries := func(n historyEntry) bool {
		for _, id := range set {
			if contains(n.AddedLabelIDs, id) {
				return true
			}
		}
		return false
	}
	if !r.perLabel {
		complete, err := r.labelledBy(ctx, team, set, set, since, seen, carries, &signal)
		if err == nil || !linearapp.IsQueryInvalid(err) {
			return signal, complete && err == nil, err
		}
		// Linear refused the set's filter: one query per label, for the
		// rest of this intake run.
		r.perLabel = true
	}
	complete := true
	for _, label := range set {
		done, err := r.labelledBy(ctx, team, []string{label}, set, since, seen, carries, &signal)
		if err != nil {
			return signal, false, err
		}
		complete = complete && done
	}
	return signal, complete, nil
}

// labelledBy pages the team's issues carrying one of labels (the whole set,
// or one label of it when Linear refused the set's filter); an issue an
// earlier page or label already brought is not counted or queued again.
func (r *intakeRun) labelledBy(ctx context.Context, team string, labels, set []string, since time.Time, seen map[string]bool, carries func(historyEntry) bool, signal *bool) (bool, error) {
	query, vars := labelledQuery, map[string]any{"team": team, "labels": labels, "since": since.UTC().Format(time.RFC3339Nano)}
	if r.perLabel {
		query, vars = labelledOneQuery, map[string]any{"team": team, "label": labels[0], "since": vars["since"]}
	}
	var after any
	for page := 0; page < intakePages; page++ {
		if page > 0 && deferred(ctx) {
			return false, nil
		}
		var out struct {
			Issues struct {
				Nodes []struct {
					ID        string `json:"id"`
					UpdatedAt string `json:"updatedAt"`
					Team      *struct {
						ID string `json:"id"`
					} `json:"team"`
					Project *struct {
						ID string `json:"id"`
					} `json:"project"`
					Labels struct {
						Nodes []struct {
							ID string `json:"id"`
						} `json:"nodes"`
						PageInfo pageInfo `json:"pageInfo"`
					} `json:"labels"`
				} `json:"nodes"`
				PageInfo pageInfo `json:"pageInfo"`
			} `json:"issues"`
		}
		vars["after"] = after
		r.poll.Calls++
		if err := r.w.App.GraphQL(ctx, query, vars, &out); err != nil {
			return false, err
		}
		for _, n := range out.Issues.Nodes {
			if !linearprovider.ValidID(n.ID) {
				return false, errors.New("the labelled read escaped the selected team")
			}
			if seen[n.ID] {
				continue
			}
			seen[n.ID] = true
			if n.Team == nil || n.Team.ID != team {
				// A sub-team's row under its parent's filter: a listed
				// team's own read returns it; any other is dropped and
				// counted.
				if n.Team == nil || !contains(r.binding.LinearTeamIDs, n.Team.ID) {
					r.poll.skip("other_team")
				}
				continue
			}
			if updated, ok := linearprovider.UpdatedAfterTime(n.UpdatedAt); !ok || updated.Before(since) {
				return false, errors.New("the labelled read did not prove its updatedAt filter")
			}
			label := ""
			for _, id := range set {
				for _, l := range n.Labels.Nodes {
					if label == "" && l.ID == id {
						label = id
					}
				}
			}
			// A row whose labels run past one page may carry the set's label
			// further on: candidate's own read decides it.
			if label == "" && !n.Labels.PageInfo.HasNextPage {
				r.poll.skip("no_label")
				continue
			}
			if label == "" {
				label = set[0]
			}
			// The row's project rules it out of a project set's binding;
			// a row without one is left to candidate's read.
			if r.binding.Mode == "project_set" && n.Project != nil && !contains(r.binding.LinearProjectIDs, n.Project.ID) {
				r.poll.skip("not_in_binding")
				continue
			}
			*signal = r.candidate(ctx, "S1", n.ID, carries, "", "", &attribution{kind: "label", target: label, set: set, since: since}) || *signal
		}
		if !out.Issues.PageInfo.HasNextPage {
			return true, nil
		}
		if out.Issues.PageInfo.EndCursor == "" || out.Issues.PageInfo.EndCursor == after {
			return false, errors.New("the labelled read gave no next cursor")
		}
		after = out.Issues.PageInfo.EndCursor
	}
	return false, nil
}

// inertTeam: the binding lists the team without a project and no bound
// project carries it (linearboard.InertTeamV2). Only a team the binding
// lists so is read against Linear, once per intake run.
func (r *intakeRun) inertTeam(ctx context.Context, team string) (bool, error) {
	if r.binding.Mode != "project_set" || !contains(r.binding.TeamsWithoutProject, team) {
		return false, nil
	}
	if r.inert == nil {
		teams, err := r.svc.TeamsWithoutProjectV2(ctx, r.scope)
		if err != nil {
			return false, err
		}
		r.inert = map[string]bool{}
		for _, t := range teams {
			r.inert[t.TeamID] = t.Inert
		}
	}
	return r.inert[team], nil
}

// needsAgent is the team's needs-agent label set (linearboard.LabelSetV2),
// by exact name, cached for an hour; empty when there is none or a team
// lists more than one.
func (r *intakeRun) needsAgent(ctx context.Context, team string) ([]string, error) {
	now := r.w.now()
	key := r.pid + "|" + team
	r.w.mu.Lock()
	if r.w.labels == nil {
		r.w.labels = map[string]labelCache{}
	}
	cached, ok := r.w.labels[key]
	r.w.mu.Unlock()
	if ok && now.Sub(cached.checked) < labelCacheTTL {
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
		if page > 0 && deferred(ctx) {
			return signal, false, nil
		}
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
		if page > 0 && deferred(ctx) {
			return signal, false, nil
		}
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
