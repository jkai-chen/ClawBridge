package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"clawbridge/internal/bus"
	"clawbridge/internal/linearapp"
	"clawbridge/internal/linearprovider"
)

const (
	spTeam     = "e0000000-0000-4000-8000-0000000000a1"
	spOther    = "e0000000-0000-4000-8000-0000000000a2"
	spPlanned  = "e0000000-0000-4000-8000-0000000000b1"
	spStarted  = "e0000000-0000-4000-8000-0000000000b2"
	spDone     = "e0000000-0000-4000-8000-0000000000b3"
	spArchived = "e0000000-0000-4000-8000-0000000000b4"
	spName     = "Planned \x1b]8;;evil\x07"
)

// fakeStatusProbe scripts the app's answers, records every call, and moves
// its project's status as a send succeeds.
type fakeStatusProbe struct {
	project  linearapp.P8Project
	missing  bool
	archived bool
	trashed  bool
	teams    []string
	statuses []map[string]any
	current  string
	scope    bool
	// setErrs answers each send in turn; moves[i] false leaves the status.
	setErrs []error
	moves   []bool
	readErr error
	sets    int
	calls   []string
	closed  bool
}

func newFakeStatusProbe() *fakeStatusProbe {
	return &fakeStatusProbe{
		project: linearapp.P8Project{Found: true},
		teams:   []string{spTeam},
		statuses: []map[string]any{
			{"id": spPlanned, "name": spName, "type": "planned", "archivedAt": nil},
			{"id": spStarted, "name": spName, "type": "started", "archivedAt": nil},
			{"id": spDone, "name": spName, "type": "completed", "archivedAt": nil},
			{"id": spArchived, "name": spName, "type": "started", "archivedAt": "2026-01-01T00:00:00Z"},
		},
		current: spPlanned,
		scope:   true,
	}
}

func (f *fakeStatusProbe) ProbeP8Project(context.Context, string) (linearapp.P8Project, error) {
	f.calls = append(f.calls, "shape")
	return f.project, nil
}

func (f *fakeStatusProbe) typeOf(id string) string {
	for _, s := range f.statuses {
		if s["id"] == id {
			return s["type"].(string)
		}
	}
	return ""
}

func (f *fakeStatusProbe) GraphQL(_ context.Context, query string, _ map[string]any, out any) error {
	var answer map[string]any
	switch query {
	case statusProbeQuery:
		f.calls = append(f.calls, "read")
		if f.readErr != nil {
			return f.readErr
		}
		teams := []any{}
		for _, t := range f.teams {
			teams = append(teams, map[string]any{"id": t, "name": spName})
		}
		project := map[string]any{"id": p8Project, "name": spName, "trashed": f.trashed, "teams": map[string]any{"nodes": teams}, "status": map[string]any{"id": f.current, "name": spName, "type": f.typeOf(f.current)}}
		if f.archived {
			project["archivedAt"] = "2026-01-01T00:00:00Z"
		}
		answer = map[string]any{"project": project, "projectStatuses": map[string]any{"nodes": f.statuses, "pageInfo": map[string]any{"hasNextPage": false}}}
		if f.missing {
			answer["project"] = nil
		}
	case projectStatusReadBackQuery:
		f.calls = append(f.calls, "readback")
		answer = map[string]any{"project": map[string]any{"status": map[string]any{"id": f.current}}}
	default:
		return errors.New("unexpected query")
	}
	raw, _ := json.Marshal(answer)
	return json.Unmarshal(raw, out)
}

func (f *fakeStatusProbe) HasScope(context.Context, string) (bool, error) {
	f.calls = append(f.calls, "scope")
	return f.scope, nil
}

func (f *fakeStatusProbe) StatusSetter(context.Context) (statusSetter, error) { return f, nil }

func (f *fakeStatusProbe) SetProjectStatus(_ context.Context, project, status string) error {
	f.calls = append(f.calls, "set:"+f.typeOf(status))
	i := f.sets
	f.sets++
	if project != p8Project {
		return errors.New("wrong project")
	}
	var err error
	if i < len(f.setErrs) {
		err = f.setErrs[i]
	}
	if i >= len(f.moves) || f.moves[i] {
		f.current = status
	}
	return err
}

func (f *fakeStatusProbe) Close() error { f.closed = true; return nil }

// statusProbeStore fakes the server's bindings.
type fakeProbeStore struct {
	noTokens
	bindings bus.ProbeBindings
	err      error
}

func (s fakeProbeStore) OperatorProbeBindings(context.Context, string) (bus.ProbeBindings, error) {
	return s.bindings, s.err
}

func runStatusProbe(t *testing.T, f *fakeStatusProbe, store fakeProbeStore, dry bool) (map[string]any, string, error) {
	t.Helper()
	old := newStatusProbeApp
	t.Cleanup(func() { newStatusProbeApp = old })
	newStatusProbeApp = func(linearapp.Store) (statusProbeApp, error) { return f, nil }
	if store.bindings.Teams == nil {
		store.bindings.Teams = []string{spOther, spTeam}
	}
	store.noTokens = noTokens{t}
	var out bytes.Buffer
	err := linearAppStatusProbe(context.Background(), store, strings.ToUpper(p8Project), dry, &out)
	if strings.Contains(out.String(), "Planned") || strings.Contains(out.String(), "\x1b") {
		t.Fatal("a name reached the output:", out.String())
	}
	got := map[string]map[string]any{}
	if out.Len() > 0 {
		if e := json.Unmarshal(out.Bytes(), &got); e != nil {
			t.Fatal(e, out.String())
		}
	}
	return got["project_status_probe"], strings.Join(f.calls, ","), err
}

func TestStatusProbeMovesThereAndBack(t *testing.T) {
	t.Setenv(probeProjectEnv, p8Project)
	f := newFakeStatusProbe()
	out, calls, err := runStatusProbe(t, f, fakeProbeStore{}, false)
	if err != nil || calls != "shape,read,scope,set:started,readback,set:completed,readback,set:planned,readback" || !f.closed || f.current != spPlanned {
		t.Fatal(err, calls, f.current)
	}
	if out["result"] != "PASS" || out["original"].(map[string]any)["id"] != spPlanned || out["original"].(map[string]any)["type"] != "planned" {
		t.Fatal(out)
	}
	if teams := out["team_ids"].([]any); len(teams) != 1 || teams[0] != spTeam || out["bound_team_ids"].([]any)[0] != spTeam {
		t.Fatal(out)
	}
	moves := out["moves"].([]any)
	for i, want := range []string{"a:started", "b:completed", "c:planned"} {
		m := moves[i].(map[string]any)
		if m["step"].(string)+":"+m["to_type"].(string) != want || m["outcome"] != "accepted" || m["code"] != nil {
			t.Fatal(i, m)
		}
	}
}

func TestStatusProbeDryRunSendsNothing(t *testing.T) {
	t.Setenv(probeProjectEnv, p8Project)
	f := newFakeStatusProbe()
	out, calls, err := runStatusProbe(t, f, fakeProbeStore{}, true)
	if err != nil || calls != "shape,read,scope" || out["dry_run"] != true || len(out["plan"].([]any)) != 3 || out["moves"] != nil {
		t.Fatal(err, calls, out)
	}
	if plan := out["plan"].([]any)[0].(map[string]any); plan["to_id"] != spStarted {
		t.Fatal(plan)
	}
}

func TestStatusProbeRefusesBeforeSending(t *testing.T) {
	cases := map[string]struct {
		env   string
		f     func(*fakeStatusProbe)
		store fakeProbeStore
		calls string
	}{
		"env unset":       {"", nil, fakeProbeStore{}, ""},
		"env another":     {spTeam, nil, fakeProbeStore{}, ""},
		"bound":           {p8Project, nil, fakeProbeStore{bindings: bus.ProbeBindings{ProjectBound: true}}, ""},
		"has issues":      {p8Project, func(f *fakeStatusProbe) { f.project.Issues = true }, fakeProbeStore{}, "shape"},
		"has a lead":      {p8Project, func(f *fakeStatusProbe) { f.project.Lead = true }, fakeProbeStore{}, "shape"},
		"not found":       {p8Project, func(f *fakeStatusProbe) { f.missing = true }, fakeProbeStore{}, "shape,read"},
		"archived":        {p8Project, func(f *fakeStatusProbe) { f.archived = true }, fakeProbeStore{}, "shape,read"},
		"trashed":         {p8Project, func(f *fakeStatusProbe) { f.trashed = true }, fakeProbeStore{}, "shape,read"},
		"unbound team":    {p8Project, func(f *fakeStatusProbe) { f.teams = []string{"e0000000-0000-4000-8000-0000000000a9"} }, fakeProbeStore{}, "shape,read"},
		"no write scope":  {p8Project, func(f *fakeStatusProbe) { f.scope = false }, fakeProbeStore{}, "shape,read,scope"},
		"two started":     {p8Project, func(f *fakeStatusProbe) { f.statuses[3]["archivedAt"] = nil }, fakeProbeStore{}, "shape,read"},
		"no completed":    {p8Project, func(f *fakeStatusProbe) { f.statuses[2]["type"] = "canceled" }, fakeProbeStore{}, "shape,read"},
		"bindings unread": {p8Project, nil, fakeProbeStore{err: errors.New("authority unavailable")}, ""},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			t.Setenv(probeProjectEnv, c.env)
			f := newFakeStatusProbe()
			if c.f != nil {
				c.f(f)
			}
			_, calls, err := runStatusProbe(t, f, c.store, false)
			if err == nil || calls != c.calls || f.sets != 0 {
				t.Fatal(err, calls)
			}
			if c.store.err == nil && !strings.Contains(err.Error(), "nothing was sent") {
				t.Fatal(err)
			}
			if name == "not found" && !strings.Contains(err.Error(), "can't be read") {
				t.Fatal(err)
			}
		})
	}
}

func TestStatusProbeStopsOnTheFirstMoveNotAccepted(t *testing.T) {
	t.Setenv(probeProjectEnv, p8Project)
	cases := map[string]struct {
		f       func(*fakeStatusProbe)
		calls   string
		step    string
		outcome string
		code    string
		now     string
	}{
		// A lost answer the read-back settles is accepted, once.
		"lost but landed": {func(f *fakeStatusProbe) { f.setErrs = []error{&linearprovider.Error{Kind: "uncertain"}} }, "", "", "", "", ""},
		"uncertain": {func(f *fakeStatusProbe) {
			f.setErrs, f.moves = []error{nil, &linearprovider.Error{Kind: "uncertain"}}, []bool{true, false}
		}, "shape,read,scope,set:started,readback,set:completed,readback,read", "b", "uncertain", "uncertain", spStarted},
		"refused": {func(f *fakeStatusProbe) {
			f.setErrs, f.moves = []error{&linearprovider.Error{Kind: "provider_rejected"}}, []bool{false}
		}, "shape,read,scope,set:started,read", "a", "refused", "provider_rejected", spPlanned},
		"back fails": {func(f *fakeStatusProbe) {
			f.moves = []bool{true, true, false}
		}, "shape,read,scope,set:started,readback,set:completed,readback,set:planned,readback,read", "c", "uncertain", "read_back_mismatch", spDone},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			f := newFakeStatusProbe()
			c.f(f)
			out, calls, err := runStatusProbe(t, f, fakeProbeStore{}, false)
			if c.step == "" {
				if err != nil || out["result"] != "PASS" || f.sets != 3 {
					t.Fatal(err, calls)
				}
				return
			}
			if err == nil || calls != c.calls || out["result"] != "FAIL" || !strings.Contains(err.Error(), "step "+c.step) || !strings.Contains(err.Error(), "without retrying") {
				t.Fatal(err, calls)
			}
			moves := out["moves"].([]any)
			last := moves[len(moves)-1].(map[string]any)
			if last["step"] != c.step || last["outcome"] != c.outcome || last["code"] != c.code || out["status_now"].(map[string]any)["id"] != c.now {
				t.Fatal(last, out["status_now"])
			}
		})
	}
}

func TestStatusProbeArguments(t *testing.T) {
	for _, c := range []struct {
		args []string
		want string
	}{
		{[]string{"linear-app-probe", "--confirm", "--project-status"}, "needs --project"},
		{[]string{"linear-app-probe", "--confirm", "--project-status", "--p8", "--project", p8Project}, "not --p8"},
		{[]string{"linear-app-probe", "--confirm", "--project-status", "--health-order", "--project", p8Project}, "not --p8"},
		{[]string{"linear-app-probe", "--project-status", "--project", p8Project}, "repeat with --confirm"},
		{[]string{"linear-app-probe", "--project-status", "--dry-run", "--project", p8Project}, "repeat with --confirm"},
	} {
		var out bytes.Buffer
		if err := RunOperator(context.Background(), c.args, &out); err == nil || !strings.Contains(err.Error(), c.want) || out.Len() != 0 {
			t.Fatal(c.args, err)
		}
	}
}
