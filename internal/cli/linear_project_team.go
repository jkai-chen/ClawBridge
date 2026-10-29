package cli

import (
	"context"
	"errors"
	"fmt"

	"clawbridge/internal/bus"
	"clawbridge/internal/linearboard"
	"clawbridge/internal/linearprovider"
)

// linearProjectAddTeam is linear project update PROJECT --add-team TEAM
// (FR10): a preview, or with --confirm the add the preview showed, through
// the person's own Linear profile. Only UUIDs are taken: Linear's addTeams
// would resolve a team's name itself.
func linearProjectAddTeam(ctx context.Context, service *linearboard.Service, scope bus.Request, v linearFlags, project string) (any, error) {
	if !linearprovider.ValidID(v.addTeam) {
		return nil, errors.New("linear project update requires --add-team, the bound team's UUID (a team's name is never sent), or plan fields: --priority, --target-date, --start-date, --clear-target-date or --clear-start-date")
	}
	if v.confirm != (v.expectedSHA != "") {
		return nil, errors.New("--confirm goes with --expected-plan-sha256, the plan_sha256 the preview showed")
	}
	if v.confirm && !validLinearOperation(v.operation) {
		return nil, errors.New("--confirm requires --operation-id")
	}
	// Without a session the add is the person's own authority: an agent
	// in its person's shell adds a team from its coordinator session.
	if v.confirm && scope.Session == "" {
		if marker := agentMarker(closesEnv()); marker != "" {
			return nil, fmt.Errorf("adding a team to a Linear project without a session is a person's, and %s says an agent runs this command; an agent adds one from the project's coordinator session", marker)
		}
	}
	return service.AddProjectTeam(ctx, scope, linearboard.ProjectTeamAddInput{ProjectID: project, TeamID: v.addTeam, OperationID: v.operation, Confirm: v.confirm, ExpectedPlanSHA256: v.expectedSHA})
}
