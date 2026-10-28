package cli

import (
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
