package linearboard

import (
	"context"
	"errors"
	"strings"

	"clawbridge/internal/bus"
	"clawbridge/internal/linearprovider"
)

// IssueSubscribers reads the project's issue subscribers setting
// (live/ISSUE-SUBSCRIBERS-PLAN.md).
func (s *Service) IssueSubscribers(ctx context.Context, scope bus.Request) (map[string]any, error) {
	scope.Operation = "issue_subscribers_get"
	scope.Args = instanceArgs(scope.Args)
	r, e := s.cfg.Executor.Execute(ctx, scope)
	if e != nil {
		return nil, e
	}
	data, _ := r.Data.(map[string]any)
	return data, nil
}

// ChangeIssueSubscribers makes one change to the setting at the revision it
// reads first: on, off, owner on|off, add ID or remove ID.
func (s *Service) ChangeIssueSubscribers(ctx context.Context, scope bus.Request, action, value string) (map[string]any, error) {
	current, err := s.IssueSubscribers(ctx, scope)
	if err != nil {
		return nil, err
	}
	args := map[string]any{"expected_revision": current["revision"]}
	ids := []string{}
	switch list := current["user_ids"].(type) {
	case []string:
		ids = append(ids, list...)
	case []any:
		for _, v := range list {
			if id, ok := v.(string); ok {
				ids = append(ids, id)
			}
		}
	}
	switch action {
	case "on", "off":
		if value != "" {
			return nil, errors.New("subscribers " + action + " takes no argument")
		}
		args["enabled"] = action == "on"
	case "owner":
		if value != "on" && value != "off" {
			return nil, errors.New("subscribers owner takes on or off")
		}
		args["owner"] = value == "on"
	case "add", "remove":
		id := strings.ToLower(value)
		if !linearprovider.ValidID(id) {
			return nil, errors.New("subscribers " + action + " takes a Linear user UUID (linear members lists them)")
		}
		kept := []string{}
		for _, existing := range ids {
			if existing != id {
				kept = append(kept, existing)
			}
		}
		if action == "add" {
			if len(kept) != len(ids) {
				return current, nil
			}
			kept = append(kept, id)
		} else if len(kept) == len(ids) {
			return nil, errors.New("that Linear user isn't on the list")
		}
		args["user_ids"] = kept
	default:
		return nil, errors.New("use linear subscribers show|on|off|owner on|off|add ID|remove ID|backfill")
	}
	scope.Operation = "issue_subscribers_set"
	scope.Args = instanceArgs(scope.Args)
	for k, v := range args {
		scope.Args[k] = v
	}
	r, e := s.cfg.Executor.Execute(ctx, scope)
	if e != nil {
		return nil, e
	}
	data, _ := r.Data.(map[string]any)
	return data, nil
}

// SubscribersBackfill asks the hosted server to find the issues the
// project's agents created or claimed since a time, and with apply to queue
// them for subscribing (§10).
func (s *Service) SubscribersBackfill(ctx context.Context, scope bus.Request, since string, apply bool) (any, error) {
	scope.Operation = "linear_app_subscribers_backfill"
	scope.Args = instanceArgs(scope.Args)
	scope.Args["since"] = since
	if apply {
		scope.Args["apply"] = true
	}
	r, e := s.cfg.Executor.Execute(ctx, scope)
	if e != nil {
		if strings.Contains(e.Error(), `unsupported operation "linear_app_subscribers_backfill`) {
			return nil, errors.New("capability_unavailable: a backfill runs on a hosted server's ClawBridge app; this project's authority is local")
		}
		return nil, e
	}
	return r.Data, nil
}
