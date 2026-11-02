package cli

import (
	"context"
	"errors"
	"fmt"

	"clawbridge/internal/bus"
	"clawbridge/internal/linearboard"
	"clawbridge/internal/linearprovider"
)

// setMilestonePlan is the board's milestone plan call; tests replace it.
var setMilestonePlan = (*linearboard.Service).SetMilestonePlan

// linearMilestonePlan is linear milestone create PROJECT --name N
// [--target-date D] and linear milestone update MILESTONE --linear-project PROJECT
// [--name N] [--target-date D | --clear-target-date] (FR55), each with
// --operation-id ID: a preview, or with --confirm the plan the preview
// showed, through the person's own Linear profile. action is "create" or
// "update"; id is the project (create) or the milestone (update).
func linearMilestonePlan(ctx context.Context, service *linearboard.Service, scope bus.Request, v linearFlags, action, id string) (any, error) {
	if !validLinearOperation(v.operation) {
		return nil, errors.New("linear milestone " + action + " requires --operation-id; the preview and its confirm use the same one")
	}
	if v.confirm != (v.expectedSHA != "") {
		return nil, errors.New("--confirm goes with --expected-plan-sha256, the plan_sha256 the preview showed")
	}
	// Without a session the plan is the person's own authority: an agent in
	// its person's shell sets one from its session.
	if v.confirm && scope.Session == "" {
		if marker := agentMarker(closesEnv()); marker != "" {
			return nil, fmt.Errorf("changing a project's milestones without a session is a person's, and %s says an agent runs this command; an agent confirms from its session, or previews and asks the project owner or a lead", marker)
		}
	}
	in := linearboard.MilestonePlanInput{ProjectID: id, OperationID: v.operation, Confirm: v.confirm, ExpectedPlanSHA256: v.expectedSHA}
	if action == "update" {
		in.ProjectID, in.MilestoneID = v.project, id
	}
	if v.supplied["name"] {
		name := v.name
		in.Name = &name
	}
	switch {
	case v.supplied["target-date"]:
		date, e := linearboard.ParsePlanDate("--target-date", v.targetDate)
		if e != nil {
			return nil, e
		}
		in.TargetDate = linearprovider.Field[*string]{Present: true, Value: &date}
	case v.clearTargetDate:
		in.TargetDate = linearprovider.Field[*string]{Present: true}
	}
	return setMilestonePlan(service, ctx, scope, in)
}
