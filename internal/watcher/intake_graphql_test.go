package watcher

import (
	"context"
	"maps"
	"slices"
	"testing"
	"time"

	"clawbridge/internal/bus"
	"clawbridge/internal/linearapp"
	"clawbridge/internal/linearprovider"
)

// The S1 fixture: a parent team and two bound sub-teams, plus an unbound
// team whose rows a parent's filter spills; each sub-team's own copy of
// needs-agent, the parent's, and an unrelated label; a bound project and an
// unbound one; rows updated before and after since.
const (
	gqlSub1      = "a1000000-0000-4000-8000-000000000001"
	gqlSub2      = "a1000000-0000-4000-8000-000000000002"
	gqlUnbound   = "a1000000-0000-4000-8000-000000000003"
	gqlLabel1    = "b1000000-0000-4000-8000-000000000001"
	gqlLabel2    = "b1000000-0000-4000-8000-000000000002"
	gqlLabelX    = "b1000000-0000-4000-8000-000000000003"
	gqlParentA   = "c1000000-0000-4000-8000-000000000001"
	gqlParentX   = "c1000000-0000-4000-8000-000000000002"
	gqlParentOld = "c1000000-0000-4000-8000-000000000003"
	gqlSub1A     = "c1000000-0000-4000-8000-000000000004"
	gqlSub1P     = "c1000000-0000-4000-8000-000000000005"
	gqlSub1Out   = "c1000000-0000-4000-8000-000000000006"
	gqlSub2A     = "c1000000-0000-4000-8000-000000000007"
	gqlSub2Hide  = "c1000000-0000-4000-8000-000000000008"
	gqlOtherA    = "c1000000-0000-4000-8000-000000000009"
	gqlOtherX    = "c1000000-0000-4000-8000-00000000000a"
)

type gqlRow struct {
	team, project string
	labels        []string
	old, hidden   bool
}

// s1Fixture sets the fixture up, its cursors started and due.
func s1Fixture(t *testing.T) (*env, map[string]gqlRow, map[string][]string) {
	t.Helper()
	e := setup(t)
	bound, unbound := linearProjectID(1), linearProjectID(2)
	e.f.projects = map[string]bool{bound: true, unbound: true}
	e.f.projectTeams = map[string][]string{bound: {team, gqlSub1, gqlSub2}, unbound: {gqlSub1, gqlSub2, gqlUnbound}}
	if _, err := e.s.Execute(context.Background(), bus.Request{Operation: "taskboard_set_v2", Workspace: "w", Project: "p", Args: map[string]any{"expected_revision": 1, "mode": "project_set", "linear_team_ids": []string{team, gqlSub1, gqlSub2}, "linear_default_team_id": team, "linear_project_ids": []string{bound}}}); err != nil {
		t.Fatal(err)
	}
	e.call("intake_trust_add", "coord", map[string]any{"linear_user_id": trustedUser})
	needs := func(id, t string) linearprovider.Label {
		return linearprovider.Label{ID: id, Name: needsAgentLabel, TeamID: t, EligibilityKnown: true}
	}
	e.f.parents = map[string]string{gqlSub1: team, gqlSub2: team}
	e.f.teamLabels = map[string][]linearprovider.Label{
		team:    {needs(labelNeeds, team), {ID: gqlLabelX, Name: "bug", TeamID: team, EligibilityKnown: true}},
		gqlSub1: {needs(gqlLabel1, gqlSub1)},
		gqlSub2: {needs(gqlLabel2, gqlSub2)},
	}
	sets := map[string][]string{team: {labelNeeds}, gqlSub1: {gqlLabel1, labelNeeds}, gqlSub2: {gqlLabel2, labelNeeds}}
	rows := map[string]gqlRow{
		gqlParentA:   {team: team, project: bound, labels: []string{labelNeeds}},
		gqlParentX:   {team: team, project: bound, labels: []string{gqlLabelX}},
		gqlParentOld: {team: team, project: bound, labels: []string{labelNeeds}, old: true},
		gqlSub1A:     {team: gqlSub1, project: bound, labels: []string{gqlLabel1}},
		gqlSub1P:     {team: gqlSub1, project: bound, labels: []string{labelNeeds, gqlLabelX}},
		gqlSub1Out:   {team: gqlSub1, project: unbound, labels: []string{gqlLabel1}},
		gqlSub2A:     {team: gqlSub2, project: bound, labels: []string{gqlLabel2, labelNeeds}},
		gqlSub2Hide:  {team: gqlSub2, project: unbound, labels: []string{gqlLabel2}, hidden: true},
		gqlOtherA:    {team: gqlUnbound, project: unbound, labels: []string{labelNeeds}},
		gqlOtherX:    {team: gqlUnbound, project: unbound, labels: []string{gqlLabelX}},
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	e.f.extra, e.f.carries, e.f.unlisted, e.f.updated = map[string]linearprovider.IssueV2{}, map[string][]string{}, map[string]bool{}, map[string]string{}
	e.f.labelled, e.f.spill = nil, true
	for _, id := range slices.Sorted(maps.Keys(rows)) {
		r := rows[id]
		e.f.labelled = append(e.f.labelled, id)
		e.f.extra[id] = linearprovider.IssueV2{Issue: linearprovider.Issue{ID: id, Identifier: "GQL-" + id[len(id)-1:], Title: "t", TeamID: r.team, ProjectID: r.project}, ProjectKnown: true, DescriptionKnown: true, UpdatedAt: now}
		e.f.carries[id] = r.labels
		e.f.unlisted[id] = r.hidden
		if r.old {
			e.f.updated[id] = time.Now().Add(-2 * time.Hour).UTC().Format(time.RFC3339Nano)
		}
		e.f.history[id] = []map[string]any{entry(now, trustedUser, map[string]any{"addedLabelIds": r.labels})}
	}
	e.tick() // the first poll starts the cursors
	e.dueNow()
	return e, rows, sets
}

// oldS1 is what the per-label path queued and skipped on the fixture: each
// bound team read once per label of its set, Linear returning the rows
// (any team's, as the parent's filter spills) that carry the label and
// were updated after since; another unbound team's row counted on every
// read; a row whose listed project is outside the binding skipped; any
// other proven by its own read, which rules out one outside the binding.
func oldS1(e *env, rows map[string]gqlRow, sets map[string][]string) ([]string, map[string]int) {
	e.t.Helper()
	bound := map[string]bool{team: true, gqlSub1: true, gqlSub2: true}
	queued, skipped := []string{}, map[string]int{}
	for _, t := range []string{team, gqlSub1, gqlSub2} {
		var cursor string
		e.queryRow(`SELECT cursor_time FROM watch_cursors WHERE source='intake-s1' AND subject='`+t+`'`, &cursor)
		at, err := time.Parse(time.RFC3339Nano, cursor)
		if err != nil {
			e.t.Fatal(err)
		}
		since := at.Add(-intakeLag)
		seen := map[string]bool{}
		for _, label := range sets[t] {
			for _, id := range e.f.labelled {
				r := rows[id]
				updated := time.Now()
				if r.old {
					updated = time.Now().Add(-2 * time.Hour)
				}
				if !slices.Contains(r.labels, label) || !updated.After(since) {
					continue
				}
				if r.team != t {
					if !bound[r.team] {
						skipped["other_team"]++
					}
					continue
				}
				if seen[id] {
					continue
				}
				seen[id] = true
				if r.project != linearProjectID(1) {
					skipped["not_in_binding"]++
					continue
				}
				queued = append(queued, id)
			}
		}
	}
	slices.Sort(queued)
	return queued, skipped
}

func queries(e *env, from int, q string) int {
	n := 0
	for _, got := range e.f.queries[from:] {
		if got == q {
			n++
		}
	}
	return n
}

// S1 through one app GraphQL query per team queues exactly what the
// per-label path did, with the same skips; it makes one query per due team
// and no per-label query, and every page is charged.
func TestIntakeS1GraphQLMatchesThePerLabelPath(t *testing.T) {
	e, rows, sets := s1Fixture(t)
	wantQueued, wantSkipped := oldS1(e, rows, sets)
	if !slices.Equal(wantQueued, []string{gqlParentA, gqlSub1A, gqlSub1P, gqlSub2A}) || !maps.Equal(wantSkipped, map[string]int{"other_team": 3, "not_in_binding": 2}) {
		t.Fatal("the fixture no longer covers every case:", wantQueued, wantSkipped)
	}
	var before int
	e.queryRow(`SELECT budget_used FROM watch_lease`, &before)
	from := len(e.f.queries)
	tick := e.tick()
	if tick.Intake == nil || len(tick.Intake.Unreadable) != 0 || len(tick.Intake.NoLabel) != 0 {
		t.Fatalf("intake: %+v", tick.Intake)
	}
	got := slices.Sorted(slices.Values(tick.Intake.Queued))
	if !slices.Equal(got, wantQueued) || !maps.Equal(tick.Intake.Skipped, wantSkipped) {
		t.Fatalf("queued %v skipped %v, the per-label path %v %v", got, tick.Intake.Skipped, wantQueued, wantSkipped)
	}
	if n, one := queries(e, from, labelledQuery), queries(e, from, labelledOneQuery); n != 3 || one != 0 {
		t.Fatalf("%d set queries and %d per-label queries for 3 due teams", n, one)
	}
	var after int
	e.queryRow(`SELECT budget_used FROM watch_lease`, &after)
	if tick.Intake.Calls < 3 || after-before < tick.Intake.Calls {
		t.Fatal("the pages weren't charged:", after-before, tick.Intake.Calls)
	}
	if _, ok := tick.Intake.TookMS["S1"]; !ok {
		t.Fatalf("S1's time is missing: %+v", tick.Intake.TookMS)
	}
}

// When Linear refuses the set's filter, S1 reads each label of the set
// with its own query and queues the same; the refusal is remembered, so
// the next team makes no set query.
func TestIntakeS1FallsBackToOneQueryPerLabel(t *testing.T) {
	e, rows, sets := s1Fixture(t)
	wantQueued, wantSkipped := oldS1(e, rows, sets)
	e.f.refuseIn = true
	from := len(e.f.queries)
	tick := e.tick()
	if tick.Intake == nil || len(tick.Intake.Unreadable) != 0 {
		t.Fatalf("intake: %+v", tick.Intake)
	}
	got := slices.Sorted(slices.Values(tick.Intake.Queued))
	if !slices.Equal(got, wantQueued) || !maps.Equal(tick.Intake.Skipped, wantSkipped) {
		t.Fatalf("queued %v skipped %v, want %v %v", got, tick.Intake.Skipped, wantQueued, wantSkipped)
	}
	labels := 0
	for _, set := range sets {
		labels += len(set)
	}
	if n, one := queries(e, from, labelledQuery), queries(e, from, labelledOneQuery); n != 1 || one != labels {
		t.Fatalf("%d set queries and %d per-label queries; want 1 and %d", n, one, labels)
	}
}

// Another error is no fallback: the team's source is unreadable, its
// cursor stays, and no per-label query runs.
func TestIntakeS1OtherErrorIsUnreadable(t *testing.T) {
	e := setup(t)
	e.f.labelled = []string{issueA}
	e.tick()
	var started string
	e.queryRow(`SELECT cursor_time FROM watch_cursors WHERE source='intake-s1'`, &started)
	e.dueNow()
	e.f.labelledErr = &linearapp.Error{Code: "query_failed"}
	from := len(e.f.queries)
	tick := e.tick()
	var kept string
	e.queryRow(`SELECT cursor_time FROM watch_cursors WHERE source='intake-s1'`, &kept)
	if tick.Intake == nil || !slices.Contains(tick.Intake.Unreadable, "intake-s1") || kept != started {
		t.Fatalf("intake: %+v cursor %s→%s", tick.Intake, started, kept)
	}
	if queries(e, from, labelledQuery) != 1 || queries(e, from, labelledOneQuery) != 0 {
		t.Fatal("another error fell back to per-label queries")
	}
}

// Each row is proven locally: a row whose labels don't hold the set is
// skipped as no_label, and a row older than since fails the read.
func TestIntakeS1ProvesEachRow(t *testing.T) {
	e := setup(t)
	e.call("intake_trust_add", "coord", map[string]any{"linear_user_id": trustedUser})
	e.f.labelled = []string{issueA, issueB}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	for _, id := range e.f.labelled {
		e.f.history[id] = []map[string]any{entry(now, trustedUser, map[string]any{"addedLabelIds": []string{labelNeeds}})}
	}
	e.f.rowLabels = map[string][]string{issueB: {"cccccccc-cccc-4ccc-8ccc-cccccccccccc"}}
	e.tick()
	e.dueNow()
	tick := e.tick()
	if tick.Intake == nil || !slices.Equal(tick.Intake.Queued, []string{issueA}) || tick.Intake.Skipped["no_label"] != 1 {
		t.Fatalf("intake: %+v", tick.Intake)
	}
	// The same row with its labels running past the page: candidate's own
	// read decides it, and its history proves the needs-agent label.
	e.f.rowLabelsMore = map[string]bool{issueB: true}
	e.dueNow()
	if tick = e.tick(); tick.Intake == nil || !slices.Contains(tick.Intake.Queued, issueB) || tick.Intake.Skipped["no_label"] != 0 {
		t.Fatalf("a row with more labels than a page was skipped: %+v", tick.Intake)
	}
	e.f.rowLabelsMore = nil
	e.f.rowLabels = nil
	e.f.sinceIgnored = true
	e.f.updated = map[string]string{issueB: time.Now().Add(-2 * time.Hour).UTC().Format(time.RFC3339Nano)}
	e.dueNow()
	if tick = e.tick(); tick.Intake == nil || !slices.Contains(tick.Intake.Unreadable, "intake-s1") {
		t.Fatalf("a row older than since was taken: %+v", tick.Intake)
	}
}

// A team with more than intakePages pages is truncated and keeps its
// cursor; once its read completes, the cursor moves.
func TestIntakeS1PagesStopAtIntakePages(t *testing.T) {
	e := setup(t)
	now := time.Now().UTC().Format(time.RFC3339Nano)
	e.f.extra = map[string]linearprovider.IssueV2{}
	for i := 0; i <= intakePages; i++ {
		id := "d1000000-0000-4000-8000-00000000000" + string(rune('0'+i))
		e.f.extra[id] = linearprovider.IssueV2{Issue: linearprovider.Issue{ID: id, Identifier: "PG-1", Title: "t", TeamID: team}, ProjectKnown: true, DescriptionKnown: true, UpdatedAt: now}
		e.f.labelled = append(e.f.labelled, id)
	}
	e.f.labelledPage = 1
	e.tick()
	var started string
	e.queryRow(`SELECT cursor_time FROM watch_cursors WHERE source='intake-s1'`, &started)
	e.dueNow()
	from := len(e.f.queries)
	tick := e.tick()
	var kept string
	e.queryRow(`SELECT cursor_time FROM watch_cursors WHERE source='intake-s1'`, &kept)
	if tick.Intake == nil || !slices.Contains(tick.Intake.Truncated, "intake-s1") || kept != started || queries(e, from, labelledQuery) != intakePages {
		t.Fatalf("truncated: %+v cursor %s→%s pages %d", tick.Intake, started, kept, queries(e, from, labelledQuery))
	}
	if tick.Intake.Calls < intakePages {
		t.Fatal("a page wasn't counted:", tick.Intake.Calls)
	}
	e.f.labelled = e.f.labelled[:intakePages]
	e.dueNow()
	tick = e.tick()
	var moved string
	e.queryRow(`SELECT cursor_time FROM watch_cursors WHERE source='intake-s1'`, &moved)
	if tick.Intake == nil || len(tick.Intake.Truncated) != 0 || moved <= started {
		t.Fatalf("complete: %+v cursor %s→%s", tick.Intake, started, moved)
	}
}

// Every page S1 reads is one charged call, rows or none: with the label
// set cached and no row to prove, the poll's calls are exactly its pages.
func TestIntakeS1ChargesEveryPage(t *testing.T) {
	e := setup(t)
	e.f.labelled, e.f.spill, e.f.labelledPage = []string{issueOut, issueOut}, true, 1
	e.tick()
	e.dueNow()
	e.tick() // resolves and caches the team's needs-agent set
	e.dueNow()
	from := len(e.f.queries)
	tick := e.tick()
	pages := queries(e, from, labelledQuery)
	if tick.Intake == nil || pages != 2 || tick.Intake.Calls != pages+queries(e, from, delegatedQuery)+queries(e, from, mentionsQuery) {
		t.Fatalf("calls %+v for %d pages", tick.Intake, pages)
	}
}

// labelSetsBy runs one intake tick with every team due and the label cache
// empty, the needs-agent sets read through the app's query or, refused,
// through LabelSetV2; it returns the set each team's labelled read asked
// for, the teams with none, and the provider's label listing pages.
func labelSetsBy(e *env, mcp bool) (map[string][]string, []string, int) {
	e.t.Helper()
	e.w.labels = nil
	e.f.refuseLabelSet = mcp
	e.f.labelSets, e.f.labelListings = nil, 0
	e.dueNow()
	tick := e.tick()
	if tick.Intake == nil || len(tick.Intake.Unreadable) != 0 {
		e.t.Fatalf("intake: %+v", tick.Intake)
	}
	return e.f.labelSets, slices.Sorted(slices.Values(tick.Intake.NoLabel)), e.f.labelListings
}

// The app's query gives each team the set LabelSetV2 gives, under its
// rules, with no label listing through the provider: a sub-team's own
// label and its bound parent's; a workspace label once; a team with a
// workspace label and its own of the name, or two of its own, none; an
// archived label, a group, another name's spelling and an unbound
// parent's label left out.
func TestNeedsAgentSetThroughTheAppMatchesLabelSetV2(t *testing.T) {
	needs := func(id, t string) linearprovider.Label {
		return linearprovider.Label{ID: id, Name: needsAgentLabel, TeamID: t, EligibilityKnown: true}
	}
	workspace := linearprovider.Label{ID: "b1000000-0000-4000-8000-0000000000aa", Name: needsAgentLabel, Global: true, EligibilityKnown: true}
	other := "b1000000-0000-4000-8000-0000000000bb"
	cases := map[string]struct {
		labels  map[string][]linearprovider.Label
		parents map[string]string
		want    map[string][]string
		none    []string
	}{
		"own and parent's": {
			want: map[string][]string{team: {labelNeeds}, gqlSub1: {gqlLabel1, labelNeeds}, gqlSub2: {gqlLabel2, labelNeeds}},
		},
		"workspace label": {
			labels: map[string][]linearprovider.Label{team: {workspace}, gqlSub1: {workspace}, gqlSub2: {needs(gqlLabel2, gqlSub2), workspace}},
			want:   map[string][]string{team: {workspace.ID}, gqlSub1: {workspace.ID}},
			none:   []string{gqlSub2},
		},
		"two of the team's own": {
			labels: map[string][]linearprovider.Label{team: {needs(labelNeeds, team)}, gqlSub1: {needs(gqlLabel1, gqlSub1), needs(other, gqlSub1)}, gqlSub2: {needs(gqlLabel2, gqlSub2)}},
			want:   map[string][]string{team: {labelNeeds}, gqlSub2: {gqlLabel2, labelNeeds}},
			none:   []string{gqlSub1},
		},
		"two of the parent's": {
			labels: map[string][]linearprovider.Label{team: {needs(labelNeeds, team), needs(other, team)}, gqlSub1: {needs(gqlLabel1, gqlSub1)}, gqlSub2: {needs(gqlLabel2, gqlSub2)}},
			want:   map[string][]string{},
			none:   slices.Sorted(slices.Values([]string{gqlSub1, gqlSub2, team})),
		},
		"archived, group and other spellings": {
			labels: map[string][]linearprovider.Label{
				team:    {needs(labelNeeds, team), {ID: other, Name: "Needs-Agent", TeamID: team, EligibilityKnown: true}},
				gqlSub1: {{ID: gqlLabel1, Name: needsAgentLabel, TeamID: gqlSub1, Archived: true, EligibilityKnown: true}, {ID: gqlLabelX, Name: needsAgentLabel, TeamID: gqlSub1, Group: true}},
				gqlSub2: {needs(gqlLabel2, gqlSub2)},
			},
			want: map[string][]string{team: {labelNeeds}, gqlSub1: {labelNeeds}, gqlSub2: {gqlLabel2, labelNeeds}},
		},
		"unbound parent": {
			labels:  map[string][]linearprovider.Label{team: {needs(labelNeeds, team)}, gqlSub1: {needs(gqlLabel1, gqlSub1)}, gqlSub2: {needs(gqlLabel2, gqlSub2)}, gqlUnbound: {needs(other, gqlUnbound)}},
			parents: map[string]string{gqlSub1: gqlUnbound, gqlSub2: team},
			want:    map[string][]string{team: {labelNeeds}, gqlSub1: {gqlLabel1}, gqlSub2: {gqlLabel2, labelNeeds}},
		},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			e, _, _ := s1Fixture(t)
			if c.labels != nil {
				e.f.teamLabels = c.labels
			}
			if c.parents != nil {
				e.f.parents = c.parents
			}
			mcpSets, mcpNone, listings := labelSetsBy(e, true)
			if listings == 0 || !maps.EqualFunc(mcpSets, c.want, slices.Equal) || !slices.Equal(mcpNone, c.none) {
				t.Fatalf("LabelSetV2 gave %v, none %v; the case wants %v, none %v", mcpSets, mcpNone, c.want, c.none)
			}
			from := len(e.f.queries)
			appSets, appNone, listings := labelSetsBy(e, false)
			if !maps.EqualFunc(appSets, mcpSets, slices.Equal) || !slices.Equal(appNone, mcpNone) {
				t.Fatalf("the app gave %v, none %v; LabelSetV2 %v, none %v", appSets, appNone, mcpSets, mcpNone)
			}
			if listings != 0 || queries(e, from, needsAgentQuery) != 3 {
				t.Fatalf("%d provider label pages and %d app label queries for 3 teams", listings, queries(e, from, needsAgentQuery))
			}
		})
	}
}

// When Linear refuses the query, the run resolves the rest of its sets
// through LabelSetV2 without asking the app again; a team the app can't
// see is resolved through LabelSetV2 alone.
func TestNeedsAgentSetFallsBackToLabelSetV2(t *testing.T) {
	e, _, sets := s1Fixture(t)
	from := len(e.f.queries)
	got, none, listings := labelSetsBy(e, true)
	if !maps.EqualFunc(got, sets, slices.Equal) || len(none) != 0 || listings == 0 || queries(e, from, needsAgentQuery) != 1 {
		t.Fatalf("refused: sets %v none %v, %d listings, %d app queries", got, none, listings, queries(e, from, needsAgentQuery))
	}
	e.f.hiddenTeams = map[string]bool{gqlSub1: true}
	got, none, listings = labelSetsBy(e, false)
	if !maps.EqualFunc(got, sets, slices.Equal) || len(none) != 0 || listings == 0 {
		t.Fatalf("hidden team: sets %v none %v, %d listings", got, none, listings)
	}
	// An answer about another team than the one asked is no answer.
	e.f.hiddenTeams, e.f.answerAs = nil, map[string]string{gqlSub2: team}
	got, none, listings = labelSetsBy(e, false)
	if !maps.EqualFunc(got, sets, slices.Equal) || len(none) != 0 || listings == 0 {
		t.Fatalf("another team's answer: sets %v none %v, %d listings", got, none, listings)
	}
}

// Another error from the app is the team's source unreadable, as for the
// labelled read; nothing falls back.
func TestNeedsAgentSetOtherErrorIsUnreadable(t *testing.T) {
	e, _, _ := s1Fixture(t)
	e.w.labels = nil
	e.f.labelSetErr = &linearapp.Error{Code: "query_failed"}
	e.f.labelListings = 0
	tick := e.tick()
	if tick.Intake == nil || !slices.Contains(tick.Intake.Unreadable, "intake-s1") || e.f.labelListings != 0 {
		t.Fatalf("intake: %+v, %d listings", tick.Intake, e.f.labelListings)
	}
}

// The query's pages are charged: once the candidates are queued, a tick
// with the cache empty charges one call per team more than one with it
// warm.
func TestNeedsAgentSetQueriesAreCharged(t *testing.T) {
	e, _, _ := s1Fixture(t)
	e.tick()
	e.dueNow()
	warm := e.tick()
	e.w.labels = nil
	e.dueNow()
	cold := e.tick()
	if cold.Intake == nil || warm.Intake == nil || cold.Intake.Calls-warm.Intake.Calls != 3 {
		t.Fatalf("cold %+v warm %+v", cold.Intake, warm.Intake)
	}
}
