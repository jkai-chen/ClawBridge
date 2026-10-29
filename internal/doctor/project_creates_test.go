package doctor

import (
	"context"
	"errors"
	"strings"
	"testing"

	"clawbridge/internal/bus"
)

type overviewExecutor struct {
	routeExecutor
	data any
	err  error
}

func (o overviewExecutor) Execute(_ context.Context, r bus.Request) (bus.Result, error) {
	if r.Operation != "project_overview" {
		return bus.Result{}, errors.New("unexpected " + r.Operation)
	}
	return bus.Result{Data: o.data}, o.err
}

// An unconfirmed coordinator write is a warning naming its kind and operation, as
// a local (typed) or remote (decoded) overview gives it; none passes; an
// older server, without the record, adds no check.
func TestCheckProjectCreates(t *testing.T) {
	run := func(o overviewExecutor) (Check, bool) {
		var got Check
		added := false
		checkProjectCreates(context.Background(), o, "w", "p", func(c Check) { got, added = c, true })
		return got, added
	}
	c, _ := run(overviewExecutor{data: map[string]any{"project_creates": map[string]any{"unconfirmed": []map[string]any{}}}})
	if c.ID != "linear.project_creates" || c.Status != Pass {
		t.Fatal(c)
	}
	for _, rows := range []any{
		[]map[string]any{{"operation_id": "op-1"}},
		[]any{map[string]any{"operation_id": "op-1"}},
	} {
		c, _ = run(overviewExecutor{data: map[string]any{"project_creates": map[string]any{"unconfirmed": rows}}})
		if c.Status != Warn || !strings.Contains(c.Message, "op-1") || !strings.Contains(c.Action, "--ack") {
			t.Fatal(c)
		}
	}
	// Each unconfirmed write is named by its kind; a row without one (an
	// older server's) is a project create.
	c, _ = run(overviewExecutor{data: map[string]any{"project_creates": map[string]any{"unconfirmed": []any{
		map[string]any{"operation_id": "op-1"},
		map[string]any{"operation_id": "op-2", "write": "project_team_add", "state": "reserved"},
		map[string]any{"operation_id": "op-3", "write": "project_team_add", "state": "uncertain"},
	}}}})
	if c.Status != Warn || !strings.Contains(c.Message, "3 coordinator Linear write(s)") || !strings.Contains(c.Message, "project_create op-1,") || !strings.Contains(c.Message, "project_team_add op-2,") || !strings.Contains(c.Message, "project_team_add op-3 (uncertain)") {
		t.Fatal(c)
	}
	if _, added := run(overviewExecutor{data: map[string]any{"project": map[string]any{}}}); added {
		t.Fatal("an older server's overview added a check")
	}
	if _, added := run(overviewExecutor{err: errors.New("down")}); added {
		t.Fatal("a failed read added a check")
	}
}
