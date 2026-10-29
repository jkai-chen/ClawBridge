package linearboard

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"clawbridge/internal/bus"
	"clawbridge/internal/linearprovider"
)

// closesRecorder answers the writes a link with --closes makes, in order.
type closesRecorder struct {
	*authorityV2
	ops       []string
	set       map[string]any
	closes    []map[string]any
	closesErr error
}

func (a *closesRecorder) Execute(ctx context.Context, r bus.Request) (bus.Result, error) {
	if r.Operation == "linear_write_charge" {
		return budgetAnswer(r)
	}
	switch r.Operation {
	case "taskboard_set_v2":
		a.ops = append(a.ops, r.Operation)
		a.set = r.Args
		return bus.Result{Source: "local-authority", Data: map[string]any{"revision": 2}}, nil
	case "status_closes_set":
		a.ops = append(a.ops, r.Operation)
		a.closes = append(a.closes, r.Args)
		if a.closesErr != nil {
			return bus.Result{}, a.closesErr
		}
		return bus.Result{Source: "local-authority", Data: map[string]any{"revision": 3}}, nil
	case "status_policy_set":
		a.ops = append(a.ops, r.Operation)
	}
	return a.authorityV2.Execute(ctx, r)
}

func closesTest(t *testing.T, status map[string]any) (*Service, *closesRecorder, *providerV2, bus.Request, LinkInputV2) {
	t.Helper()
	s, a, p, scope := testV2(t)
	a.status = status
	rec := &closesRecorder{authorityV2: a}
	s.cfg.Executor = rec
	s.cfg.Open = func(context.Context, *http.Client, bool) (Provider, error) { return p, nil }
	return s, rec, p, scope, LinkInputV2{TeamIDs: []string{otherTeamID, teamID}, DefaultTeamID: teamID, ProjectIDs: []string{projectB, projectA}, ExpectedRevision: 1}
}

func enabledPolicy(closes map[string]any) map[string]any {
	return map[string]any{"revision": 4, "enabled": true, "teams": map[string]any{}, "closes": closes}
}

// §10.1 a: the preview says who closes each project, and what this link
// would set.
func TestLinkPreviewSaysWhoCloses(t *testing.T) {
	s, _, _, scope, in := closesTest(t, nil)
	r, e := s.LinkV2(context.Background(), scope, in)
	if e != nil || len(r.(LinkPreviewV2).Closes) != 2 || r.(LinkPreviewV2).Closes[0].Closes != "not enforced (no status policy)" {
		t.Fatal(r, e)
	}
	s, _, _, scope, in = closesTest(t, enabledPolicy(map[string]any{projectA: "agent"}))
	in.Closes = map[string]string{projectB: ClosesAgent, projectA: ClosesAgent}
	r, e = s.LinkV2(context.Background(), scope, in)
	lines := r.(LinkPreviewV2).Closes
	if e != nil || lines[0].LinearProject != projectA || lines[0].Closes != "agents" || lines[0].Set != "" || lines[1].Closes != "people (default)" || lines[1].Set != ClosesAgent || r.(LinkPreviewV2).MinimalPolicy != "" {
		t.Fatal(lines, e)
	}
	// With no policy, an agent answer states the minimal policy first.
	s, _, _, scope, in = closesTest(t, nil)
	in.Closes = map[string]string{projectB: ClosesAgent}
	if r, e = s.LinkV2(context.Background(), scope, in); e != nil || !strings.Contains(r.(LinkPreviewV2).MinimalPolicy, "close protection turns on") {
		t.Fatal(r, e)
	}
}

func TestLinkClosesIsCheckedBeforeAnythingIsSent(t *testing.T) {
	for name, c := range map[string]struct {
		edit func(*LinkInputV2)
		want string
	}{
		"outside the link": {func(in *LinkInputV2) {
			in.Closes = map[string]string{"55555555-5555-4555-8555-555555555555": ClosesAgent}
		}, "not one of this link's"},
		"bad value": {func(in *LinkInputV2) { in.Closes = map[string]string{projectA: "bot"} }, "UUID=agent"},
		"team board": {func(in *LinkInputV2) {
			in.ProjectIDs, in.TeamBoard, in.Closes = nil, true, map[string]string{projectA: ClosesHuman}
		}, "team board"},
		"agent without a terminal": {func(in *LinkInputV2) { in.Closes, in.Via = map[string]string{projectB: ClosesAgent}, ClosesViaCLI }, "confirmed at a terminal"},
	} {
		s, rec, _, scope, in := closesTest(t, enabledPolicy(nil))
		in.Confirm, in.Via = true, ClosesViaTTY
		c.edit(&in)
		if _, e := s.LinkV2(context.Background(), scope, in); e == nil || !strings.Contains(e.Error(), c.want) || len(rec.ops) != 0 {
			t.Fatal(name, e, rec.ops)
		}
	}
}

// §10.3: the binding first, naming the projects about to be set, then each
// closes set at the revision it read; a link without --closes sets none.
func TestLinkSetsTheBindingThenWhoCloses(t *testing.T) {
	s, rec, _, scope, in := closesTest(t, enabledPolicy(map[string]any{projectA: "agent"}))
	in.Confirm = true
	if _, e := s.LinkV2(context.Background(), scope, in); e != nil || len(rec.ops) != 1 || rec.set["closes_pending"] != nil {
		t.Fatal("a link without --closes changed who closes:", e, rec.ops, rec.set)
	}
	s, rec, _, scope, in = closesTest(t, enabledPolicy(map[string]any{projectA: "agent"}))
	in.Confirm, in.Via = true, ClosesViaTTY
	in.Closes = map[string]string{projectA: ClosesHuman, projectB: ClosesAgent}
	r, e := s.LinkV2(context.Background(), scope, in)
	if e != nil || strings.Join(rec.ops, ",") != "taskboard_set_v2,status_closes_set,status_closes_set" {
		t.Fatal(r, e, rec.ops)
	}
	if pending := rec.set["closes_pending"].([]string); len(pending) != 2 || pending[0] != projectA || pending[1] != projectB {
		t.Fatal(rec.set)
	}
	if c := rec.closes[0]; c["linear_project"] != projectA || c["closes"] != ClosesHuman || c["via"] != ClosesViaCLI || c["expected_revision"] != int64(4) {
		t.Fatal(c)
	}
	if c := rec.closes[1]; c["linear_project"] != projectB || c["closes"] != ClosesAgent || c["via"] != ClosesViaTTY {
		t.Fatal(c)
	}
	outcomes := r.(map[string]any)["closes"].([]ClosesOutcome)
	if len(outcomes) != 2 || outcomes[1].Outcome != "set" {
		t.Fatal(outcomes)
	}
}

// A failure after the binding keeps the binding, leaves the project to
// people, and says what to re-run.
func TestLinkClosesFailureKeepsTheBinding(t *testing.T) {
	s, rec, _, scope, in := closesTest(t, enabledPolicy(nil))
	rec.closesErr = errors.New("status policy changed: current revision is 5")
	in.Confirm, in.Via = true, ClosesViaTTY
	in.Closes = map[string]string{projectB: ClosesAgent}
	r, e := s.LinkV2(context.Background(), scope, in)
	if e != nil || rec.set == nil {
		t.Fatal(r, e)
	}
	o := r.(map[string]any)["closes"].([]ClosesOutcome)
	if len(o) != 1 || o[0].Outcome != "failed" || !strings.Contains(o[0].Rerun, "closes set --linear-project "+projectB+" --agent") || !strings.Contains(o[0].Rerun, "closed by people") {
		t.Fatal(o)
	}
}

// §10.2 (i): with no policy, an agent answer creates the minimal one only
// when asked, and only where the status list is typed.
func TestLinkMinimalPolicy(t *testing.T) {
	typedStates := []linearprovider.Status{{ID: "22222222-2222-4222-8222-2222222222b5", Name: "Done", Type: "completed"}}
	s, rec, _, scope, in := closesTest(t, nil)
	in.Confirm, in.Via = true, ClosesViaTTY
	in.Closes = map[string]string{projectB: ClosesAgent}
	if _, e := s.LinkV2(context.Background(), scope, in); e == nil || !strings.Contains(e.Error(), "--minimal-status-policy") || len(rec.ops) != 0 {
		t.Fatal("the minimal policy was created unasked:", e, rec.ops)
	}
	in.MinimalPolicy = true
	// R89-L2: a lead is told before the binding is written.
	rec.role = "lead"
	if _, e := s.LinkV2(context.Background(), scope, in); e == nil || !strings.Contains(e.Error(), "only the project owner") || len(rec.ops) != 0 {
		t.Fatal("a lead's minimal policy reached the binding:", e, rec.ops)
	}
	rec.role = "owner"
	if _, e := s.LinkV2(context.Background(), scope, in); e == nil || !strings.Contains(e.Error(), "state types") || len(rec.ops) != 0 {
		t.Fatal("the minimal policy offered with an untyped list:", e, rec.ops)
	}
	s, rec, p, scope, in := closesTest(t, nil)
	p.statuses = typedStates
	rec.role = "owner"
	in.Confirm, in.Via, in.MinimalPolicy = true, ClosesViaTTY, true
	in.Closes = map[string]string{projectB: ClosesAgent, projectA: ClosesHuman}
	if _, e := s.LinkV2(context.Background(), scope, in); e != nil || strings.Join(rec.ops, ",") != "taskboard_set_v2,status_policy_set" {
		t.Fatal(e, rec.ops)
	}
	policy := rec.statusSet["status_policy"].(map[string]any)
	if policy["enabled"] != true || len(policy["teams"].(map[string]any)) != 0 || policy["closes"].(map[string]any)[projectB] != ClosesAgent || len(policy["closes"].(map[string]any)) != 1 || rec.statusSet["via"] != ClosesViaTTY || rec.statusSet["expected_revision"] != int64(0) {
		t.Fatal(rec.statusSet)
	}
}

// §10.1 b: project create --closes agent carries into the confirm
// command; a team board refuses it with nothing sent.
func TestProjectCreateCloses(t *testing.T) {
	s, a, _, scope := adminTest(t)
	r, e := s.Admin(context.Background(), scope, AdminInput{Kind: "project_create", OperationID: "p", TeamID: teamID, Name: "Q4", Closes: ClosesAgent})
	if e != nil || !strings.Contains(r.ConfirmCommand, "--closes "+newProject+"=agent --expected-revision 1 --confirm") || a.sets != 0 {
		t.Fatal(r, e)
	}
	s, _, _, scope = adminTest(t)
	if r, e = s.Admin(context.Background(), scope, AdminInput{Kind: "project_create", OperationID: "p", TeamID: teamID, Name: "Q4", Closes: ClosesHuman}); e != nil || strings.Contains(r.ConfirmCommand, "--closes") {
		t.Fatal("human added to the confirm command:", r, e)
	}
	s, a, p, scope := adminTest(t)
	a.b.Mode, a.b.LinearProjectIDs = "team_board", []string{}
	if _, e = s.Admin(context.Background(), scope, AdminInput{Kind: "project_create", OperationID: "p", TeamID: teamID, Name: "Q4", Closes: ClosesAgent}); e == nil || !strings.Contains(e.Error(), "team board") || len(p.writes) != 0 {
		t.Fatal("--closes on a team board:", e, p.writes)
	}
	if e = ValidateAdmin(AdminInput{Kind: "label_create", OperationID: "l", TeamID: teamID, Name: "Bug", Closes: ClosesAgent}); e == nil {
		t.Fatal("--closes on a label")
	}
}

// §10.4: an older server is told apart, and the whole policy is never
// written instead.
func TestClosesSetOnAnOlderServer(t *testing.T) {
	s, rec, _, scope, _ := closesTest(t, enabledPolicy(nil))
	rec.closesErr = errors.New(`unsupported operation "status_closes_set"`)
	if _, e := s.SetClosesV2(context.Background(), scope, projectA, ClosesHuman, 4, ClosesViaCLI); e == nil || !strings.Contains(e.Error(), "predates closes set") || rec.statusSet != nil {
		t.Fatal(e, rec.statusSet)
	}
}

func TestClosesShowMarksUnboundProjects(t *testing.T) {
	s, _, _, scope, _ := closesTest(t, enabledPolicy(map[string]any{projectA: "agent", "55555555-5555-4555-8555-555555555555": "agent"}))
	v, e := s.ClosesV2(context.Background(), scope)
	if e != nil || v.Revision != 4 || !v.Enforced || len(v.Projects) != 3 {
		t.Fatal(v, e)
	}
	if last := v.Projects[2]; last.Bound || last.Closes != "agents (not bound)" {
		t.Fatal(v.Projects)
	}
}

// R89-M1: a policy file that lets agents close a project is confirmed by
// name before anything is sent, and the write says via cli-tty; without a
// confirm it is refused, and a file that loosens nothing asks nothing.
func TestStatusPolicySetConfirmsAgentCloses(t *testing.T) {
	file := `{"enabled": true, "closes": {"` + projectA + `": "agent"}}`
	s, rec, p, scope, _ := closesTest(t, nil)
	p.statuses = bothTeams()
	if _, e := s.SetStatusPolicyV2(context.Background(), scope, []byte(file), 0, nil); e == nil || !strings.Contains(e.Error(), "confirmed at a terminal") || rec.statusSet != nil {
		t.Fatal("loosened without a confirm:", e, rec.statusSet)
	}
	// The fake's projects have no names; the CLI's confirm refuses a
	// blank one (TestAgentClosesIsConfirmedByAPerson).
	var asked map[string]string
	refuse := func(names map[string]string) error { asked = names; return errors.New("the name typed is not it") }
	if _, e := s.SetStatusPolicyV2(context.Background(), scope, []byte(file), 0, refuse); e == nil || rec.statusSet != nil || len(asked) != 1 {
		t.Fatal("a refused confirm still wrote, or asked wrongly:", e, asked, rec.statusSet)
	}
	if _, e := s.SetStatusPolicyV2(context.Background(), scope, []byte(file), 0, func(map[string]string) error { return nil }); e != nil || rec.statusSet["via"] != ClosesViaTTY {
		t.Fatal(e, rec.statusSet)
	}
	s, rec, p, scope, _ = closesTest(t, enabledPolicy(map[string]any{projectA: "agent"}))
	p.statuses = bothTeams()
	if _, e := s.SetStatusPolicyV2(context.Background(), scope, []byte(file), 4, nil); e != nil || rec.statusSet["via"] != ClosesViaCLI {
		t.Fatal("an unchanged agent key asked for a terminal:", e, rec.statusSet)
	}
}

// bothTeams is sfStates for the bound team and a copy for the other.
func bothTeams() []linearprovider.Status {
	out := append([]linearprovider.Status{}, sfStates...)
	for _, st := range sfStates {
		st.TeamID, st.ID = otherTeamID, strings.Replace(st.ID, "c1", "c2", 1)
		out = append(out, st)
	}
	return out
}

// A3 in the TUI: a link confirmed in the TUI's review, where the person
// typed the project's name, may let agents close, and says so as tui.
func TestLinkClosesConfirmedInTheTUI(t *testing.T) {
	s, rec, _, scope, in := closesTest(t, enabledPolicy(nil))
	in.Confirm, in.Via = true, ClosesViaTUI
	in.Closes = map[string]string{projectB: ClosesAgent}
	if _, e := s.LinkV2(context.Background(), scope, in); e != nil || strings.Join(rec.ops, ",") != "taskboard_set_v2,status_closes_set" || rec.closes[0]["via"] != ClosesViaTUI {
		t.Fatal(e, rec.ops, rec.closes)
	}
}

// A4 §11.4: on a server where project flow is proven, the minimal policy
// also lets project status follow the work, and the preview says so; on
// one where it isn't, or an older server, it doesn't, and says why.
func TestLinkMinimalPolicyProjectFlow(t *testing.T) {
	typedStates := []linearprovider.Status{{ID: "22222222-2222-4222-8222-2222222222b5", Name: "Done", Type: "completed"}}
	for _, proven := range []bool{true, false} {
		status := map[string]any{"revision": 0, "enabled": false, "teams": map[string]any{}}
		if proven {
			status["project_flow_proven"] = true
		}
		s, rec, p, scope, in := closesTest(t, status)
		p.statuses = typedStates
		rec.role = "owner"
		in.Closes = map[string]string{projectB: ClosesAgent}
		r, e := s.LinkV2(context.Background(), scope, in)
		if e != nil {
			t.Fatal(e)
		}
		text := r.(LinkPreviewV2).MinimalPolicy
		follows := strings.Contains(text, "Project status: follows the work") && strings.Contains(text, `"project_flow": true`)
		if follows != proven || (!proven && !strings.Contains(text, "project flow is not yet proven on this server")) {
			t.Fatal(proven, text)
		}
		in.Confirm, in.Via = true, ClosesViaTTY
		if _, e = s.LinkV2(context.Background(), scope, in); e == nil || strings.Contains(e.Error(), "Project status: follows the work") != proven {
			t.Fatal("the refusal without --minimal-status-policy and the preview disagree:", proven, e)
		}
		in.MinimalPolicy = true
		if _, e = s.LinkV2(context.Background(), scope, in); e != nil {
			t.Fatal(e)
		}
		policy := rec.statusSet["status_policy"].(map[string]any)
		if _, on := policy["project_flow"]; on != proven || (proven && policy["project_flow"] != true) {
			t.Fatal("the minimal policy and its preview disagree on project flow:", proven, policy)
		}
	}
}
