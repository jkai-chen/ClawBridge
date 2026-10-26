package linearboard

import (
	"context"
	"errors"
	"slices"
	"strings"

	"clawbridge/internal/bus"
	"clawbridge/internal/linearprovider"
)

// projectStatusWriter is a provider that can move a project's status;
// *linearprovider.Provider is one.
type projectStatusWriter interface {
	SetProjectStatus(ctx context.Context, project, status string) error
}

// SetProjectStatusV2 moves one bound Linear project to a project status by
// ID (A4). It checks, before anything is sent, that the project is in the
// project's project-set binding. An error refused before dispatch is a
// refusedBeforeDispatchError; StatusUpdateOutcome tells refusals apart from
// uncertain sends the same way as for status updates.
func (s *Service) SetProjectStatusV2(ctx context.Context, scope bus.Request, linearProject, status string) error {
	linearProject, status = strings.ToLower(linearProject), strings.ToLower(status)
	if !linearprovider.ValidID(linearProject) || !linearprovider.ValidID(status) {
		return refusedBeforeDispatch(errors.New("invalid project status move"))
	}
	b, e := s.BindingV2(ctx, scope)
	if e != nil {
		return refusedBeforeDispatch(e)
	}
	if !b.Configured || b.Mode != "project_set" || !slices.Contains(b.LinearProjectIDs, linearProject) {
		return refusedBeforeDispatch(outOfScopeError("that Linear project is not in this project's binding"))
	}
	_, p, release, e := s.openForWrite(ctx)
	if e != nil {
		return refusedBeforeDispatch(e)
	}
	defer release()
	w, ok := p.(projectStatusWriter)
	if !ok {
		return refusedBeforeDispatch(&linearprovider.Error{Kind: "capability_unavailable", NotSent: true})
	}
	return w.SetProjectStatus(ctx, linearProject, status)
}
