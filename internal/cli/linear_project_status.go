package cli

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"clawbridge/internal/bus"
	"clawbridge/internal/linearboard"
	"clawbridge/internal/linearprovider"
)

// linearProjectStatus reads (linear project status LINEAR_PROJECT) or sets
// (linear project status set LINEAR_PROJECT --type T --operation-id ID) a
// bound Linear project's status through the server's ClawBridge app. A set
// without --confirm is a preview, which prints the confirm to run.
func linearProjectStatus(ctx context.Context, service *linearboard.Service, scope bus.Request, flags Config, v linearFlags, lp string) (any, error) {
	if !v.statusSet {
		for _, k := range []string{"type", "operation-id", "confirm", "expected-status", "status-id", "allow-open"} {
			if v.supplied[k] {
				return nil, fmt.Errorf("--%s belongs to linear project status set", k)
			}
		}
		data, err := service.ProjectStatusAppV2(ctx, scope, false, map[string]any{"linear_project": lp, "suggest": v.suggest})
		return terminalSafeAll(data), err
	}
	if v.suggest {
		return nil, errors.New("--suggest belongs to linear project status (the read)")
	}
	if !bus.ProjectStatusTypes[v.statusType] || !validLinearOperation(v.operation) {
		return nil, errors.New("project status set requires --type backlog|planned|started|completed|canceled and --operation-id")
	}
	if v.confirm != (v.expectedStatus != "") || (v.allowOpen && !v.confirm) {
		return nil, errors.New("--confirm goes with --expected-status, the current status ID the preview showed (and --allow-open only with --confirm)")
	}
	for _, id := range []string{v.expectedStatus, v.statusID} {
		if id != "" && !linearprovider.ValidID(id) {
			return nil, errors.New("--expected-status and --status-id take a Linear status UUID")
		}
	}
	// Without a session the set is the person's own authority: an agent
	// in its person's shell sets a status from its coordinator session.
	if scope.Session == "" {
		if marker := agentMarker(closesEnv()); marker != "" {
			return nil, fmt.Errorf("setting a Linear project's status without a session is a person's, and %s says an agent runs this command; an agent sets it from the project's coordinator session", marker)
		}
	}
	args := map[string]any{"linear_project": lp, "type": v.statusType, "operation_id": v.operation}
	if v.statusID != "" {
		args["status_id"] = strings.ToLower(v.statusID)
	}
	if v.confirm {
		args["confirm"], args["expected_status_id"] = true, strings.ToLower(v.expectedStatus)
		if v.allowOpen {
			args["allow_open"] = true
		}
	}
	data, err := service.ProjectStatusAppV2(ctx, scope, true, args)
	if err != nil {
		return nil, err
	}
	out, _ := terminalSafeAll(data).(map[string]any)
	if expected, _ := out["expected_status_id"].(string); out["preview"] == true && out["refused"] == nil && linearprovider.ValidID(expected) {
		next := fmt.Sprintf("clawbridge linear project status set %s --type %s --operation-id %s --confirm --expected-status %s", lp, v.statusType, v.operation, expected)
		if v.statusID != "" {
			next += " --status-id " + strings.ToLower(v.statusID)
		}
		if out["needs_allow_open"] == true {
			next += " --allow-open"
		}
		out["next"] = next
	}
	return out, nil
}
