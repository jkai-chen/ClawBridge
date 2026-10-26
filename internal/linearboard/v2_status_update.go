package linearboard

import (
	"context"
	"errors"
	"slices"
	"strings"

	"clawbridge/internal/bus"
	"clawbridge/internal/linearprovider"
)

// statusUpdateWriter is a provider that can create project status updates;
// *linearprovider.Provider is one.
type statusUpdateWriter interface {
	SaveStatusUpdate(ctx context.Context, project, body, health string) (string, error)
}

var errInvalidStatusUpdate = errors.New("invalid status update")

// healthValues are the healths a status update may carry, as Linear names
// them.
var healthValues = []string{"onTrack", "atRisk", "offTrack"}

// PostStatusUpdateV2 creates one project status update, the only status
// update write there is: never an edit or a delete. It checks, before
// anything is sent, the mention guard over the body, the health, and that
// the Linear project is in the project's project-set binding. It returns
// the update's ID, or "" with no error when Linear answered without a
// readable ID. An error refused before dispatch is a
// refusedBeforeDispatchError (StatusUpdateOutcome tells them apart).
func (s *Service) PostStatusUpdateV2(ctx context.Context, scope bus.Request, linearProject, body, health string) (string, error) {
	linearProject = strings.ToLower(linearProject)
	if e := mentionGuard(&body); e != nil {
		return "", refusedBeforeDispatch(e)
	}
	if !slices.Contains(healthValues, health) || !linearprovider.ValidID(linearProject) || !validText(body, linearprovider.MaxStatusUpdateBody, false) {
		return "", refusedBeforeDispatch(errInvalidStatusUpdate)
	}
	b, e := s.BindingV2(ctx, scope)
	if e != nil {
		return "", refusedBeforeDispatch(e)
	}
	if !b.Configured || b.Mode != "project_set" || !slices.Contains(b.LinearProjectIDs, linearProject) {
		return "", refusedBeforeDispatch(outOfScopeError("that Linear project is not in this project's binding"))
	}
	_, p, release, e := s.openForWrite(ctx)
	if e != nil {
		return "", refusedBeforeDispatch(e)
	}
	defer release()
	w, ok := p.(statusUpdateWriter)
	if !ok {
		return "", refusedBeforeDispatch(errors.New("this Linear connection cannot post status updates"))
	}
	return w.SaveStatusUpdate(ctx, linearProject, body, health)
}

// StatusUpdateOutcome classifies a status update send for its ledger:
// "accepted" with an ID; "refused" when nothing reached Linear or Linear
// answered that it made nothing; "uncertain" otherwise. code is a short
// failure code, never Linear text.
func StatusUpdateOutcome(id string, err error) (outcome, code string) {
	var refused refusedBeforeDispatchError
	var provider *linearprovider.Error
	switch {
	case err == nil && linearprovider.ValidID(id):
		return "accepted", ""
	case err == nil:
		return "uncertain", "receipt_unreadable"
	case errors.As(err, &refused):
		var outside outOfScopeError
		if errors.As(err, &outside) {
			return "refused", "out_of_binding"
		}
		if errors.Is(err, errMention) {
			return "refused", "mention"
		}
		if errors.Is(err, errInvalidStatusUpdate) {
			return "refused", "invalid"
		}
		if errors.As(err, &provider) {
			return "refused", provider.Kind
		}
		return "refused", "refused"
	case errors.As(err, &provider):
		// Never sent, checked before sending, or a tool error: Linear's
		// tool answered that it made nothing.
		if provider.NotSent || provider.Kind == "invalid" || provider.Kind == "capability_unavailable" || provider.Kind == "provider_rejected" {
			return "refused", provider.Kind
		}
		return "uncertain", provider.Kind
	}
	return "uncertain", "failed"
}
