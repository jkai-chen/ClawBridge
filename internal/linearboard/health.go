package linearboard

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"

	"clawbridge/internal/bus"
)

// HealthV2 reads the project's health rows: each bound Linear project's
// policy, its last confirmed and pending health, and its last run's report.
func (s *Service) HealthV2(ctx context.Context, scope bus.Request) (any, error) {
	scope.Operation = "health_get"
	scope.Args = instanceArgs(scope.Args)
	r, e := s.cfg.Executor.Execute(ctx, scope)
	return r.Data, e
}

// SetHealthV2 sets one bound Linear project's health policy from the
// owner's file, at expected, the row's revision the owner last read (0 for
// a new row).
func (s *Service) SetHealthV2(ctx context.Context, scope bus.Request, linearProject string, raw []byte, expected int64) (any, error) {
	if e := uniqueKeys(raw); e != nil {
		return nil, e
	}
	var policy map[string]any
	dec := json.NewDecoder(bytes.NewReader(raw))
	if dec.Decode(&policy) != nil || dec.Decode(new(any)) != io.EOF || policy == nil {
		return nil, errors.New(`the health policy file must be a JSON object such as {"enabled": true}`)
	}
	return s.setHealth(ctx, scope, linearProject, policy, expected)
}

// EnableHealthV2 turns one bound Linear project's health on or off at
// expected, keeping the rest of its policy (the defaults for a new row).
func (s *Service) EnableHealthV2(ctx context.Context, scope bus.Request, linearProject string, enabled bool, expected int64) (any, error) {
	policy := map[string]any{}
	if expected > 0 {
		current, e := s.HealthV2(ctx, scope)
		if e != nil {
			return nil, e
		}
		raw, _ := json.Marshal(current)
		var view struct {
			Projects []struct {
				LinearProjectID string         `json:"linear_project_id"`
				Policy          map[string]any `json:"policy"`
			} `json:"projects"`
		}
		_ = json.Unmarshal(raw, &view)
		for _, p := range view.Projects {
			if strings.EqualFold(p.LinearProjectID, linearProject) && p.Policy != nil {
				policy = p.Policy
			}
		}
	}
	policy["enabled"] = enabled
	return s.setHealth(ctx, scope, linearProject, policy, expected)
}

func (s *Service) setHealth(ctx context.Context, scope bus.Request, linearProject string, policy map[string]any, expected int64) (any, error) {
	scope.Operation = "health_set"
	scope.Args = instanceArgs(scope.Args)
	scope.Args["linear_project"] = strings.ToLower(linearProject)
	scope.Args["expected_revision"] = expected
	scope.Args["health_policy"] = policy
	r, e := s.cfg.Executor.Execute(ctx, scope)
	return r.Data, e
}
