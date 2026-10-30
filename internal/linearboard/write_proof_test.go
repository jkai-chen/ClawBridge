package linearboard

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"testing"
	"time"

	"clawbridge/internal/bus"
	"clawbridge/internal/linearprovider"
)

// proofCountV2 counts the binding proof's reads: get_team, list_projects
// (one complete page per team, from the fake's project teams) and
// get_project.
type proofCountV2 struct {
	*providerV2
	teams, lists, projects int
	teamErr, projectErr    error
}

func (p *proofCountV2) GetTeam(ctx context.Context, id string) (linearprovider.Team, error) {
	p.teams++
	if p.teamErr != nil {
		return linearprovider.Team{}, p.teamErr
	}
	return p.providerV2.GetTeam(ctx, id)
}
func (p *proofCountV2) GetProject(ctx context.Context, id string) (linearprovider.Project, error) {
	p.projects++
	if p.projectErr != nil {
		return linearprovider.Project{}, p.projectErr
	}
	return p.providerV2.GetProject(ctx, id)
}
func (p *proofCountV2) TeamProjects(_ context.Context, team string, _ linearprovider.PageOptions) (linearprovider.Page[linearprovider.ProjectListing], error) {
	p.lists++
	rows := []linearprovider.ProjectListing{}
	for id, teams := range p.providerV2.projects {
		if contains(teams, team) {
			rows = append(rows, linearprovider.ProjectListing{Project: linearprovider.Project{ID: id, TeamIDs: teams}})
		}
	}
	return linearprovider.Page[linearprovider.ProjectListing]{Items: rows, Complete: true}, nil
}

// binding is the proof's reads since the last call.
func (p *proofCountV2) binding() (teams, lists, projects int) {
	teams, lists, projects = p.teams, p.lists, p.projects
	p.teams, p.lists, p.projects = 0, 0, 0
	return
}

// writeProofV2 is testV2 with the write team proof kept, an app-like token
// identity and a fake clock.
func writeProofV2(t *testing.T) (*Service, *authorityV2, *proofCountV2, bus.Request, *time.Time, *string) {
	t.Helper()
	s, a, p, scope := testV2(t)
	c := &proofCountV2{providerV2: p}
	s.cfg.Open = func(context.Context, *http.Client, bool) (Provider, error) { return c, nil }
	now := time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)
	s.cfg.Now = func() time.Time { return now }
	token := "app/org/token-1"
	s.cfg.WriteIdentity = func() string { return token }
	s.writeProofs = NewWriteTeamProofs()
	return s, a, c, scope, &now, &token
}

var writeProofOps int

func titleUpdateV2() WriteInputV2 {
	writeProofOps++
	title := "title " + strconv.Itoa(writeProofOps)
	return WriteInputV2{WriteInput: WriteInput{Operation: "update", OperationID: "proof-update-" + strconv.Itoa(writeProofOps), IssueID: issueID, Title: &title}}
}

func createInV2(project string) WriteInputV2 {
	writeProofOps++
	title := "new issue"
	return WriteInputV2{WriteInput: WriteInput{Operation: "create", OperationID: "proof-create-" + strconv.Itoa(writeProofOps), Title: &title}, Project: linearprovider.Field[*string]{Present: true, Value: ptrV2(project)}}
}

func relationInV2() RelationInputV2 {
	writeProofOps++
	return RelationInputV2{OperationID: "proof-relation-" + strconv.Itoa(writeProofOps), Action: "add", Type: "blocks", FromID: issueID, ToID: parentA}
}

// mustFullProof: the write verified the whole binding (2 teams, a
// list_projects page each).
func mustFullProof(t *testing.T, p *proofCountV2, what string) {
	t.Helper()
	if teams, lists, _ := p.binding(); teams != 2 || lists != 2 {
		t.Fatalf("%s: teams=%d lists=%d, want a full binding proof", what, teams, lists)
	}
}

func mustKeptProof(t *testing.T, p *proofCountV2, what string, projects int) {
	t.Helper()
	if teams, lists, got := p.binding(); teams != 0 || lists != 0 || got != projects {
		t.Fatalf("%s: teams=%d lists=%d projects=%d, want the kept team proof and %d get_project", what, teams, lists, got, projects)
	}
}

func TestWriteTeamProofIsReusedFor60Seconds(t *testing.T) {
	s, _, p, scope, now, _ := writeProofV2(t)
	p.issues[parentA] = linearprovider.IssueV2{Issue: linearprovider.Issue{ID: parentA, TeamID: otherTeamID, ProjectID: projectB}, ProjectKnown: true, RelationsKnown: true}
	ctx := context.Background()
	if _, e := s.WriteV2(ctx, scope, titleUpdateV2()); e != nil {
		t.Fatal(e)
	}
	mustFullProof(t, p, "first update")
	*now = now.Add(30 * time.Second)
	reads := p.reads
	if _, e := s.WriteV2(ctx, scope, titleUpdateV2()); e != nil {
		t.Fatal(e)
	}
	// An update's own calls only: its two issue reads.
	mustKeptProof(t, p, "second update", 0)
	if p.reads-reads != 2 {
		t.Fatalf("update issue reads = %d, want 2", p.reads-reads)
	}
	reads = p.reads
	if _, e := s.RelationV2(ctx, scope, relationInV2()); e != nil {
		t.Fatal(e)
	}
	mustKeptProof(t, p, "relation", 0)
	if p.reads-reads != 4 {
		t.Fatalf("relation issue reads = %d, want 4", p.reads-reads)
	}
	// A create naming its project reads that project, once.
	if _, e := s.WriteV2(ctx, scope, createInV2(projectA)); e != nil {
		t.Fatal(e)
	}
	mustKeptProof(t, p, "create", 1)
	if p.writes != 4 {
		t.Fatal("writes", p.writes)
	}
	// 61 s after the proof was made: verified in full again.
	*now = now.Add(31 * time.Second)
	if _, e := s.WriteV2(ctx, scope, titleUpdateV2()); e != nil {
		t.Fatal(e)
	}
	mustFullProof(t, p, "update after 61 s")
}

func TestWriteTeamProofIsDropped(t *testing.T) {
	ctx := context.Background()
	t.Run("failed write", func(t *testing.T) {
		s, _, p, scope, _, _ := writeProofV2(t)
		if _, e := s.WriteV2(ctx, scope, titleUpdateV2()); e != nil {
			t.Fatal(e)
		}
		mustFullProof(t, p, "first")
		p.writeErr = errors.New("save failed")
		if _, e := s.WriteV2(ctx, scope, titleUpdateV2()); e == nil {
			t.Fatal("failed write succeeded")
		}
		mustKeptProof(t, p, "failing write", 0)
		p.writeErr = nil
		if _, e := s.WriteV2(ctx, scope, titleUpdateV2()); e != nil {
			t.Fatal(e)
		}
		mustFullProof(t, p, "after a failed write")
	})
	t.Run("binding revision", func(t *testing.T) {
		s, a, p, scope, _, _ := writeProofV2(t)
		if _, e := s.WriteV2(ctx, scope, titleUpdateV2()); e != nil {
			t.Fatal(e)
		}
		mustFullProof(t, p, "first")
		a.b.Revision++
		if _, e := s.WriteV2(ctx, scope, titleUpdateV2()); e != nil {
			t.Fatal(e)
		}
		mustFullProof(t, p, "after a revision change")
	})
	t.Run("token re-minted", func(t *testing.T) {
		s, _, p, scope, _, token := writeProofV2(t)
		if _, e := s.WriteV2(ctx, scope, titleUpdateV2()); e != nil {
			t.Fatal(e)
		}
		mustFullProof(t, p, "first")
		*token = "app/org/token-2"
		if _, e := s.WriteV2(ctx, scope, titleUpdateV2()); e != nil {
			t.Fatal(e)
		}
		mustFullProof(t, p, "under a new token")
		// No token identity: every write verifies in full.
		*token = ""
		for range 2 {
			if _, e := s.WriteV2(ctx, scope, titleUpdateV2()); e != nil {
				t.Fatal(e)
			}
			mustFullProof(t, p, "without a token identity")
		}
	})
	t.Run("team not found", func(t *testing.T) {
		s, _, p, scope, now, _ := writeProofV2(t)
		p.teamErr = errors.New("team not found")
		if _, e := s.WriteV2(ctx, scope, titleUpdateV2()); e == nil || p.writes != 0 {
			t.Fatal("write without its team proven", e)
		}
		p.binding()
		p.teamErr = nil
		*now = now.Add(time.Second)
		if _, e := s.WriteV2(ctx, scope, titleUpdateV2()); e != nil {
			t.Fatal(e)
		}
		mustFullProof(t, p, "after a team error")
	})
	t.Run("project not found", func(t *testing.T) {
		s, _, p, scope, _, _ := writeProofV2(t)
		if _, e := s.WriteV2(ctx, scope, titleUpdateV2()); e != nil {
			t.Fatal(e)
		}
		mustFullProof(t, p, "first")
		p.projectErr = errors.New("project not found")
		if _, e := s.WriteV2(ctx, scope, createInV2(projectA)); e == nil || p.writes != 1 {
			t.Fatal("create without its project proven", e)
		}
		mustKeptProof(t, p, "create", 1)
		p.projectErr = nil
		if _, e := s.WriteV2(ctx, scope, titleUpdateV2()); e != nil {
			t.Fatal(e)
		}
		mustFullProof(t, p, "after a project error")
	})
}

// A bound project moved to an unbound team inside the window: every write
// to it is refused before dispatch, and the kept proof goes.
func TestWriteTeamProofRefusesAMovedProject(t *testing.T) {
	ctx := context.Background()
	for _, op := range []string{"create", "update", "relation", "update unbound project"} {
		t.Run(op, func(t *testing.T) {
			s, _, p, scope, _, _ := writeProofV2(t)
			p.issues[parentA] = linearprovider.IssueV2{Issue: linearprovider.Issue{ID: parentA, TeamID: otherTeamID, ProjectID: projectB}, ProjectKnown: true, RelationsKnown: true}
			if _, e := s.WriteV2(ctx, scope, titleUpdateV2()); e != nil {
				t.Fatal(e)
			}
			mustFullProof(t, p, "first")
			p.providerV2.projects[projectA] = []string{unboundTeamID}
			moved := p.issues[issueID]
			moved.TeamID = unboundTeamID
			if op == "update unbound project" {
				// Still in a bound team, now in a project the binding lacks.
				moved.TeamID, moved.ProjectID = teamID, unboundProject
			}
			var e error
			switch op {
			case "create":
				_, e = s.WriteV2(ctx, scope, createInV2(projectA))
			case "relation":
				p.issues[issueID] = moved
				_, e = s.RelationV2(ctx, scope, relationInV2())
			default:
				p.issues[issueID] = moved
				_, e = s.WriteV2(ctx, scope, titleUpdateV2())
			}
			var outside outOfScopeError
			if !errors.As(e, &outside) || p.writes != 1 || len(p.edges) != 0 {
				t.Fatalf("write to a moved target: %v, writes=%d", e, p.writes)
			}
			projects := 0
			if op == "create" {
				projects = 1
			}
			mustKeptProof(t, p, "refused write", projects)
			// The refusal dropped the kept proof.
			p.providerV2.projects[projectA] = []string{teamID}
			p.issues[parentA] = linearprovider.IssueV2{Issue: linearprovider.Issue{ID: parentA, TeamID: otherTeamID, ProjectID: projectB}, ProjectKnown: true}
			if _, e = s.WriteV2(ctx, scope, WriteInputV2{WriteInput: WriteInput{Operation: "update", OperationID: "after-" + strconv.Itoa(len(op)), IssueID: parentA, Title: ptrV2("t")}}); e != nil {
				t.Fatal(e)
			}
			if teams, _, _ := p.binding(); teams != 2 {
				t.Fatal("kept proof survived a refused target", teams)
			}
		})
	}
}

// An issue moved to an unbound team between the pre-lock read and the
// in-lock re-read is refused by the re-read.
func TestWriteTeamProofInLockReadCatchesAMove(t *testing.T) {
	ctx := context.Background()
	for _, op := range []string{"update", "relation", "relation target"} {
		t.Run(op, func(t *testing.T) {
			s, a, p, scope, _, _ := writeProofV2(t)
			p.issues[parentA] = linearprovider.IssueV2{Issue: linearprovider.Issue{ID: parentA, TeamID: otherTeamID, ProjectID: projectB}, ProjectKnown: true, RelationsKnown: true}
			if _, e := s.WriteV2(ctx, scope, titleUpdateV2()); e != nil {
				t.Fatal(e)
			}
			mustFullProof(t, p, "first")
			// The move lands just after the second write's first in-lock
			// access check, before its issue re-read.
			start := a.accesses
			a.afterAccess = func(n int) {
				if n == start+2 {
					// The relation's target end moves in its own test.
					id := issueID
					if op == "relation target" {
						id = parentA
					}
					i := p.issues[id]
					i.TeamID = unboundTeamID
					p.issues[id] = i
				}
			}
			var e error
			if op == "update" {
				_, e = s.WriteV2(ctx, scope, titleUpdateV2())
			} else {
				_, e = s.RelationV2(ctx, scope, relationInV2())
			}
			if e == nil || p.writes != 1 || len(p.edges) != 0 {
				t.Fatalf("a move before dispatch was sent: %v", e)
			}
			mustKeptProof(t, p, "moved write", 0)
		})
	}
}

// A team board's pairs are its teams: a kept proof is the whole proof, and
// a create reads nothing more.
func TestWriteTeamProofOnATeamBoard(t *testing.T) {
	s, a, p, scope, _, _ := writeProofV2(t)
	a.b.Mode, a.b.LinearProjectIDs = "team_board", nil
	ctx := context.Background()
	title := "board issue"
	for n := range 2 {
		// An empty label set keeps the create on the V2 path.
		in := WriteInputV2{WriteInput: WriteInput{Operation: "create", OperationID: "board-create-" + strconv.Itoa(n), Title: &title}, Labels: linearprovider.Field[[]string]{Present: true, Value: []string{}}}
		if _, e := s.WriteV2(ctx, scope, in); e != nil {
			t.Fatal(e)
		}
		teams, lists, projects := p.binding()
		if (n == 0 && teams != 2) || (n == 1 && teams != 0) || lists != 0 || projects != 0 {
			t.Fatalf("create %d: teams=%d lists=%d projects=%d", n, teams, lists, projects)
		}
	}
}
