package linearboard

import (
	"context"
	"errors"
	"strings"
	"testing"

	"clawbridge/internal/bus"
	"clawbridge/internal/linearprovider"
)

// lanesV2 binds two write teams with a lane each, "near" writing teamID and
// "far" writing otherTeamID, and puts the session in lane, with an issue in
// each team (CROSS-TEAM-BINDING-DESIGN §3.1).
func lanesV2(t *testing.T, lane string, teams ...string) (*Service, *authorityV2, *providerV2, bus.Request) {
	t.Helper()
	s, a, p, scope := testV2(t)
	a.b.MappingVersion = 3
	a.b.Lanes = []bus.Lane{{Name: "far", WriteTeamIDs: []string{otherTeamID}, DefaultTeamID: otherTeamID}, {Name: "near", WriteTeamIDs: []string{teamID}, DefaultTeamID: teamID}}
	a.b.Unpinned = bus.LaneReadOnly
	a.lane, a.laneTeams = lane, append([]string{}, teams...)
	p.issues[otherIssue] = linearprovider.IssueV2{Issue: linearprovider.Issue{ID: otherIssue, TeamID: otherTeamID, ProjectID: projectB}, ProjectKnown: true, LabelsKnown: true, LabelIDs: []string{}, ParentKnown: true, RelationsKnown: true}
	return s, a, p, scope
}

func outsideLane(e error) bool { return e != nil && strings.Contains(e.Error(), "outside_lane") }

// A session writes its lane's teams and is refused on another lane's, with
// nothing sent; the refusal names both lanes. The client asks the server
// for the lane.
func TestLaneWritesOnlyItsTeams(t *testing.T) {
	s, a, p, scope := lanesV2(t, "near", teamID)
	if e := update(s, scope, "in", issueID); e != nil || p.writes != 1 {
		t.Fatal("the lane's own team refused:", e)
	}
	if a.accessArgs["lane_scope"] != 1 {
		t.Fatal("the lane was never asked for:", a.accessArgs)
	}
	e := update(s, scope, "out", otherIssue)
	if !outsideLane(e) || !strings.Contains(e.Error(), "written by lane far") || !strings.Contains(e.Error(), "you are in lane near") || p.writes != 1 {
		t.Fatal("another lane's team written:", e, p.writes)
	}
	if e = comment(s, scope, "c-out", otherIssue); !outsideLane(e) || len(p.bodies) != 0 {
		t.Fatal("a comment on another lane's write team:", e, p.bodies)
	}
	// A create lands in the lane's teams only.
	create := func(op, team, project string) error {
		_, e := s.WriteV2(context.Background(), scope, WriteInputV2{WriteInput: WriteInput{Operation: "create", OperationID: op, Title: ptrV2("t")}, TeamID: team, Project: linearprovider.Field[*string]{Present: true, Value: ptrV2(project)}})
		return e
	}
	if e = create("n-out", otherTeamID, projectB); !outsideLane(e) || len(p.created) != 0 {
		t.Fatal("a create in another lane's team:", e, p.created)
	}
	if e = create("n-in", teamID, projectA); e != nil || len(p.created) != 1 {
		t.Fatal("a create in the lane refused:", e)
	}
}

// An outside_lane refusal names where the server found the lane, after the
// lane clause; the client asks for the source alongside the lane.
func TestLaneRefusalNamesTheLaneSource(t *testing.T) {
	wrong := "outside_lane: team " + otherTeamID + " is written by lane far; you are in lane near"
	none := "outside_lane: team " + teamID + " is written by lane near; this session has no lane (unpinned sessions are read-only, or its pinned lane is gone)"
	for from, want := range map[string]string{
		bus.LaneFromSessionPin:      wrong + " (lane from the session's pin); nothing was sent",
		bus.LaneFromMovedSessionPin: wrong + " (lane from a pin of the session it moved from); nothing was sent",
		bus.LaneFromPrincipalPin:    wrong + " (lane from your own pin as a person); nothing was sent",
	} {
		s, a, p, scope := lanesV2(t, "near", teamID)
		a.laneFrom = from
		if e := update(s, scope, "out", otherIssue); e == nil || e.Error() != want || p.writes != 0 {
			t.Fatal(from, e)
		}
		if a.accessArgs["lane_from"] != 1 || a.accessArgs["lane_scope"] != 1 {
			t.Fatal("the lane source was never asked for:", a.accessArgs)
		}
	}
	s, a, p, scope := lanesV2(t, bus.LaneReadOnly)
	a.laneFrom = bus.LaneFromUnpinned
	if e := update(s, scope, "ro", issueID); e == nil || e.Error() != none+" (lane from the binding's unpinned setting); nothing was sent" || p.writes != 0 {
		t.Fatal("unpinned:", e)
	}
}

// A call that named no session is told how to name one, after the lane
// source; "nothing was sent" stays last.
func TestLaneRefusalWithoutASession(t *testing.T) {
	s, a, p, scope := lanesV2(t, bus.LaneReadOnly)
	a.actor, a.name, a.laneFrom = "", "", bus.LaneFromUnpinned
	scope.Session, scope.Args = "", map[string]any{}
	want := "outside_lane: team " + teamID + " is written by lane near; this session has no lane (unpinned sessions are read-only, or its pinned lane is gone) (lane from the binding's unpinned setting); this call named no session: pass --session, or set CLAWBRIDGE_SESSION; nothing was sent"
	if e := update(s, scope, "ro", issueID); e == nil || e.Error() != want || p.writes != 0 {
		t.Fatal(e)
	}
	b := bus.TaskboardBindingV2{LinearTeamIDs: []string{teamID, otherTeamID}, Lanes: []bus.Lane{{Name: "near", WriteTeamIDs: []string{teamID}, DefaultTeamID: teamID}}}
	access := bus.TaskboardAccess{Lane: "near", LaneWriteTeamIDs: []string{teamID}, LaneFrom: bus.LaneFromPrincipalPin}
	want = "outside_lane: no lane writes team " + otherTeamID + "; you are in lane near (lane from your own pin as a person); this call named no session: pass --session, or set CLAWBRIDGE_SESSION; nothing was sent"
	if e := checkLaneV2(b, access, "", otherTeamID); e == nil || e.Error() != want {
		t.Fatal(e)
	}
}

// v0311Server is a v0.3.11 server: it refuses lane_from as an argument it
// doesn't know and names no lane source.
type v0311Server struct{ *authorityV2 }

func (o v0311Server) Execute(ctx context.Context, r bus.Request) (bus.Result, error) {
	if r.Operation == "taskboard_access" && r.Args["lane_from"] != nil {
		return bus.Result{}, errors.New(`unsupported argument "lane_from" for taskboard_access`)
	}
	return o.authorityV2.Execute(ctx, r)
}

// Against a v0.3.11 server the client drops lane_from, and a refusal reads
// as it does today.
func TestLaneRefusalFromAnOlderServer(t *testing.T) {
	s, a, p, scope := lanesV2(t, "near", teamID)
	a.laneFrom = bus.LaneFromSessionPin
	s.cfg.Executor = v0311Server{a}
	if e := update(s, scope, "in", issueID); e != nil || p.writes != 1 {
		t.Fatal("the lane's own team refused:", e)
	}
	if _, asked := a.accessArgs["lane_from"]; asked || a.accessArgs["lane_scope"] != 1 {
		t.Fatal("lane_from kept, or the lane dropped:", a.accessArgs)
	}
	if e := update(s, scope, "out", otherIssue); e == nil || e.Error() != "outside_lane: team "+otherTeamID+" is written by lane far; you are in lane near; nothing was sent" || p.writes != 1 {
		t.Fatal("not today's text:", e)
	}
	s, a, _, scope = lanesV2(t, bus.LaneReadOnly)
	s.cfg.Executor = v0311Server{a}
	if e := update(s, scope, "ro", issueID); e == nil || e.Error() != "outside_lane: team "+teamID+" is written by lane near; this session has no lane (unpinned sessions are read-only, or its pinned lane is gone); nothing was sent" {
		t.Fatal("not today's read-only text:", e)
	}
}

// Unpinned sessions are read-only by default; a binding with lanes refuses
// every write when the server names no lane; a binding without lanes
// writes as before, whatever the server says.
func TestLaneReadOnlyAndUnnamed(t *testing.T) {
	s, _, p, scope := lanesV2(t, bus.LaneReadOnly)
	if e := update(s, scope, "ro", issueID); !outsideLane(e) || !strings.Contains(e.Error(), "no lane") || p.writes != 0 {
		t.Fatal("a read-only session wrote:", e)
	}
	s, a, p, scope := lanesV2(t, "")
	a.laneTeams = nil
	if e := update(s, scope, "none", issueID); !outsideLane(e) || !strings.Contains(e.Error(), "upgrade the server") || p.writes != 0 {
		t.Fatal("a session with no lane named wrote:", e)
	}
	s, a, p, scope = testV2(t)
	a.lane, a.laneTeams = bus.LaneReadOnly, []string{}
	if e := update(s, scope, "plain", issueID); e != nil || p.writes != 1 {
		t.Fatal("a binding without lanes refused:", e)
	}
}

// A comment team takes comments from every lane; a marker is ClawBridge's
// own bookkeeping, not the session's write.
func TestLaneLeavesCommentTeamsAndMarkers(t *testing.T) {
	s, a, p, scope := lanesV2(t, "near", teamID)
	a.b.LinearTeamIDs = []string{teamID}
	a.b.Lanes = a.b.Lanes[1:]
	a.b.LinearOtherTeams = map[string]string{otherTeamID: bus.TeamAccessComment}
	if e := comment(s, scope, "c", otherIssue); e != nil || len(p.bodies) != 1 {
		t.Fatal("a comment team refused a lane's comment:", e)
	}
	s, _, p, scope = lanesV2(t, "near", teamID)
	if _, e := s.WriteV2(context.Background(), scope, WriteInputV2{WriteInput: WriteInput{Operation: "comment", OperationID: markerOperation(), IssueID: otherIssue, Body: "marker"}, marker: true}); e != nil || len(p.bodies) != 1 {
		t.Fatal("a marker refused by a lane:", e)
	}
}

// The lane is checked again inside the write lock: a pin changed after the
// first check stops the write, and nothing is sent.
func TestLaneChangedBeforeDispatch(t *testing.T) {
	s, a, p, scope := lanesV2(t, "near", teamID)
	a.afterAccess = func(n int) {
		if n >= 2 {
			a.lane, a.laneTeams = bus.LaneReadOnly, []string{}
		}
	}
	if e := update(s, scope, "moved", issueID); !outsideLane(e) || !strings.Contains(e.Error(), "lane changed") || p.writes != 0 {
		t.Fatal("a write went out after its lane changed:", e, p.writes)
	}
	s, a, p, scope = lanesV2(t, "near", teamID)
	a.afterAccess = func(n int) {
		if n >= 2 {
			a.lane, a.laneTeams = bus.LaneReadOnly, []string{}
		}
	}
	if _, e := s.RelationV2(context.Background(), scope, RelationInputV2{OperationID: "r-moved", Action: "add", Type: "blocks", FromID: issueID, ToID: otherIssue}); !outsideLane(e) || p.writes != 0 {
		t.Fatal("a relation went out after its lane changed:", e, p.writes)
	}
}

// X2: a relation add needs the caller's own end in the lane, whatever the
// type's normal direction; a remove needs the stored edge's from end.
func TestLaneRelations(t *testing.T) {
	relation := func(s *Service, scope bus.Request, op, action, kind, from, to string) error {
		_, e := s.RelationV2(context.Background(), scope, RelationInputV2{OperationID: op, Action: action, Type: kind, FromID: from, ToID: to})
		return e
	}
	s, _, p, scope := lanesV2(t, "near", teamID)
	if e := relation(s, scope, "r1", "add", "blocks", issueID, otherIssue); e != nil || p.writes != 1 {
		t.Fatal("own end in lane refused:", e)
	}
	if e := relation(s, scope, "r2", "add", "blocks", otherIssue, issueID); !outsideLane(e) || p.writes != 1 {
		t.Fatal("another lane's end as source:", e)
	}
	// blocked-by swaps the ends; the caller's own end stays the source.
	if e := relation(s, scope, "r3", "add", "blocked-by", issueID, otherIssue); e != nil || p.writes != 2 {
		t.Fatal("blocked-by from own end refused:", e)
	}
	if e := relation(s, scope, "r4", "add", "related", otherIssue, issueID); !outsideLane(e) {
		t.Fatal("related from another lane's end:", e)
	}
	// Remove: the stored edge runs far → near, so near can't remove it.
	far := p.issues[otherIssue]
	far.Relations = []linearprovider.Relation{{ID: "99999999-9999-4999-8999-999999999999", Type: "related", FromID: otherIssue, ToID: issueID}}
	p.issues[otherIssue] = far
	near := p.issues[issueID]
	near.RelationsKnown = true
	p.issues[issueID] = near
	if e := relation(s, scope, "r5", "remove", "related", issueID, otherIssue); !outsideLane(e) || p.writes != 2 {
		t.Fatal("removed another lane's stored edge:", e)
	}
	far.Relations[0].FromID, far.Relations[0].ToID = issueID, otherIssue
	p.issues[otherIssue] = far
	if e := relation(s, scope, "r6", "remove", "related", otherIssue, issueID); e != nil || p.writes != 3 {
		t.Fatal("own stored edge's removal refused:", e)
	}
}

// An agent's context shows its lane and the teams it writes.
func TestContextShowsTheLane(t *testing.T) {
	s, _, _, scope := lanesV2(t, "near", teamID)
	ctx, e := s.ContextV2(context.Background(), scope)
	if e != nil || ctx.Lane == nil || ctx.Lane.Name != "near" || len(ctx.Lane.WriteTeamIDs) != 1 {
		t.Fatal(ctx.Lane, e)
	}
	s, _, _, scope = testV2(t)
	if ctx, e = s.ContextV2(context.Background(), scope); e != nil || ctx.Lane != nil {
		t.Fatal("a lane without lanes:", ctx.Lane, e)
	}
}

// A link sends lanes and unpinned only with a binding that has them; the
// link is checked against the binding's rules before anything is sent.
func TestLinkLanesValidation(t *testing.T) {
	in := LinkInputV2{TeamIDs: []string{teamID, otherTeamID}, DefaultTeamID: teamID, TeamBoard: true, Lanes: []bus.Lane{{Name: "near", WriteTeamIDs: []string{teamID}, DefaultTeamID: teamID}}}
	if e := ValidateLinkV2(in); e != nil {
		t.Fatal(e)
	}
	for name, change := range map[string]func(*LinkInputV2){
		"not a write team": func(in *LinkInputV2) { in.Lanes[0].WriteTeamIDs = []string{projectA} },
		"bad name":         func(in *LinkInputV2) { in.Lanes[0].Name = "Near" },
		"unknown unpinned": func(in *LinkInputV2) { in.Unpinned = "far" },
		"unpinned alone":   func(in *LinkInputV2) { in.Lanes = nil; in.Unpinned = bus.LaneReadOnly },
	} {
		bad := in
		bad.Lanes = append([]bus.Lane{}, in.Lanes...)
		change(&bad)
		if e := ValidateLinkV2(bad); e == nil {
			t.Fatal(name, "validated")
		}
	}
}
