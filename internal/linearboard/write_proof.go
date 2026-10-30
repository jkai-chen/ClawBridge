package linearboard

import (
	"context"
	"errors"
	"slices"
	"sync"
	"time"

	"clawbridge/internal/bus"
	"clawbridge/internal/linearconn"
	"clawbridge/internal/linearprovider"
)

// maxWriteTeamProofs bounds the bindings whose team proof is kept; past it,
// the cache starts again empty.
const maxWriteTeamProofs = 256

// WriteTeamProofs keeps the team half of issue writes' binding proofs for
// BindingProofTTL: the get_team of every listed team, with its parent. The
// project half is never kept; each write checks its own target instead
// (proveWriteBinding). An entry is keyed by the binding's digest and
// revision, the Linear organisation and the token's identity, and any
// failed write on the binding drops it. It is safe for concurrent use.
type WriteTeamProofs struct {
	ttl time.Duration
	mu  sync.Mutex
	m   map[string]teamProof
}

type teamProof struct {
	at    time.Time
	teams []linearprovider.Team
}

// NewWriteTeamProofs returns an empty cache with the binding proof's TTL.
func NewWriteTeamProofs() *WriteTeamProofs {
	return &WriteTeamProofs{ttl: BindingProofTTL, m: map[string]teamProof{}}
}

// CacheWriteTeamProof lets this Service's issue writes reuse the team half
// of their binding proof (WriteTeamProofs). Only a long-lived process
// should call it, before its first command.
func (s *Service) CacheWriteTeamProof() {
	if s.writeProofs == nil {
		s.writeProofs = NewWriteTeamProofs()
	}
}

func (c *WriteTeamProofs) get(key string, now time.Time) ([]linearprovider.Team, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	entry, ok := c.m[key]
	age := now.Sub(entry.at)
	if !ok || age < 0 || age >= c.ttl {
		return nil, false
	}
	return slices.Clone(entry.teams), true
}

func (c *WriteTeamProofs) put(key string, now time.Time, teams []linearprovider.Team) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for k, entry := range c.m {
		if age := now.Sub(entry.at); age < 0 || age >= c.ttl {
			delete(c.m, k)
		}
	}
	if len(c.m) >= maxWriteTeamProofs {
		c.m = map[string]teamProof{}
	}
	c.m[key] = teamProof{at: now, teams: slices.Clone(teams)}
}

func (c *WriteTeamProofs) drop(key string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.m, key)
}

// writeProofKey is the identity a kept team proof belongs to, "" when
// writes must verify in full: the binding (its digest covers the revision,
// mode, teams and projects), the Linear organisation, and the token the
// write runs with. The app's token identity changes on every re-mint; a
// personal profile has no token generation to show, so its account stands
// in for it.
func (s *Service) writeProofKey(b bus.TaskboardBindingV2, profile linearconn.Profile) string {
	token := "profile:" + profile.ID + "/" + profile.AccountLabel
	if s.cfg.WriteIdentity != nil {
		if token = s.cfg.WriteIdentity(); token == "" {
			return ""
		}
	}
	return digest([]any{"write-team-proof", s.cfg.ProfileID, s.cfg.Workspace, profile.WorkspaceID, token, b.WorkspaceID, b.ProjectID, b.Revision, digest(b)})
}

// proveWriteBinding is the binding proof of one issue write, and the
// function that drops a kept team proof when the write fails. With a team
// proof kept for this binding and token, it reads nothing: the proof it
// returns carries the kept teams and no project-set pairs (Targets), so
// the write admits only its own target, read from Linear now
// (admitIssueV2, admitProjectV2). Otherwise it verifies the whole binding
// and keeps its team half. A held operation keeps its own proof.
func (s *Service) proveWriteBinding(ctx context.Context, p Provider, b bus.TaskboardBindingV2, profile linearconn.Profile) (verifiedV2, func(), error) {
	c := s.writeProofs
	if c == nil || s.held != nil {
		v, e := s.proveBinding(ctx, p, b)
		return v, func() {}, e
	}
	key := s.writeProofKey(b, profile)
	if key == "" {
		v, e := verifyBindingV2(ctx, p, b)
		return v, func() {}, e
	}
	drop := func() { c.drop(key) }
	now := s.cfg.Now()
	if teams, ok := c.get(key, now); ok {
		v := verifiedV2{Teams: teams}
		if b.Mode == "team_board" {
			// A team board's pairs are its teams: the team half is all of it.
			for _, id := range readableTeamsV2(b, teams) {
				v.Pairs = append(v.Pairs, pairV2{TeamID: id})
			}
		} else {
			v.Targets = true
		}
		return v, drop, nil
	}
	v, e := verifyBindingV2(ctx, p, b)
	if e != nil {
		drop()
		return v, drop, e
	}
	c.put(key, now, v.Teams)
	return v, drop, nil
}

// admitIssueV2 admits, on a proof without its project half, the pair of an
// issue this write has just read from Linear: an issue in a bound project
// and a readable team shows that the project carries the team. Any other
// issue adds nothing, so the scope check refuses it.
func admitIssueV2(v verifiedV2, b bus.TaskboardBindingV2, issue linearprovider.IssueV2) verifiedV2 {
	if !v.Targets || b.Mode != "project_set" || !issue.ProjectKnown || issue.ProjectID == "" {
		return v
	}
	if !contains(b.LinearProjectIDs, issue.ProjectID) || !b.TeamReadable(issue.TeamID) {
		return v
	}
	return withPairV2(v, pairV2{ProjectID: issue.ProjectID, TeamID: issue.TeamID})
}

// admitProjectV2 admits, on a proof without its project half, the pairs of
// a write's destination project that no issue read has shown: one
// get_project, whose teams must include the target team. A project outside
// the binding, or one Linear now reports outside the target team, adds
// nothing, so the scope check refuses the write before anything is sent.
func admitProjectV2(ctx context.Context, p Provider, v verifiedV2, b bus.TaskboardBindingV2, team, project string) (verifiedV2, error) {
	if !v.Targets || b.Mode != "project_set" || project == "" || !contains(b.LinearProjectIDs, project) {
		return v, nil
	}
	if slices.Contains(v.Pairs, pairV2{ProjectID: project, TeamID: team}) {
		return v, nil
	}
	found, e := p.GetProject(ctx, project)
	if e != nil {
		return v, e
	}
	if found.ID != project {
		return v, errors.New("Linear project identity changed")
	}
	for _, team := range readableTeamsV2(b, v.Teams) {
		if contains(found.TeamIDs, team) {
			v = withPairV2(v, pairV2{ProjectID: project, TeamID: team})
		}
	}
	return v, nil
}

func withPairV2(v verifiedV2, pair pairV2) verifiedV2 {
	if slices.Contains(v.Pairs, pair) {
		return v
	}
	v.Pairs = append(slices.Clone(v.Pairs), pair)
	return v
}
