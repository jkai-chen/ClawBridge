package cli

import (
	"strconv"
	"strings"

	"clawbridge/internal/linearboard"
)

// kindSeedOutput makes a seed's answer terminal-safe (label names are text
// people wrote in Linear) and adds to a clean preview the command that
// confirms it. A refused confirm keeps its error and shows the plan.
func kindSeedOutput(seed linearboard.KindSeedResult, v linearFlags, err error) (any, error) {
	out, e := safeData(seed)
	if e != nil {
		return nil, e
	}
	if m, ok := out.(map[string]any); ok && seed.Preview && seed.Refused == 0 && err == nil {
		m["next"] = strings.Join([]string{"clawbridge linear kind seed --operation-id", v.operation, "--profile", v.profile, "--expected-plan-sha256", seed.PlanSHA, "--confirm"}, " ")
	}
	return out, err
}

// kindApplyOutput makes an apply's answer terminal-safe and adds to a clean
// preview the confirm that starts it: a canary of one project first, then
// the same without --canary.
func kindApplyOutput(result linearboard.KindApplyResult, v linearFlags, err error) (any, error) {
	out, e := safeData(result)
	if e != nil {
		return nil, e
	}
	if m, ok := out.(map[string]any); ok && result.Preview && result.Refused == 0 && err == nil {
		confirm := []string{"clawbridge linear kind apply --file", strconv.Quote(terminalSafe(v.filterFile)), "--operation-id", v.operation, "--profile", v.profile, "--expected-plan-sha256", result.PlanSHA, "--confirm"}
		m["next"] = strings.Join(append(confirm, "--canary"), " ")
		m["then"] = "check the canary project in Linear, preview again, and confirm without --canary"
	}
	return out, err
}
