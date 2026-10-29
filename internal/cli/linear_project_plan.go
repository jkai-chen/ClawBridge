package cli

import (
	"context"
	"errors"
	"fmt"

	"clawbridge/internal/bus"
	"clawbridge/internal/linearboard"
	"clawbridge/internal/linearprovider"
)

// projectPlanFlags reports whether linear project update was given plan
// fields (FR13) rather than --add-team.
func projectPlanFlags(v linearFlags) bool {
	return v.supplied["priority"] || v.supplied["target-date"] || v.supplied["start-date"] || v.clearTargetDate || v.clearStartDate
}

// setProjectPlan is the board's plan call; tests replace it.
var setProjectPlan = (*linearboard.Service).SetProjectPlan

// linearProjectPlan is linear project update PROJECT [--priority P]
// [--target-date D | --clear-target-date] [--start-date D |
// --clear-start-date] --operation-id ID (FR13): a preview, or with
// --confirm the plan the preview showed, through the person's own Linear
// profile.
func linearProjectPlan(ctx context.Context, service *linearboard.Service, scope bus.Request, v linearFlags, project string) (any, error) {
	if !validLinearOperation(v.operation) {
		return nil, errors.New("linear project update with plan fields requires --operation-id; the preview and its confirm use the same one")
	}
	if v.confirm != (v.expectedSHA != "") {
		return nil, errors.New("--confirm goes with --expected-plan-sha256, the plan_sha256 the preview showed")
	}
	// Without a session the plan is the person's own authority: an agent
	// in its person's shell sets one from its session.
	if v.confirm && scope.Session == "" {
		if marker := agentMarker(closesEnv()); marker != "" {
			return nil, fmt.Errorf("setting a Linear project's plan without a session is a person's, and %s says an agent runs this command; an agent sets one from its session, or previews and asks the project owner or a lead", marker)
		}
	}
	in := linearboard.ProjectPlanInput{ProjectID: project, OperationID: v.operation, Confirm: v.confirm, ExpectedPlanSHA256: v.expectedSHA}
	if v.supplied["priority"] {
		n, e := linearboard.ParsePriority(v.priority)
		if e != nil {
			return nil, e
		}
		in.Priority = &n
	}
	for _, d := range []struct {
		flag  string
		value string
		set   bool
		clear bool
		into  *linearprovider.Field[*string]
	}{{"--target-date", v.targetDate, v.supplied["target-date"], v.clearTargetDate, &in.TargetDate}, {"--start-date", v.startDate, v.supplied["start-date"], v.clearStartDate, &in.StartDate}} {
		switch {
		case d.set:
			date, e := linearboard.ParsePlanDate(d.flag, d.value)
			if e != nil {
				return nil, e
			}
			*d.into = linearprovider.Field[*string]{Present: true, Value: &date}
		case d.clear:
			*d.into = linearprovider.Field[*string]{Present: true}
		}
	}
	return setProjectPlan(service, ctx, scope, in)
}
