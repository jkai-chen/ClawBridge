package linearboard

import (
	"context"
	"strings"
	"testing"

	"clawbridge/internal/bus"
	"clawbridge/internal/linearprovider"
)

const otherIssue = "e7e7e7e7-e7e7-4e7e-8e7e-e7e7e7e7e7e7"

// accessV2 binds teamID for writing and otherTeamID at access, with an
// issue in each (CROSS-TEAM-BINDING-DESIGN §3).
func accessV2(t *testing.T, access string) (*Service, *authorityV2, *providerV2, bus.Request) {
	t.Helper()
	s, a, p, scope := testV2(t)
	a.b.LinearTeamIDs, a.b.MappingVersion = []string{teamID}, 3
	a.b.LinearOtherTeams = map[string]string{otherTeamID: access}
	if access == bus.TeamAccessLinked {
		// A linked team has no pairs, so no bound project may be its alone.
		a.b.LinearProjectIDs = []string{projectA}
	}
	p.issues[otherIssue] = linearprovider.IssueV2{Issue: linearprovider.Issue{ID: otherIssue, TeamID: otherTeamID, ProjectID: projectB}, ProjectKnown: true, LabelsKnown: true, LabelIDs: []string{}, ParentKnown: true}
	return s, a, p, scope
}

func update(s *Service, scope bus.Request, op, issue string) error {
	title := "t"
	_, e := s.WriteV2(context.Background(), scope, WriteInputV2{WriteInput: WriteInput{Operation: "update", OperationID: op, IssueID: issue, Title: &title}})
	return e
}

func comment(s *Service, scope bus.Request, op, issue string) error {
	_, e := s.WriteV2(context.Background(), scope, WriteInputV2{WriteInput: WriteInput{Operation: "comment", OperationID: op, IssueID: issue, Body: "noted"}})
	return e
}

// listedTeams is the teams one full issue listing reads.
func listedTeams(t *testing.T, s *Service, p *providerV2, scope bus.Request) map[string]bool {
	t.Helper()
	seen, cursor := map[string]bool{}, ""
	for {
		p.queries = nil
		page, e := s.IssuesV2(context.Background(), scope, IssuesInputV2{Limit: 10, Cursor: cursor})
		if e != nil {
			t.Fatal(e)
		}
		for _, q := range p.queries {
			seen[q.TeamID] = true
		}
		if page.Complete {
			return seen
		}
		cursor = page.NextCursor
	}
}

// A read team is listed and read in full, and never written, a comment
// included: each refusal comes before any Linear write.
func TestReadTeamIsReadNeverWritten(t *testing.T) {
	s, _, p, scope := accessV2(t, bus.TeamAccessRead)
	if _, e := s.IssueV2(context.Background(), scope, otherIssue); e != nil {
		t.Fatal("a read team's issue unread:", e)
	}
	if teams := listedTeams(t, s, p, scope); !teams[teamID] || !teams[otherTeamID] {
		t.Fatal("a read team not listed:", teams)
	}
	for name, write := range map[string]func() error{
		"update":  func() error { return update(s, scope, "u", otherIssue) },
		"comment": func() error { return comment(s, scope, "c", otherIssue) },
	} {
		if e := write(); e == nil || !strings.Contains(e.Error(), "read_only_team") || p.writes != 0 || len(p.bodies) != 0 {
			t.Fatal(name, "written:", e, p.writes, p.bodies)
		}
	}
	// The write team is written as before.
	if e := update(s, scope, "w", issueID); e != nil || p.writes != 1 {
		t.Fatal("the write team refused:", e)
	}
}

// A comment team takes an agent's new comments, nothing else; a marker's
// comment needs a write team.
func TestCommentTeamTakesCommentsOnly(t *testing.T) {
	s, _, p, scope := accessV2(t, bus.TeamAccessComment)
	if e := comment(s, scope, "c", otherIssue); e != nil || len(p.bodies) != 1 {
		t.Fatal("a comment refused:", e, p.bodies)
	}
	if e := update(s, scope, "u", otherIssue); e == nil || !strings.Contains(e.Error(), "read_only_team") || p.writes != 1 {
		t.Fatal("an update written:", e, p.writes)
	}
	_, e := s.WriteV2(context.Background(), scope, WriteInputV2{WriteInput: WriteInput{Operation: "comment", OperationID: "m", IssueID: otherIssue, Body: "marker"}, marker: true})
	if e == nil || len(p.bodies) != 1 {
		t.Fatal("a marker comment posted on a comment team:", e)
	}
}

// A linked team is never listed, read in full or written.
func TestLinkedTeamIsNeverReadInFull(t *testing.T) {
	s, _, p, scope := accessV2(t, bus.TeamAccessLinked)
	if _, e := s.IssueV2(context.Background(), scope, otherIssue); e == nil || !strings.Contains(e.Error(), "linked_only") {
		t.Fatal("a linked team's issue read in full:", e)
	}
	if teams := listedTeams(t, s, p, scope); teams[otherTeamID] {
		t.Fatal("a linked team listed:", teams)
	}
	if e := comment(s, scope, "c", otherIssue); e == nil || !strings.Contains(e.Error(), "read_only_team") || len(p.bodies) != 0 {
		t.Fatal("a linked team written:", e)
	}
}

// The parent is Linear's: a link naming one shows every team's access, and
// is refused when Linear doesn't report a listed team as its sub-team.
func TestLinkVerifiesTheParentTeam(t *testing.T) {
	s, _, p, scope := testV2(t)
	in := LinkInputV2{TeamIDs: []string{teamID}, DefaultTeamID: teamID, ProjectIDs: []string{projectA, projectB}, ExpectedRevision: 1, OtherTeams: map[string]string{otherTeamID: bus.TeamAccessComment}, ParentTeamID: teamID}
	p.parents = map[string]string{otherTeamID: teamID}
	r, e := s.LinkV2(context.Background(), scope, in)
	if e != nil {
		t.Fatal(e)
	}
	if preview := r.(LinkPreviewV2); preview.ParentTeamID != teamID || preview.OtherTeams[otherTeamID] != bus.TeamAccessComment {
		t.Fatal(preview)
	}
	p.parents = nil
	if _, e = s.LinkV2(context.Background(), scope, in); e == nil || !strings.Contains(e.Error(), "sub-team") {
		t.Fatal("an unproven parent accepted:", e)
	}
	for name, bad := range map[string]LinkInputV2{
		"write and read":  {TeamIDs: []string{teamID}, DefaultTeamID: teamID, ProjectIDs: []string{projectA}, OtherTeams: map[string]string{teamID: bus.TeamAccessRead}},
		"unknown access":  {TeamIDs: []string{teamID}, DefaultTeamID: teamID, ProjectIDs: []string{projectA}, OtherTeams: map[string]string{otherTeamID: "admin"}},
		"unlisted parent": {TeamIDs: []string{teamID}, DefaultTeamID: teamID, ProjectIDs: []string{projectA}, ParentTeamID: otherTeamID},
	} {
		if e := ValidateLinkV2(bad); e == nil {
			t.Fatal(name, "accepted")
		}
	}
}

// A version 3 binding with one project never takes the legacy write path,
// which knows no access levels.
func TestCrossTeamBindingIsNeverLegacy(t *testing.T) {
	b := bus.TaskboardBindingV2{Mode: "project_set", LinearProjectIDs: []string{projectA}}
	if !legacyMappingV2(b) {
		t.Fatal("a one-project binding not legacy")
	}
	for _, v3 := range []bus.TaskboardBindingV2{
		{Mode: "project_set", LinearProjectIDs: []string{projectA}, LinearOtherTeams: map[string]string{otherTeamID: bus.TeamAccessRead}},
		{Mode: "team_board", LinearParentTeamID: teamID},
	} {
		if legacyMappingV2(v3) {
			t.Fatal("a version 3 binding served by the legacy path:", v3)
		}
	}
}

// recordingLinkV2 answers the link's write with the arguments it was sent.
type recordingLinkV2 struct {
	*authorityV2
	args map[string]any
}

func (r *recordingLinkV2) Execute(ctx context.Context, q bus.Request) (bus.Result, error) {
	if q.Operation == "taskboard_set_v2" {
		r.args = q.Args
		return bus.Result{Source: "local-authority", Data: map[string]any{"revision": 2}}, nil
	}
	return r.authorityV2.Execute(ctx, q)
}

// A confirmed link sends version 3's fields only when it uses them, so an
// older server refuses it instead of storing a binding it can't read.
func TestLinkSendsVersion3FieldsOnlyWhenUsed(t *testing.T) {
	s, a, p, scope := testV2(t)
	r := &recordingLinkV2{authorityV2: a}
	s.cfg.Executor = r
	p.parents = map[string]string{otherTeamID: teamID}
	in := LinkInputV2{TeamIDs: []string{teamID}, DefaultTeamID: teamID, ProjectIDs: []string{}, TeamBoard: true, ExpectedRevision: 1, Confirm: true}
	if _, e := s.LinkV2(context.Background(), scope, in); e != nil {
		t.Fatal(e)
	}
	if _, sent := r.args["linear_other_teams"]; sent {
		t.Fatal("version 3 fields sent for a version 2 binding:", r.args)
	}
	in.OtherTeams, in.ParentTeamID = map[string]string{otherTeamID: bus.TeamAccessRead}, teamID
	if _, e := s.LinkV2(context.Background(), scope, in); e != nil {
		t.Fatal(e)
	}
	if others, _ := r.args["linear_other_teams"].(map[string]any); others[otherTeamID] != bus.TeamAccessRead || r.args["linear_parent_team_id"] != teamID {
		t.Fatal("version 3 fields not sent:", r.args)
	}
}
