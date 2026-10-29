package linearboard

import (
	"context"

	"clawbridge/internal/bus"
)

// TeamWithoutProject is a write team the binding lists without a project,
// as Linear shows it now: Inert while no bound project carries it, else a
// stale entry the next link drops.
type TeamWithoutProject struct {
	TeamID string `json:"team_id"`
	Name   string `json:"name,omitempty"`
	Inert  bool   `json:"inert"`
}

// TeamsWithoutProjectV2 reads the binding's teams without a project against
// Linear, for doctor and the TUI. It reads only, and reads nothing from
// Linear when the binding lists none.
func (s *Service) TeamsWithoutProjectV2(ctx context.Context, scope bus.Request) ([]TeamWithoutProject, error) {
	b, e := s.BindingV2(ctx, scope)
	if e != nil || len(b.TeamsWithoutProject) == 0 {
		return nil, e
	}
	_, p, done, e := s.open(ctx)
	if e != nil {
		return nil, e
	}
	defer done()
	v, e := s.verifyReadBindingV2(ctx, p, b)
	if e != nil {
		return nil, e
	}
	out := []TeamWithoutProject{}
	for _, id := range b.TeamsWithoutProject {
		t := TeamWithoutProject{TeamID: id, Inert: InertTeamV2(b, v, id)}
		for _, team := range v.Teams {
			if team.ID == id {
				t.Name = team.Name
			}
		}
		out = append(out, t)
	}
	return out, nil
}

// Label is the team's state in doctor's and the TUI's words.
func (t TeamWithoutProject) Label() string {
	name := t.Name
	if name == "" {
		name = t.TeamID
	}
	if t.Inert {
		return name + ": no project yet (inert)"
	}
	return name + ": a bound project carries it (stale entry; the next link drops it)"
}
