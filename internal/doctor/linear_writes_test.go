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

// An unconfirmed Linear write is a warning naming its kind and operation, as
// a local (typed) or remote (decoded) overview gives it; none passes; an
// older server, without the record, adds no check. The check is
// linear.writes, with linear.project_creates as its alias (FR9).
func TestCheckLinearWrites(t *testing.T) {
	run := func(o overviewExecutor) (Check, bool) {
		var got Check
		added := false
		checkLinearWrites(context.Background(), o, "w", "p", func(c Check) { got, added = c, true })
		return got, added
	}
	c, _ := run(overviewExecutor{data: map[string]any{"project_creates": map[string]any{"unconfirmed": []map[string]any{}}}})
	if c.ID != "linear.writes" || len(c.Aliases) != 1 || c.Aliases[0] != "linear.project_creates" || c.Status != Pass || c.Message != "No Linear write is unconfirmed." {
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
	if c.Status != Warn || !strings.Contains(c.Message, "3 Linear write(s)") || !strings.Contains(c.Message, "project_create op-1,") || !strings.Contains(c.Message, "project_team_add op-2,") || !strings.Contains(c.Message, "project_team_add op-3 (uncertain)") {
		t.Fatal(c)
	}
	// A v0.3.9 server lists them under linear_writes; a stale and an
	// uncertain write of each kind are named, in the overview's order.
	var rows []any
	for _, kind := range []string{"project_create", "issue_move", "project_team_add"} {
		rows = append(rows, map[string]any{"operation_id": kind + "-stale", "write": kind, "state": "reserved"}, map[string]any{"operation_id": kind + "-uncertain", "write": kind, "state": "uncertain"})
	}
	c, _ = run(overviewExecutor{data: map[string]any{"linear_writes": map[string]any{"unconfirmed": rows}, "project_creates": map[string]any{"unconfirmed": []any{}}}})
	if c.Status != Warn || !strings.Contains(c.Message, "6 Linear write(s)") {
		t.Fatal(c)
	}
	for _, kind := range []string{"project_create", "issue_move", "project_team_add"} {
		if !strings.Contains(c.Message, kind+" "+kind+"-stale,") || !strings.Contains(c.Message, kind+" "+kind+"-uncertain (uncertain)") {
			t.Fatal(kind, c.Message)
		}
	}
	if _, added := run(overviewExecutor{data: map[string]any{"project": map[string]any{}}}); added {
		t.Fatal("an older server's overview added a check")
	}
	if _, added := run(overviewExecutor{err: errors.New("down")}); added {
		t.Fatal("a failed read added a check")
	}
}
