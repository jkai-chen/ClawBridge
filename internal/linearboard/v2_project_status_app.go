package linearboard

import (
	"context"
	"errors"
	"strings"

	"clawbridge/internal/bus"
)

// ProjectStatusAppV2 reads (set false) or sets a bound Linear project's
// status through the hosted server's ClawBridge app
// (linear_app_project_status[_set]): the server reads Linear, maps the type
// and sends the move. A local authority has no app: capability_unavailable.
func (s *Service) ProjectStatusAppV2(ctx context.Context, scope bus.Request, set bool, args map[string]any) (any, error) {
	scope.Operation = "linear_app_project_status"
	if set {
		scope.Operation += "_set"
	}
	scope.Args = instanceArgs(scope.Args)
	for k, v := range args {
		scope.Args[k] = v
	}
	r, e := s.cfg.Executor.Execute(ctx, scope)
	if e != nil {
		if strings.Contains(e.Error(), `unsupported operation "linear_app_project_status`) {
			return nil, errors.New("capability_unavailable: a Linear project's status is read and set by a hosted server's ClawBridge app; this project's authority is local")
		}
		return nil, e
	}
	return r.Data, nil
}
