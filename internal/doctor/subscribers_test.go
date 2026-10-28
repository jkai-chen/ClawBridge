package doctor

import (
	"context"
	"errors"
	"strings"
	"testing"

	"clawbridge/internal/bus"
)

type subscribersExecutor struct {
	data map[string]any
	err  error
}

func (e subscribersExecutor) Execute(context.Context, bus.Request) (bus.Result, error) {
	return bus.Result{Data: e.data}, e.err
}

func (subscribersExecutor) ResolveProject(context.Context, string, string, string) (string, string, error) {
	return "w", "p", nil
}

// The doctor hint for a board linked before the setting (Q1), Linear
// refusing the owner, rows that failed; never an email (B4).
func TestCheckIssueSubscribers(t *testing.T) {
	for name, c := range map[string]struct {
		data   map[string]any
		err    error
		status string
		want   string
	}{
		"older server": {nil, errors.New("unsupported operation"), Pass, "doesn't subscribe anyone"},
		"unproven":     {map[string]any{"proven": false, "enabled": true}, nil, Pass, "after a live check"},
		"off":          {map[string]any{"proven": true, "enabled": false}, nil, Warn, "linear subscribers on"},
		"owner":        {map[string]any{"proven": true, "enabled": true, "owner_not_in_linear_until": "2026-10-29T00:00:00Z"}, nil, Warn, "subscribers add LINEAR_USER_UUID"},
		"failed":       {map[string]any{"proven": true, "enabled": true, "rows": map[string]any{"failed": float64(2)}}, nil, Warn, "2 agent issues"},
		"on":           {map[string]any{"proven": true, "enabled": true, "rows": map[string]any{"failed": float64(0)}}, nil, Pass, "notify the project's people"},
	} {
		var got Check
		checkIssueSubscribers(context.Background(), subscribersExecutor{c.data, c.err}, "w", "p", func(ch Check) { got = ch })
		text := got.Message + " " + got.Action
		if got.ID != "linear.issue_subscribers" || got.Status != c.status || !strings.Contains(text, c.want) || strings.Contains(text, "@") {
			t.Fatalf("%s: %+v", name, got)
		}
	}
}
