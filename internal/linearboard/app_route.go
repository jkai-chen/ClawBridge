package linearboard

import (
	"context"
	"errors"
	"net/http"
	"sync"

	"clawbridge/internal/bus"
	"clawbridge/internal/linearprovider"
)

// rejection is a hosted server's answer that refused a request
// (remote.HTTPError).
type rejection interface {
	Rejection() (status int, code, message string)
}

// appRoute is what a hosted server said about writing to Linear as the
// ClawBridge app, asked once per service.
type appRoute struct {
	mu     sync.Mutex
	known  bool
	on     bool
	userID string
}

// appWrites reports whether this service's agent writes go to the hosted
// server as linear_write. A server that offers the app always gets them:
// there is no fallback to the member's own Linear profile, so a failed
// app write is reported, never retried as the member. A server without the
// app, or older than v0.2.8, refuses linear_app_status as invalid; then
// writes stay with the member's profile. Any other answer is no answer, and
// nothing is sent.
func (s *Service) appWrites(ctx context.Context) (bool, error) {
	if !s.cfg.AppRoute {
		return false, nil
	}
	s.app.mu.Lock()
	defer s.app.mu.Unlock()
	if s.app.known {
		return s.app.on, nil
	}
	result, err := s.cfg.Executor.Execute(ctx, bus.Request{Operation: "linear_app_status", Args: map[string]any{}})
	var refused rejection
	status := 0
	if errors.As(err, &refused) {
		status, _, _ = refused.Rejection()
	}
	switch {
	case status == http.StatusBadRequest:
		s.app.known = true
		return false, nil
	case err != nil:
		return false, errors.New("cannot tell whether the ClawBridge server writes to Linear as the ClawBridge app; nothing was sent, retry")
	}
	data, _ := result.Data.(map[string]any)
	user, _ := data["app_user_id"].(string)
	if data["configured"] != true || !linearprovider.ValidID(user) {
		return false, errors.New("the ClawBridge server's Linear app status is invalid; nothing was sent")
	}
	s.app.known, s.app.on, s.app.userID = true, true, user
	return true, nil
}

// appUserID is the ClawBridge app's Linear user when this service's writes
// go through it, else "".
func (s *Service) appUserID(ctx context.Context) string {
	if on, err := s.appWrites(ctx); err != nil || !on {
		return ""
	}
	s.app.mu.Lock()
	defer s.app.mu.Unlock()
	return s.app.userID
}

// appWrite sends one agent write to the hosted server, which makes it as
// the ClawBridge app with every check, the marker and its own shared
// operation ledger. Its answers keep the types a local write returns.
func (s *Service) appWrite(ctx context.Context, scope bus.Request, operation string, args map[string]any) (any, error) {
	instance, _ := scope.Args["instance"].(string)
	request := bus.Request{Operation: "linear_write", Workspace: scope.Workspace, Project: scope.Project, Session: scope.Session, Args: map[string]any{"instance": instance, "operation": operation, "args": args}}
	result, err := s.cfg.Executor.Execute(ctx, request)
	var rejected rejection
	if errors.As(err, &rejected) {
		status, code, message := rejected.Rejection()
		switch {
		case status == http.StatusConflict && code == "in_progress":
			if message == "" {
				message = "in_progress: this operation is being sent to Linear by another request; retry the same operation_id shortly"
			}
			return nil, InProgressError{Message: message}
		case status == http.StatusConflict && code == "conflict":
			if message == "" {
				message = "Linear operation may have been dispatched; outcome uncertain, inspect Linear before retrying"
			}
			return nil, UncertainError{Message: message}
		case status < 500:
			// Refused before anything reached Linear.
			return nil, err
		}
	}
	if err != nil {
		return nil, UncertainError{Message: "the ClawBridge server's Linear write may have been sent; outcome uncertain, retry the same operation_id to read the recorded outcome"}
	}
	data, _ := result.Data.(map[string]any)
	receipt, ok := data["receipt"]
	if !ok || receipt == nil {
		return nil, UncertainError{Message: "the ClawBridge server answered a Linear write without a receipt; outcome uncertain, retry the same operation_id"}
	}
	if outcome, _ := data["outcome"].(string); outcome != "" {
		return receipt, &ReceiptError{Outcome: outcome}
	}
	return receipt, nil
}

// writeArgsV2 is the linear_write argument map for a create, update or
// comment: what parseWriteArgsV2 reads back into the same input.
func writeArgsV2(in WriteInputV2) map[string]any {
	args := map[string]any{}
	for key, value := range map[string]string{"operation_id": in.OperationID, "id": in.IssueID, "body": in.Body, "team_id": in.TeamID, "in_response_to": in.InResponseTo} {
		if value != "" {
			args[key] = value
		}
	}
	for key, value := range map[string]*string{"title": in.Title, "description": in.Description, "state_id": in.StateID, "expected_title": in.ExpectedTitle, "expected_description_sha256": in.ExpectedDescriptionSHA256} {
		if value != nil {
			args[key] = *value
		}
	}
	for key, field := range map[string]linearprovider.Field[*string]{"project_id": in.Project, "parent_id": in.Parent, "assignee_id": in.Assignee} {
		if field.Present {
			if field.Value == nil {
				args[key] = nil
			} else {
				args[key] = *field.Value
			}
		}
	}
	for key, field := range map[string]linearprovider.Field[[]string]{"label_ids": in.Labels, "expected_label_ids": in.ExpectedLabels} {
		if field.Present {
			args[key] = field.Value
		}
	}
	for key, ids := range map[string][]string{"add_label_ids": in.AddLabels, "remove_label_ids": in.RemoveLabels} {
		if ids != nil {
			args[key] = ids
		}
	}
	return args
}
