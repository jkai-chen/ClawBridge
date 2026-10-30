package doctor

import (
	"context"
	"strings"
	"testing"

	"clawbridge/internal/bus"
)

type webhookExecutor struct {
	routeExecutor
	status map[string]any
	err    error
	op     string
}

func (s *webhookExecutor) Execute(_ context.Context, r bus.Request) (bus.Result, error) {
	s.op = r.Operation
	if r.Operation != "webhook_status" {
		return bus.Result{}, codedError{"invalid", "unexpected"}
	}
	return bus.Result{Data: s.status}, s.err
}

func whTeam(id, state string, configured bool) map[string]any {
	return map[string]any{"team_id": id, "configured": configured, "state": state}
}

func whStatus(extra map[string]any, teams ...map[string]any) map[string]any {
	list := []any{}
	for _, t := range teams {
		list = append(list, t)
	}
	s := map[string]any{"teams": list}
	for k, v := range extra {
		s[k] = v
	}
	return s
}

func TestDoctorReportsLinearWebhooks(t *testing.T) {
	const ceiling = "verified deliveries hit the server's ceiling; repeated 429s can get the webhook disabled in Linear: check it is still enabled"
	for name, c := range map[string]struct {
		status map[string]any
		err    error
		want   string // "" means no check
		text   string
	}{
		"every team live": {whStatus(nil, whTeam("t1", "live", true), whTeam("t2", "live", true)), nil, Pass, "live"},
		"unconfigured team": {whStatus(nil, whTeam("t1", "live", true), whTeam("t2", "unconfigured", false)), nil, Warn,
			"run `clawbridge linear webhook-setup` for the webhook to create in Linear; the server operator adds its secret to CLAWBRIDGE_LINEAR_WEBHOOK_SECRETS (docs/operations/deployment.md)"},
		"waiting first": {whStatus(nil, whTeam("t1", "waiting_first", true)), nil, Warn, "create or edit an issue in that team"},
		"degraded":      {whStatus(nil, whTeam("t1", "degraded", true)), nil, Warn, "check the webhook in Linear; until 24 h after the miss the team polls on its usual cadence, not hourly"},
		"signature rejects": {whStatus(map[string]any{"signature_rejects_24h": 3}, whTeam("t1", "live", true)), nil, Warn,
			"a secret may be wrong or rotated"},
		"unbound deliveries": {whStatus(map[string]any{"unbound_team_deliveries_24h": 2}, whTeam("t1", "live", true)), nil, Warn,
			"delete that webhook in Linear"},
		"throttled": {whStatus(map[string]any{"verified_throttled_24h": 1}, whTeam("t1", "live", true)), nil, Warn, ceiling},
		"gate off":  {whStatus(nil, whTeam("t1", "inert", true)), nil, Pass, "webhooks verified but inert until proven"},
		"no team configured": {whStatus(map[string]any{"signature_rejects_24h": 5},
			whTeam("t1", "unconfigured", false)), nil, "", ""},
		"older server": {nil, codedError{"invalid", "unsupported operation"}, "", ""},
		"read error":   {nil, codedError{"unavailable", "server unreachable"}, Warn, "could not be read"},
	} {
		var checks []Check
		checkLinearWebhooks(context.Background(), &webhookExecutor{status: c.status, err: c.err}, "w", "p", func(ch Check) { checks = append(checks, ch) })
		if c.want == "" {
			if len(checks) != 0 {
				t.Errorf("%s: %+v", name, checks)
			}
			continue
		}
		if len(checks) != 1 || checks[0].ID != "linear.webhooks" || checks[0].Title != "Linear webhooks" || checks[0].Status != c.want {
			t.Errorf("%s: %+v", name, checks)
			continue
		}
		if all := checks[0].Message + " " + checks[0].Action; !strings.Contains(all, c.text) {
			t.Errorf("%s: lacks %q: %s", name, c.text, all)
		}
	}
}

func TestDoctorLinearWebhooksJoinsDistinctActions(t *testing.T) {
	var checks []Check
	s := whStatus(map[string]any{"signature_rejects_24h": 1}, whTeam("t1", "waiting_first", true), whTeam("t2", "waiting_first", true))
	checkLinearWebhooks(context.Background(), &webhookExecutor{status: s}, "w", "p", func(ch Check) { checks = append(checks, ch) })
	if len(checks) != 1 || strings.Count(checks[0].Action, "create or edit an issue") != 1 || !strings.Contains(checks[0].Action, "; a secret may be wrong") {
		t.Fatalf("%+v", checks)
	}
}
