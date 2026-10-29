package linearboard

import (
	"context"
	"errors"
	"strings"
	"testing"

	"clawbridge/internal/bus"
	"clawbridge/internal/linearprovider"
)

const kindLabel = "e5000000-0000-4000-8000-000000000005"

// creates is the server side of a coordinator's create, as the fake
// authority answers it: the reservations and settles it was sent.
type creates struct {
	coordinator bool
	reserves    []map[string]any
	settles     []map[string]any
	reserveErr  error
	// label, when set, is the kind label the reservation answers.
	label any
	// kinds answers project_kind_get for the owner's --kind.
	kinds map[string]any
}

func (c *creates) execute(r bus.Request) (bus.Result, bool, error) {
	switch r.Operation {
	case "project_create_reserve":
		if c.reserveErr != nil {
			return bus.Result{}, true, c.reserveErr
		}
		c.reserves = append(c.reserves, r.Args)
		label := any(kindLabel)
		if c.label != nil {
			label = c.label
		}
		return bus.Result{Source: "local-authority", Data: map[string]any{"state": "reserved", "kind_label_id": label}}, true, nil
	case "project_create_settle":
		c.settles = append(c.settles, r.Args)
		return bus.Result{Source: "local-authority", Data: map[string]any{"state": r.Args["outcome"], "owner_sessions_told": 2}}, true, nil
	case "project_kind_get":
		return bus.Result{Source: "local-authority", Data: c.kinds}, true, nil
	}
	return bus.Result{}, false, nil
}

// coordinatorTest is adminTest with session ses as the project's
// designated coordinator, a member.
func coordinatorTest(t *testing.T) (*Service, *adminAuthority, *adminFake, *creates, bus.Request) {
	t.Helper()
	s, a, p, scope := adminTest(t)
	a.role = "member"
	c := &creates{coordinator: true}
	a.creates = c
	return s, a, p, c, scope
}

func coordinatorCreate(op, team, kind string) AdminInput {
	return AdminInput{Kind: "project_create", OperationID: op, TeamID: team, Name: "Q4", ProjectKind: kind}
}

// The coordinator creates in its default team with the kind's label: the
// reservation comes before anything is sent, the settle names the project
// after, and the result says no lead was set and how the owner was told.
func TestCoordinatorCreatesAProject(t *testing.T) {
	s, a, p, c, scope := coordinatorTest(t)
	p.afterWrite = func() {
		if len(c.reserves) != 1 || len(c.settles) != 0 {
			t.Fatal("the create was sent before its reservation:", c.reserves, c.settles)
		}
	}
	r, e := s.Admin(context.Background(), scope, coordinatorCreate("c-1", teamID, "Engineering"))
	if e != nil || r.Outcome != "accepted" || r.ID != newProject || r.KindLabelID != kindLabel || r.Lead == nil || r.Lead.Note != noLeadYet || !strings.Contains(r.OwnerNotice, "told (2") {
		t.Fatal(r, e)
	}
	if len(p.writes) != 1 || p.writes[0] != "project "+teamID+" Q4 "+kindLabel {
		t.Fatal(p.writes)
	}
	if a.accessArgs["coordinator_scope"] != 1 {
		t.Fatal("the coordinator was never asked for:", a.accessArgs)
	}
	res := c.reserves[0]
	if res["operation_id"] != "c-1" || res["team_id"] != teamID || res["kind"] != "Engineering" || res["name_sha256"] != nameDigest("Q4") || res["instance"] == nil {
		t.Fatal(res)
	}
	if len(c.settles) != 1 || c.settles[0]["outcome"] != "created" || c.settles[0]["linear_project_id"] != newProject {
		t.Fatal(c.settles)
	}
	// F3: a retry after a lost answer reads the receipt back, creates
	// nothing, and settles again (the server repeats it safely).
	again, e := s.Admin(context.Background(), scope, coordinatorCreate("c-1", teamID, "Engineering"))
	if e != nil || again.ID != newProject || len(p.writes) != 1 || len(c.settles) != 2 || c.settles[1]["linear_project_id"] != newProject {
		t.Fatal("retry:", again, e, p.writes, c.settles)
	}
}

// Every refusal the plan names comes before the reservation: nothing is
// recorded and nothing is sent.
func TestCoordinatorCreateRefusals(t *testing.T) {
	for name, c := range map[string]struct {
		in    AdminInput
		setup func(*adminAuthority, *creates)
		want  string
	}{
		"not the coordinator": {coordinatorCreate("x", teamID, "Engineering"), func(_ *adminAuthority, c *creates) { c.coordinator = false }, "project owner only"},
		"a label":             {AdminInput{Kind: "label_create", OperationID: "x", TeamID: teamID, Name: "Bug"}, nil, "project owner only"},
		"an initiative":       {AdminInput{Kind: "initiative_create", OperationID: "x", Name: "Big"}, nil, "project owner only"},
		"no kind":             {coordinatorCreate("x", teamID, ""), nil, "needs --kind"},
		"closes":              {AdminInput{Kind: "project_create", OperationID: "x", TeamID: teamID, Name: "Q4", ProjectKind: "Engineering", Closes: ClosesAgent}, nil, "--closes on project create is the project owner's"},
		"a profile link":      {AdminInput{Kind: "project_create", OperationID: "x", TeamID: teamID, Name: "see linear.app/acme/profiles", ProjectKind: "Engineering"}, nil, "mention"},
		"another team":        {coordinatorCreate("x", otherTeamID, "Engineering"), nil, "default team " + teamID},
	} {
		s, a, p, cr, scope := coordinatorTest(t)
		if c.setup != nil {
			c.setup(a, cr)
		}
		_, e := s.Admin(context.Background(), scope, c.in)
		if e == nil || !strings.Contains(e.Error(), c.want) || len(cr.reserves) != 0 || len(p.writes) != 0 {
			t.Fatal(name, e, cr.reserves, p.writes)
		}
	}
	// A reservation naming no valid label sends nothing.
	s, _, p, cr, scope := coordinatorTest(t)
	cr.label = "not-a-label"
	if _, e := s.Admin(context.Background(), scope, coordinatorCreate("x", teamID, "Engineering")); e == nil || !strings.Contains(e.Error(), "named no kind label") || len(p.writes) != 0 {
		t.Fatal(e, p.writes)
	}
	// The server's refusal of the reservation sends nothing.
	s, _, p, cr, scope = coordinatorTest(t)
	cr.reserveErr = errors.New("the kind's label isn't seeded")
	if _, e := s.Admin(context.Background(), scope, coordinatorCreate("x", teamID, "Engineering")); e == nil || !strings.Contains(e.Error(), "could not be reserved") || len(p.writes) != 0 || len(cr.settles) != 0 {
		t.Fatal(e, p.writes)
	}
}

// With lanes, the lane's default team only (Q-A); a session with no lane
// creates nothing.
func TestCoordinatorCreateLanes(t *testing.T) {
	s, a, p, cr, scope := coordinatorTest(t)
	a.b.MappingVersion = 3
	a.b.Lanes = []bus.Lane{{Name: "near", WriteTeamIDs: []string{teamID, otherTeamID}, DefaultTeamID: otherTeamID}}
	a.b.Unpinned = bus.LaneReadOnly
	a.lane, a.laneTeams = "near", []string{teamID, otherTeamID}
	if _, e := s.Admin(context.Background(), scope, coordinatorCreate("x", teamID, "Engineering")); !outsideLane(e) || !strings.Contains(e.Error(), "default team "+otherTeamID) || len(cr.reserves) != 0 {
		t.Fatal("a lane's other team:", e)
	}
	a.lane, a.laneTeams = bus.LaneReadOnly, []string{}
	if _, e := s.Admin(context.Background(), scope, coordinatorCreate("y", otherTeamID, "Engineering")); !outsideLane(e) || !strings.Contains(e.Error(), "no lane") || len(cr.reserves) != 0 {
		t.Fatal("a read-only coordinator:", e)
	}
	a.laneTeams = nil
	if _, e := s.Admin(context.Background(), scope, coordinatorCreate("z", otherTeamID, "Engineering")); !outsideLane(e) || !strings.Contains(e.Error(), "named no lane") {
		t.Fatal("no lane named:", e)
	}
	a.lane, a.laneTeams = "near", []string{teamID, otherTeamID}
	if r, e := s.Admin(context.Background(), scope, coordinatorCreate("w", otherTeamID, "Engineering")); e != nil || len(p.writes) != 1 || r.TeamID != otherTeamID {
		t.Fatal("the lane's default team:", r, e)
	}
}

// A refusal after the reservation closes it as not sent: the designation
// moving before the send, or Linear already having the name. A failure
// that may have reached Linear leaves it open.
func TestCoordinatorCreateSettlesNotSent(t *testing.T) {
	s, a, p, cr, scope := coordinatorTest(t)
	a.afterAccess = func(n int) {
		if n >= 2 {
			cr.coordinator = false
		}
	}
	if _, e := s.Admin(context.Background(), scope, coordinatorCreate("moved", teamID, "Engineering")); e == nil || len(p.writes) != 0 || len(cr.settles) != 1 || cr.settles[0]["outcome"] != "not_sent" {
		t.Fatal("designation moved:", e, p.writes, cr.settles)
	}
	s, _, p, cr, scope = coordinatorTest(t)
	p.projects["q4"] = []linearprovider.ProjectListing{{Project: linearprovider.Project{ID: projectA, Name: "Q4", TeamIDs: []string{teamID}}}}
	r, e := s.Admin(context.Background(), scope, coordinatorCreate("seen", teamID, "Engineering"))
	if e != nil || r.Outcome != "observed_unchanged" || len(p.writes) != 0 || len(cr.settles) != 1 || cr.settles[0]["outcome"] != "not_sent" || !strings.Contains(r.OwnerNotice, "not created") {
		t.Fatal("an existing name:", r, e, cr.settles)
	}
	s, _, p, cr, scope = coordinatorTest(t)
	p.createErr = errors.New("connection reset")
	if _, e = s.Admin(context.Background(), scope, coordinatorCreate("lost", teamID, "Engineering")); !IsUncertain(e) || len(cr.settles) != 0 {
		t.Fatal("an uncertain create was settled:", e, cr.settles)
	}
	// Its retry reads the uncertain record back and leaves it open too.
	if _, e = s.Admin(context.Background(), scope, coordinatorCreate("lost", teamID, "Engineering")); e == nil || len(cr.settles) != 0 || len(p.writes) != 1 {
		t.Fatal("an uncertain retry was settled:", e, cr.settles, p.writes)
	}
	// Before the reservation's write starts, a failure closes it too.
	s, _, p, cr, scope = coordinatorTest(t)
	p.incomplete = true
	if _, e = s.Admin(context.Background(), scope, coordinatorCreate("walk", teamID, "Engineering")); e == nil || len(cr.settles) != 1 || cr.settles[0]["outcome"] != "not_sent" {
		t.Fatal("an incomplete walk:", e, cr.settles)
	}
}

// The owner's --kind applies the setting's seeded label, of the create's
// team; the owner reserves nothing.
func TestOwnerCreateWithKind(t *testing.T) {
	s, a, p, scope := adminTest(t)
	c := &creates{kinds: map[string]any{"revision": 2, "team_id": teamID, "scope": "team", "group_name": "Type", "group_label_id": dupLabel, "team_bound": true,
		"values": []any{map[string]any{"name": "Engineering", "label_id": kindLabel}, map[string]any{"name": "Review"}}}}
	a.creates = c
	r, e := s.Admin(context.Background(), scope, coordinatorCreate("o-1", teamID, "Engineering"))
	if e != nil || r.KindLabelID != kindLabel || p.writes[0] != "project "+teamID+" Q4 "+kindLabel || len(c.reserves) != 0 || r.Lead != nil {
		t.Fatal(r, e, p.writes)
	}
	for kind, want := range map[string]string{"Review": "isn't seeded", "Nope": "one of the project's kinds"} {
		if _, e := s.Admin(context.Background(), scope, coordinatorCreate("o-"+kind, teamID, kind)); e == nil || !strings.Contains(e.Error(), want) {
			t.Fatal(kind, e)
		}
	}
	if _, e := s.Admin(context.Background(), scope, coordinatorCreate("o-other", otherTeamID, "Engineering")); e == nil || !strings.Contains(e.Error(), "another team") {
		t.Fatal(e)
	}
	// Without a kind, as before.
	if r, e := s.Admin(context.Background(), scope, AdminInput{Kind: "project_create", OperationID: "o-none", TeamID: teamID, Name: "Q5"}); e != nil || r.KindLabelID != "" || p.writes[len(p.writes)-1] != "project "+teamID+" Q5" {
		t.Fatal(r, e, p.writes)
	}
}
