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
		if read != linear || strings.Contains(strings.Join(ops, ","), "taskboard") != linear || strings.Contains(strings.Join(ops, ","), "dispatcher_model_doctor") != linear {
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

type appExecutor struct {
	routeExecutor
	data map[string]any
	err  error
	got  *bus.Request
}

func (a appExecutor) Execute(_ context.Context, r bus.Request) (bus.Result, error) {
	*a.got = r
	return bus.Result{Source: "server", Data: a.data}, a.err
}

// Doctor shows who makes a hosted project's agent writes in Linear, and
// fails when the app cannot see a bound team.
func TestDoctorShowsTheLinearApp(t *testing.T) {
	const app = "a1000000-0000-4000-8000-000000000001"
	for name, c := range map[string]struct {
		exec   appExecutor
		status string
		text   string
	}{
		"no app":        {appExecutor{err: &remote.HTTPError{Status: 400, Code: "invalid"}}, Pass, "own Linear profile"},
		"down":          {appExecutor{err: &remote.HTTPError{Status: 503, Code: "unavailable"}}, Warn, "could not be read (unavailable)"},
		"all teams":     {appExecutor{data: map[string]any{"configured": true, "app_user_id": app, "teams_visible": map[string]any{"t1": true, "t2": true}}}, Pass, "sees all 2 bound teams"},
		"hidden":        {appExecutor{data: map[string]any{"configured": true, "app_user_id": app, "teams_visible": map[string]any{"t1": true, "t2": false, "t0": false}}}, Fail, "cannot see bound teams t0, t2"},
		"no binding":    {appExecutor{data: map[string]any{"configured": true, "app_user_id": app, "binding": "unpinned"}}, Warn, "binding could not be read: unpinned"},
		"odd answer":    {appExecutor{data: map[string]any{"configured": false}}, Warn, "could not be read"},
		"no failures":   {appExecutor{data: map[string]any{"configured": true, "app_user_id": app, "teams_visible": map[string]any{"t1": true}, "attribution_record_failures": float64(0)}}, Pass, "sees all 1 bound teams"},
		"failed record": {appExecutor{data: map[string]any{"configured": true, "app_user_id": app, "teams_visible": map[string]any{"t1": true}, "attribution_record_failures": float64(2)}}, Warn, "failed to record who asked for 2 of its app writes"},
	} {
		var got bus.Request
		c.exec.got = &got
		var checks []Check
		checkLinearApp(context.Background(), c.exec, "w", "p", func(check Check) { checks = append(checks, check) })
		if len(checks) != 1 || checks[0].ID != "linear.app" || checks[0].Status != c.status || !strings.Contains(checks[0].Message, c.text) {
			t.Errorf("%s: %+v", name, checks)
		}
		if got.Operation != "linear_app_status" || got.Workspace != "w" || got.Project != "p" {
			t.Errorf("%s: %+v", name, got)
		}
	}
	// A local authority has no app, and is never asked.
	var ops []string
	in := Input{Config: Config{Present: true, Valid: true, Mode: "local", Workspace: "w", Project: "p"}, LinearCapabilities: func(context.Context) (linearprovider.Capabilities, error) { return linearprovider.Capabilities{}, nil }}
	Run(context.Background(), in, opsExecutor{&ops})
	if strings.Contains(strings.Join(ops, ","), "linear_app_status") {
		t.Error("a local doctor asked for the app", ops)
	}
}

// A hosted project's doctor run asks for the app.
func TestDoctorRunAsksAHostedServerForTheApp(t *testing.T) {
	var ops []string
	in := Input{Config: Config{Present: true, Valid: true, Mode: "cloud", Workspace: "w", Project: "p"}, LinearCapabilities: func(context.Context) (linearprovider.Capabilities, error) { return linearprovider.Capabilities{}, nil }}
	report := Run(context.Background(), in, serverOps{opsExecutor{&ops}})
	found := false
	for _, check := range report.Checks {
		found = found || check.ID == "linear.app"
	}
	if !found || !strings.Contains(strings.Join(ops, ","), "linear_app_status") {
		t.Fatal("no linear.app check", ops, report.Checks)
	}
}

type serverOps struct{ opsExecutor }

func (s serverOps) Execute(ctx context.Context, r bus.Request) (bus.Result, error) {
	out, err := s.opsExecutor.Execute(ctx, r)
	out.Source = "server"
	return out, err
}

// linear.outbox follows the app status's queue: pass while nothing waits
// long, warn at 15 minutes, fail at an hour or when a write was given up;
// its action names what the writes wait for. Counts only, never text.
func TestDoctorLinearOutbox(t *testing.T) {
	budget := map[string]any{"used": 140, "limit": 600, "source": "default", "window_resets_at": "2026-10-28T15:00:00Z"}
	queue := func(q map[string]any) map[string]any {
		return map[string]any{"configured": true, "app_user_id": "app", "teams_visible": map[string]any{}, "outbox": q, "budget": budget}
	}
	for name, c := range map[string]struct {
		outbox map[string]any
		status string
		text   []string
	}{
		"empty":     {map[string]any{}, Pass, []string{"No Linear writes are queued.", "140 of the project's 600 Linear calls"}},
		"fresh":     {map[string]any{"queued": 2, "oldest_queued_age_s": 300, "waiting": "linear_rate_limit"}, Pass, []string{"2 Linear writes are queued, the oldest for 5 minutes, most for Linear's rate limit."}},
		"sending":   {map[string]any{"sending": 1}, Pass, []string{"1 Linear writes are being sent now."}},
		"warn cap":  {map[string]any{"queued": 3, "oldest_queued_age_s": 15 * 60, "waiting": "project_hourly_cap"}, Warn, []string{"for the project's hourly Linear budget", "15:00 UTC", "project linear-budget set"}},
		"fail age":  {map[string]any{"queued": 1, "oldest_queued_age_s": 3600, "waiting": "ordered_behind"}, Fail, []string{"60 minutes", "behind earlier writes", "write-status"}},
		"dead":      {map[string]any{"dead_24h": 1, "expired_24h": 2}, Fail, []string{"3 writes were given up", "1 failed every try, 2 waited past 3 hours", "Redo the writes"}},
		"one dead":  {map[string]any{"dead_24h": 1}, Fail, []string{"1 writes were given up"}},
		"uncertain": {map[string]any{"uncertain_24h": 1}, Warn, []string{"may or may not have reached Linear", "Check those issues"}},
	} {
		var checks []Check
		checkLinearApp(context.Background(), appExecutor{data: queue(c.outbox), got: new(bus.Request)}, "w", "p", func(check Check) { checks = append(checks, check) })
		if len(checks) != 2 || checks[1].ID != "linear.outbox" || checks[1].Status != c.status {
			t.Errorf("%s: %+v", name, checks)
			continue
		}
		for _, want := range c.text {
			if all := checks[1].Message + " " + checks[1].Action; !strings.Contains(all, want) {
				t.Errorf("%s: lacks %q: %s", name, want, all)
			}
		}
	}
	// A server without the outbox shows no queue check.
	var checks []Check
	checkLinearApp(context.Background(), appExecutor{data: map[string]any{"configured": true, "app_user_id": "app", "teams_visible": map[string]any{}}, got: new(bus.Request)}, "w", "p", func(check Check) { checks = append(checks, check) })
	if len(checks) != 1 {
		t.Fatal("a server without the outbox got a queue check:", checks)
	}
}

// linear.instances warns when more than one server shares the database,
// and says when it can't tell.
func TestDoctorServerInstances(t *testing.T) {
	for name, c := range map[string]struct {
		n      map[string]any
		status string
		text   string
	}{
		"one":     {map[string]any{"live": 1, "known": true}, Pass, "One server paces"},
		"two":     {map[string]any{"live": 2, "known": true}, Warn, "2 servers share this database. Each paces its Linear calls on its own"},
		"unknown": {map[string]any{"live": 1, "known": false}, Pass, "can't be told"},
		"pooler":  {map[string]any{"live": 1, "known": false, "pooler": true}, Pass, "PgBouncer"},
	} {
		var checks []Check
		checkLinearApp(context.Background(), appExecutor{data: map[string]any{"configured": true, "app_user_id": "app", "teams_visible": map[string]any{}, "server_instances": c.n}, got: new(bus.Request)}, "w", "p", func(check Check) { checks = append(checks, check) })
		if len(checks) != 2 || checks[1].ID != "linear.instances" || checks[1].Status != c.status || !strings.Contains(checks[1].Message, c.text) {
			t.Errorf("%s: %+v", name, checks)
		}
	}
}
