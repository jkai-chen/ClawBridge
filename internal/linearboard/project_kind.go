package linearboard

import (
	"context"
	"errors"
	"strings"

	"clawbridge/internal/bus"
)

// ProjectKind reads the project kind setting (PROJECT-KIND-LABELS-PLAN P1):
// any member or session.
func (s *Service) ProjectKind(ctx context.Context, scope bus.Request) (any, error) {
	scope.Operation = "project_kind_get"
	scope.Args = instanceArgs(scope.Args)
	r, e := s.cfg.Executor.Execute(ctx, scope)
	return r.Data, e
}

// ProjectKindInput is one change of the setting at the revision the owner
// last read.
type ProjectKindInput struct {
	Team, Group string
	Values      []string
	Expected    int64
}

// SetProjectKind sets the group and its values: the project owner, in
// their own terminal, without a session. Nothing is sent to Linear.
func (s *Service) SetProjectKind(ctx context.Context, scope bus.Request, in ProjectKindInput) (any, error) {
	if scope.Session != "" {
		return nil, errors.New("a session can't set the project kind; the project owner runs linear kind set in their own terminal")
	}
	if in.Expected < 0 {
		return nil, errors.New("kind set requires --expected-revision (0 for none)")
	}
	values := []string{}
	for _, v := range in.Values {
		if v = strings.TrimSpace(v); v != "" {
			values = append(values, v)
		}
	}
	args := map[string]any{"expected_revision": in.Expected, "kinds": values}
	if in.Team != "" {
		args["team_id"] = in.Team
	}
	if in.Group != "" {
		args["group_name"] = in.Group
	}
	scope.Operation, scope.Args = "project_kind_set", args
	r, e := s.cfg.Executor.Execute(ctx, scope)
	return r.Data, e
}
