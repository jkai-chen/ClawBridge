package doctor

import (
	"context"
	"strings"
	"testing"

	"clawbridge/internal/bus"
)

type statusExecutor struct {
	routeExecutor
	status map[string]any
	err    error
}

func (s statusExecutor) Execute(_ context.Context, r bus.Request) (bus.Result, error) {
	if r.Operation != "comment_watch_status" {
		return bus.Result{}, codedError{"invalid", "unexpected"}
	}
	return bus.Result{Data: s.status}, s.err
}

func TestDoctorReportsTheCommentWatchAndMarkers(t *testing.T) {
	quiet := map[string]any{"watched_issues": 2, "watcher_live": true}
	for name, c := range map[string]struct {
		status        map[string]any
		err           error
		watch, marker string
		text          string
	}{
		"healthy":           {quiet, nil, Pass, Pass, "name sessions by role"},
		"no watcher":        {map[string]any{"watched_issues": 2, "watcher_live": false}, nil, Warn, Pass, "no comment watcher is running"},
		"nobody to notify":  {map[string]any{"watched_issues": 1, "watcher_live": true, "no_recipient": 1, "held_issues": 1}, nil, Warn, Pass, "held, not dropped"},
		"missing label":     {map[string]any{"watched_issues": 1, "watcher_live": true, "markers_no_label": 1}, nil, Pass, Warn, "agent-working label"},
		"stale markers":     {map[string]any{"watched_issues": 1, "watcher_live": true, "markers_pending": 3, "markers_oldest_pending": "2026-10-23T10:00:00Z"}, nil, Pass, Warn, "3 markers pending"},
		"close label drift": {map[string]any{"watched_issues": 0, "watcher_live": false, "close_label_drift": 1}, nil, Pass, Warn, "may still show agent-working"},
		"project budget":    {map[string]any{"watched_issues": 2, "watcher_live": true, "watch_budget": 120, "budget_limit": 120, "budget_source": "project", "budget_used": 37}, nil, Pass, Pass, "37 of the project's own budget of 120 Linear calls"},
		"server budget":     {map[string]any{"watched_issues": 2, "watcher_live": true, "budget_limit": 600, "budget_source": "default", "budget_used": 12}, nil, Pass, Pass, "12 of 600 Linear calls are used"},
		"older server":      {nil, codedError{"invalid", "unsupported operation"}, Warn, Warn, "upgrade the server"},
	} {
		var checks []Check
		checkCommentWatch(context.Background(), statusExecutor{status: c.status, err: c.err}, "w", "p", func(check Check) { checks = append(checks, check) })
		if len(checks) != 2 || checks[0].ID != "linear.watch" || checks[1].ID != "linear.markers" || checks[0].Status != c.watch || checks[1].Status != c.marker {
			t.Errorf("%s: %+v", name, checks)
			continue
		}
		all := checks[0].Message + checks[0].Action + checks[1].Message + checks[1].Action
		if !strings.Contains(all, c.text) {
			t.Errorf("%s: lacks %q: %s", name, c.text, all)
		}
		for _, check := range checks {
			if check.Status == Fail {
				t.Errorf("%s: a marker or watch problem failed doctor", name)
			}
		}
	}
}
