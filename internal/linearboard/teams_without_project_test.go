package linearboard

import (
	"context"
	"strings"
	"testing"

	"clawbridge/internal/bus"
	"clawbridge/internal/linearprovider"
)

// withoutProjectV2 binds teamID (carried by projectA) and otherTeamID,
// listed without a project: no bound project carries it yet.
func withoutProjectV2(t *testing.T) (*Service, *authorityV2, *providerV2, bus.Request) {
	t.Helper()
	s, a, p, scope := testV2(t)
	a.b.LinearProjectIDs = []string{projectA}
	a.b.TeamsWithoutProject = []string{otherTeamID}
	a.b.MappingVersion = 4
	p.projects = map[string][]string{projectA: {teamID}}
	p.issues[otherIssue] = linearprovider.IssueV2{Issue: linearprovider.Issue{ID: otherIssue, TeamID: otherTeamID, ProjectID: projectA}, ProjectKnown: true, LabelsKnown: true, LabelIDs: []string{}, ParentKnown: true, RelationsKnown: true}
	return s, a, p, scope
}

func withoutProject(e error) bool {
	return IsTeamWithoutProject(e) && strings.Contains(e.Error(), "team "+otherTeamID+" has no project yet")
}

func createIn(s *Service, scope bus.Request, op, team, project string) error {
	_, e := s.WriteV2(context.Background(), scope, WriteInputV2{WriteInput: WriteInput{Operation: "create", OperationID: op, Title: ptrV2("t")}, TeamID: team, Project: linearprovider.Field[*string]{Present: true, Value: ptrV2(project)}})
	return e
}

// A team listed without a project is inert: the binding verifies, the board
// lists only the other team, and every issue read or write of the team is
// refused by name, with nothing sent. The same team not listed so is still
// refused as unrepresented, so a typo'd team is caught.
func TestTeamWithoutProjectIsInert(t *testing.T) {
	s, a, p, scope := withoutProjectV2(t)
	if _, e := verifyBindingV2(context.Background(), p, a.b); e != nil {
		t.Fatal(e)
	}
	unlisted := a.b
	unlisted.TeamsWithoutProject = nil
	if _, e := verifyBindingV2(context.Background(), p, unlisted); e == nil || !strings.Contains(e.Error(), "not represented") {
		t.Fatal("an unrepresented team not listed so accepted:", e)
	}
	p.pages = []linearprovider.Page[linearprovider.IssueV2]{{Complete: true}}
	if page, e := s.IssuesV2(context.Background(), scope, IssuesInputV2{Limit: 10}); e != nil || !page.Complete || len(p.queries) != 1 || p.queries[0].TeamID != teamID {
		t.Fatal("the board read didn't list just the other team:", page, e, p.queries)
	}
	if e := createIn(s, scope, "n-inert", otherTeamID, projectA); !withoutProject(e) || len(p.created) != 0 {
		t.Fatal("a create in an inert team:", e, p.created)
	}
	if e := update(s, scope, "u-inert", otherIssue); !withoutProject(e) || p.writes != 0 {
		t.Fatal("an update in an inert team:", e)
	}
	if _, e := s.IssueV2(context.Background(), scope, otherIssue); !withoutProject(e) {
		t.Fatal("an inert team's issue read:", e)
	}
	if _, e := s.TeamLabelledV2(context.Background(), scope, TeamLabelledInputV2{TeamID: otherTeamID, LabelID: labelA, Limit: 10, UpdatedAfter: "2026-10-01T00:00:00Z"}); !withoutProject(e) {
		t.Fatal("an inert team's labelled issues read:", e)
	}
	// The other team works as before.
	if e := createIn(s, scope, "n-ok", teamID, projectA); e != nil || len(p.created) != 1 {
		t.Fatal("a create in the carried team refused:", e)
	}
}

// QA2 end to end: once a bound project carries the team (FR10's add), the
// next verify treats it as ordinary with no relink, and its entry is
// reported stale; if the project later drops it, it is inert again rather
// than the binding being refused.
func TestTeamWithoutProjectBecomesOrdinaryAndBack(t *testing.T) {
	s, _, p, scope := withoutProjectV2(t)
	state := func() TeamWithoutProject {
		t.Helper()
		teams, e := s.TeamsWithoutProjectV2(context.Background(), scope)
		if e != nil || len(teams) != 1 || teams[0].TeamID != otherTeamID {
			t.Fatal(teams, e)
		}
		return teams[0]
	}
	if got := state(); !got.Inert || !strings.Contains(got.Label(), "no project yet (inert)") {
		t.Fatal(got, got.Label())
	}
	p.projects[projectA] = []string{teamID, otherTeamID}
	if got := state(); got.Inert || !strings.Contains(got.Label(), "stale entry") {
		t.Fatal(got, got.Label())
	}
	if e := createIn(s, scope, "n-added", otherTeamID, projectA); e != nil || len(p.created) != 1 {
		t.Fatal("a create in the team once carried:", e)
	}
	if e := update(s, scope, "u-added", otherIssue); e != nil {
		t.Fatal("an update in the team once carried:", e)
	}
	p.projects[projectA] = []string{teamID}
	if got := state(); !got.Inert {
		t.Fatal("a dropped team not inert again:", got)
	}
	if e := createIn(s, scope, "n-dropped", otherTeamID, projectA); !withoutProject(e) {
		t.Fatal("a create in the team once dropped:", e)
	}
}

// A binding listing none reads nothing from Linear for doctor or the TUI.
func TestTeamsWithoutProjectReadsNothingWhenNoneListed(t *testing.T) {
	s, _, p, scope := testV2(t)
	if teams, e := s.TeamsWithoutProjectV2(context.Background(), scope); e != nil || teams != nil || p.tools != 0 {
		t.Fatal(teams, e, p.tools)
	}
}

// The link preview says what a team without a project means, and the
// confirmed link sends the list; a listed team a bound project already
// carries is dropped with a note (how a relink tidies a stale entry). The
// list is a subset of the write teams, never the default, and only for a
// project set.
func TestLinkTeamWithoutProject(t *testing.T) {
	s, a, p, scope := testV2(t)
	p.projects = map[string][]string{projectA: {teamID}}
	in := LinkInputV2{TeamIDs: []string{teamID, otherTeamID}, DefaultTeamID: teamID, ProjectIDs: []string{projectA}, ExpectedRevision: 1}
	if _, e := s.LinkV2(context.Background(), scope, in); e == nil || !strings.Contains(e.Error(), "not represented") {
		t.Fatal("an unrepresented team linked without the flag:", e)
	}
	in.TeamsWithoutProject = []string{otherTeamID}
	r, e := s.LinkV2(context.Background(), scope, in)
	if e != nil {
		t.Fatal(e)
	}
	preview := r.(LinkPreviewV2)
	if len(preview.TeamsWithoutProject) != 1 || len(preview.WithoutProject) != 1 || preview.WithoutProject[0] != "no bound project carries team "+otherTeamID+" yet; it reads nothing and takes no issue writes until one does" {
		t.Fatal(preview)
	}
	rec := &recordingLinkV2{authorityV2: a}
	s.cfg.Executor = rec
	in.Confirm = true
	if _, e = s.LinkV2(context.Background(), scope, in); e != nil {
		t.Fatal(e)
	}
	if sent, _ := rec.args["teams_without_project"].([]string); len(sent) != 1 || sent[0] != otherTeamID {
		t.Fatal("the list not sent:", rec.args)
	}
	// Carried now: the entry is dropped, and nothing of version 4 is sent.
	p.projects[projectA] = []string{teamID, otherTeamID}
	rec.args = nil
	if _, e = s.LinkV2(context.Background(), scope, in); e != nil {
		t.Fatal(e)
	}
	if _, sent := rec.args["teams_without_project"]; sent {
		t.Fatal("a carried team sent as without a project:", rec.args)
	}
	in.Confirm = false
	s.cfg.Executor = a
	r, _ = s.LinkV2(context.Background(), scope, in)
	if preview := r.(LinkPreviewV2); len(preview.TeamsWithoutProject) != 0 || len(preview.WithoutProject) != 1 || !strings.Contains(preview.WithoutProject[0], "its entry is dropped") {
		t.Fatal(preview)
	}
	for name, bad := range map[string]LinkInputV2{
		"not a write team": {TeamIDs: []string{teamID}, DefaultTeamID: teamID, ProjectIDs: []string{projectA}, TeamsWithoutProject: []string{otherTeamID}},
		"the default":      {TeamIDs: []string{teamID, otherTeamID}, DefaultTeamID: teamID, ProjectIDs: []string{projectA}, TeamsWithoutProject: []string{teamID}},
		"twice":            {TeamIDs: []string{teamID, otherTeamID}, DefaultTeamID: teamID, ProjectIDs: []string{projectA}, TeamsWithoutProject: []string{otherTeamID, otherTeamID}},
		"a team board":     {TeamIDs: []string{teamID, otherTeamID}, DefaultTeamID: teamID, ProjectIDs: []string{}, TeamBoard: true, TeamsWithoutProject: []string{otherTeamID}},
	} {
		if e := ValidateLinkV2(bad); e == nil {
			t.Fatal(name, "accepted")
		}
	}
}

// The app route is refused the same way: the server's app writer runs
// this same write, with this same check, and sends nothing to Linear.
func TestTeamWithoutProjectRefusesAppRoutedWrites(t *testing.T) {
	plain, a, p, scope := withoutProjectV2(t)
	var refused error
	server := &appServer{status: appOn, write: func(r bus.Request) (bus.Result, error) {
		args, _ := r.Args["args"].(map[string]any)
		in, e := parseWriteArgsV2(r.Args["operation"].(string), args)
		if e == nil {
			_, e = plain.WriteV2(context.Background(), scope, in)
		}
		refused = e
		return bus.Result{}, bus.PublicError("invalid", e.Error())
	}}
	s, _, _ := routedV2(t, server)
	server.authorityV2.b = a.b
	s.cfg.Open = plain.cfg.Open
	createIn(s, scope, "n-app", otherTeamID, projectA)
	if len(server.writes) != 1 || !withoutProject(refused) || len(p.created) != 0 {
		t.Fatal("an app-routed create in an inert team:", refused, len(server.writes), p.created)
	}
}
