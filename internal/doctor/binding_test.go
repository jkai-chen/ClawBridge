package doctor

import (
	"context"
	"errors"
	"strings"
	"testing"

	"clawbridge/internal/bus"
	"clawbridge/internal/linearprovider"
	"clawbridge/internal/remote"
)

type bindingExecutor struct {
	routeExecutor
	err error
	got *bus.Request
}

func (b bindingExecutor) Execute(_ context.Context, r bus.Request) (bus.Result, error) {
	*b.got = r
	return bus.Result{Data: map[string]any{}}, b.err
}

type codedError struct{ code, message string }

func (c codedError) Error() string { return c.message }
func (c codedError) Code() string  { return c.code }

func TestDoctorReportsAnUnreadableBindingWithItsRepairPath(t *testing.T) {
	for name, c := range map[string]struct {
		err    error
		status string
		action string
	}{
		"readable":           {nil, Pass, ""},
		"unreadable local":   {codedError{"binding_unreadable", "this project's task board binding (revision 7) is unreadable"}, Fail, "--expected-revision 7 --confirm --repair"},
		"unreadable cloud":   {&remote.HTTPError{Status: 409, Code: "binding_unreadable", Message: "this project's task board binding (revision 12) is in a format"}, Fail, "--expected-revision 12 --confirm --repair"},
		"no revision named":  {&remote.HTTPError{Status: 409, Code: "binding_unreadable"}, Fail, "--expected-revision N --confirm"},
		"text is not a code": {errors.New("binding_unreadable"), Warn, "Check project access"},
		"other refusal":      {codedError{"forbidden", "refused"}, Warn, "Check project access"},
	} {
		var got bus.Request
		var checks []Check
		checkTaskboardBinding(context.Background(), bindingExecutor{err: c.err, got: &got}, "ws_1", "prj_1", func(check Check) { checks = append(checks, check) })
		if len(checks) != 1 || checks[0].ID != "linear.binding" || checks[0].Status != c.status || !strings.Contains(checks[0].Action, c.action) || (c.status == Fail && !strings.Contains(checks[0].Action, "downgraded")) {
			t.Errorf("%s: %+v", name, checks)
		}
		if got.Operation != "taskboard_get_v2" || got.Workspace != "ws_1" || got.Project != "prj_1" {
			t.Errorf("%s: doctor did not read the resolved project's binding: %+v", name, got)
		}
	}
}

func TestDoctorReadsTheBindingOnlyForALinearUser(t *testing.T) {
	for _, linear := range []bool{false, true} {
		var ops []string
		in := Input{Config: Config{Present: true, Valid: true, Mode: "local", Workspace: "w", Project: "p"}}
		if linear {
			in.LinearCapabilities = func(context.Context) (linearprovider.Capabilities, error) { return linearprovider.Capabilities{}, nil }
		}
		report := Run(context.Background(), in, opsExecutor{&ops})
		read := false
		for _, check := range report.Checks {
			read = read || check.ID == "linear.binding"
		}
		if read != linear || strings.Contains(strings.Join(ops, ","), "taskboard") != linear {
			t.Errorf("linear=%v: binding check %v, ops %v", linear, read, ops)
		}
	}
}

type opsExecutor struct{ ops *[]string }

func (o opsExecutor) Execute(_ context.Context, r bus.Request) (bus.Result, error) {
	*o.ops = append(*o.ops, r.Operation)
	return bus.Result{Source: "local-authority", Data: map[string]any{"projects": []map[string]any{{"slug": "p"}}}}, nil
}
func (opsExecutor) ResolveProject(context.Context, string, string, string) (string, string, error) {
	return "w", "p", nil
}

type policyExecutor struct {
	routeExecutor
	data map[string]any
	err  error
}

func (p policyExecutor) Execute(_ context.Context, r bus.Request) (bus.Result, error) {
	if r.Operation != "label_policy_get" {
		return bus.Result{}, errors.New("unexpected " + r.Operation)
	}
	return bus.Result{Source: "server", Data: p.data}, p.err
}

// Doctor shows the approved label list's revision, who set it and when, or
// that none is set.
func TestDoctorShowsTheLabelPolicy(t *testing.T) {
	for name, c := range map[string]struct {
		exec   policyExecutor
		status string
		text   string
	}{
		"set":     {policyExecutor{data: map[string]any{"revision": 4, "labels": []any{"Bug", "Agent"}, "renames": map[string]any{"Bugs": "Bug"}, "updated_by": "owner@example.com", "updated_at": "2026-10-23T09:00:00Z"}}, Pass, "revision 4, set by owner@example.com at 2026-10-23T09:00:00Z: 2 labels, 1 renames"},
		"unset":   {policyExecutor{data: map[string]any{"revision": 0, "labels": []any{}, "renames": map[string]any{}}}, Pass, "No approved label list"},
		"refused": {policyExecutor{err: codedError{"forbidden", "refused"}}, Warn, "could not be read"},
	} {
		var checks []Check
		checkLabelPolicy(context.Background(), c.exec, "w", "p", func(check Check) { checks = append(checks, check) })
		if len(checks) != 1 || checks[0].ID != "linear.label_policy" || checks[0].Status != c.status || !strings.Contains(checks[0].Message, c.text) {
			t.Errorf("%s: %+v", name, checks)
		}
	}
}
