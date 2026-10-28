package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"clawbridge/internal/bus"
	"clawbridge/internal/linearboard"
	"clawbridge/internal/linearprovider"
)

const (
	rolesTeam = "11111111-1111-4111-8111-111111111111"
	rolesAda  = "aaaaaaaa-0000-4000-8000-00000000000a"
	rolesGus  = "bbbbbbbb-0000-4000-8000-00000000000b"
)

// rolesProject is a local project p with a linked board on rolesTeam, and
// a stubbed workspace listing: Ada, an active member, and Gus, a guest.
func rolesProject(t *testing.T) {
	t.Helper()
	dir := isolated(t)
	db := filepath.Join(dir, "cb.db")
	t.Setenv("CLAWBRIDGE_DB", db)
	invoke(t, "setup", "--local", "--workspace", "w")
	invoke(t, "project", "create", "p")
	t.Setenv("CLAWBRIDGE_PROJECT", "p")
	s, err := bus.Open(db)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.Execute(context.Background(), bus.Request{Operation: "taskboard_set_v2", Workspace: "w", Project: "p", Args: map[string]any{"expected_revision": 0, "mode": "project_set", "linear_team_ids": []string{rolesTeam}, "linear_default_team_id": rolesTeam, "linear_project_ids": []string{"3333aaaa-3333-4333-8333-333333333331"}}}); err != nil {
		t.Fatal(err)
	}
	s.Close()
	old := rolesUsers
	t.Cleanup(func() { rolesUsers = old })
	rolesUsers = func(context.Context, Config, linearboard.Executor, bus.Request, string) ([]linearprovider.WorkspaceUser, error) {
		return []linearprovider.WorkspaceUser{
			{ID: rolesAda, Name: "Ada Example", Email: "ada@example.test", Active: true, ActiveKnown: true, GuestKnown: true},
			{ID: rolesGus, Name: "Gus Guest", Active: true, ActiveKnown: true, Guest: true, GuestKnown: true},
		}, nil
	}
}

func rolesRun(t *testing.T, args ...string) (map[string]any, error) {
	t.Helper()
	var out, errs bytes.Buffer
	err := Run(context.Background(), append([]string{"--json", "linear", "roles"}, args...), &out, &errs)
	if err != nil {
		return nil, err
	}
	var r struct {
		Data map[string]any `json:"data"`
	}
	if e := json.Unmarshal(out.Bytes(), &r); e != nil {
		t.Fatal(e, out.String())
	}
	return r.Data, nil
}

// A person is found by name, previewed, then stored; a lead is added; a
// guest can't have a role; a session, a stale revision and a missing
// revision are refused.
func TestCLIRolesPersonAndLead(t *testing.T) {
	rolesProject(t)
	preview, err := rolesRun(t, "person", "ada example", "--role", "reviewer", "--team", rolesTeam, "--expected-revision", "0")
	if err != nil || preview["preview"] != true {
		t.Fatal(preview, err)
	}
	if shown, _ := rolesRun(t, "show"); shown["revision"] != float64(0) {
		t.Fatal("a preview stored", shown)
	}
	if got, err := rolesRun(t, "person", "ada@example.test", "--role", "reviewer", "--team", rolesTeam, "--expected-revision", "0", "--confirm"); err != nil || got["revision"] != float64(1) {
		t.Fatal(got, err)
	}
	got, err := rolesRun(t, "lead", rolesTeam, rolesAda, "--expected-revision", "1", "--confirm")
	if err != nil {
		t.Fatal(err)
	}
	teams := got["teams"].([]any)
	if got["revision"] != float64(2) || len(teams) != 1 || teams[0].(map[string]any)["leads"].([]any)[0] != rolesAda {
		t.Fatal(got)
	}
	if people := got["people"].([]any); people[0].(map[string]any)["role"] != "lead" {
		t.Fatal("the lead isn't a lead person", people)
	}
	invoke(t, "register", "eng", "--instance", "eng-i")
	for want, args := range map[string][]string{
		"can't have a role":       {"person", "Gus Guest", "--role", "engineer", "--team", rolesTeam, "--expected-revision", "2"},
		"current revision is 2":   {"person", "Ada Example", "--remove", "--expected-revision", "1"},
		"--expected-revision is":  {"person", "Ada Example", "--remove"},
		"without --session":       {"--session", "eng", "person", "Ada Example", "--remove", "--expected-revision", "2"},
		"no user of the Linear":   {"person", "Nobody", "--role", "engineer", "--team", rolesTeam, "--expected-revision", "2"},
		"--role and at least one": {"person", "Ada Example", "--expected-revision", "2"},
		"show|set|person|lead":    {"drop"},
	} {
		if args[0] == "--session" {
			var out, errs bytes.Buffer
			if e := Run(context.Background(), append([]string{"--session", "eng", "--instance", "eng-i", "linear", "roles"}, args[2:]...), &out, &errs); e == nil || !strings.Contains(e.Error(), want) {
				t.Fatal(want, e)
			}
			continue
		}
		if _, e := rolesRun(t, args...); e == nil || !strings.Contains(e.Error(), want) {
			t.Fatal(want, e)
		}
	}
	// Removing Ada removes their lead too.
	got, err = rolesRun(t, "person", rolesAda, "--remove", "--expected-revision", "2", "--confirm")
	if err != nil || len(got["people"].([]any)) != 0 || len(got["teams"].([]any)[0].(map[string]any)["leads"].([]any)) != 0 {
		t.Fatal(got, err)
	}
}

// --link stores the account link (not --principal, the global sign-in
// flag, which once swallowed it). Changing a person keeps what the change
// doesn't name: the link, and their leads while they stay a lead there.
func TestCLIRolesPersonKeepsLinkAndLeads(t *testing.T) {
	rolesProject(t)
	got, err := rolesRun(t, "lead", rolesTeam, rolesAda, "--expected-revision", "0", "--confirm")
	if err != nil {
		t.Fatal(err)
	}
	me := got["updated_by"].(string)
	got, err = rolesRun(t, "person", rolesAda, "--role", "lead", "--team", rolesTeam, "--link", me, "--expected-revision", "1", "--confirm")
	if err != nil {
		t.Fatal(err)
	}
	keeps := func(got map[string]any, lead bool) {
		t.Helper()
		if p := got["people"].([]any)[0].(map[string]any); !strings.EqualFold(fmt.Sprint(p["principal"]), me) {
			t.Fatal("the link was dropped", got)
		}
		if leads := got["teams"].([]any)[0].(map[string]any)["leads"].([]any); (len(leads) == 1) != lead {
			t.Fatal("leads", got)
		}
	}
	keeps(got, true)
	got, err = rolesRun(t, "person", "Ada Example", "--role", "lead", "--team", rolesTeam, "--expected-revision", "2", "--confirm")
	if err != nil {
		t.Fatal(err)
	}
	keeps(got, true)
	got, err = rolesRun(t, "person", "Ada Example", "--role", "engineer", "--team", rolesTeam, "--expected-revision", "3", "--confirm")
	if err != nil {
		t.Fatal(err)
	}
	keeps(got, false)
}

// A roles file is checked person by person against the workspace's users:
// a guest's ID is refused there too, not only by name.
func TestCLIRolesSetFileChecksEveryone(t *testing.T) {
	rolesProject(t)
	dir := t.TempDir()
	write := func(people string) string {
		path := filepath.Join(dir, "roles.json")
		if err := os.WriteFile(path, []byte(`{"teams": [], "people": [`+people+`]}`), 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	guest := write(`{"linear_user_id": "` + rolesGus + `", "role": "engineer", "teams": ["` + rolesTeam + `"]}`)
	if _, err := rolesRun(t, "set", "--file", guest, "--expected-revision", "0", "--confirm"); err == nil || !strings.Contains(err.Error(), "can't have a role") {
		t.Fatal(err)
	}
	ok := write(`{"linear_user_id": "` + rolesAda + `", "role": "engineer", "teams": ["` + rolesTeam + `"]}`)
	if got, err := rolesRun(t, "set", "--file", ok, "--expected-revision", "0", "--confirm"); err != nil || got["revision"] != float64(1) {
		t.Fatal(got, err)
	}
}

// roles publish passes the parent and flags through, refuses a session
// and two parents, and fails when the publish was refused.
func TestCLIRolesPublish(t *testing.T) {
	rolesProject(t)
	var got linearboard.RolesPublishInput
	refused := ""
	old := rolesPublish
	t.Cleanup(func() { rolesPublish = old })
	rolesPublish = func(_ context.Context, _ linearboard.Executor, scope bus.Request, _ string, in linearboard.RolesPublishInput) (linearboard.RolesPublishResult, error) {
		if scope.Session != "" {
			t.Fatal("published as a session")
		}
		got = in
		return linearboard.RolesPublishResult{Action: "create", Refused: refused}, nil
	}
	out, err := rolesRun(t, "publish", "--team", strings.ToUpper(rolesTeam), "--recreate", "--confirm")
	if err != nil || out["action"] != "create" || got.ParentKind != "team" || got.ParentID != rolesTeam || !got.Recreate || !got.Confirm {
		t.Fatal(out, got, err)
	}
	if _, err = rolesRun(t, "publish"); err != nil || got.ParentID != "" || got.Confirm {
		t.Fatal("the remembered parent", got, err)
	}
	refused = "the document matches deny patterns [x]"
	if _, err = rolesRun(t, "publish", "--confirm"); err == nil || !strings.Contains(err.Error(), "nothing was published") {
		t.Fatal(err)
	}
	if _, err = rolesRun(t, "publish", "--team", rolesTeam, "--linear-project", rolesTeam); err == nil || !strings.Contains(err.Error(), "one of --team") {
		t.Fatal(err)
	}
	invoke(t, "register", "eng", "--instance", "eng-i")
	var o, e bytes.Buffer
	if err = Run(context.Background(), []string{"--session", "eng", "--instance", "eng-i", "linear", "roles", "publish"}, &o, &e); err == nil || !strings.Contains(err.Error(), "without --session") {
		t.Fatal(err)
	}
}

// roles project-lead shows the setting and switches it (the owner, no
// session); apply-leads passes the revision, --replace and --confirm
// through and refuses a session or a missing revision.
func TestCLIRolesProjectLeadAndApplyLeads(t *testing.T) {
	rolesProject(t)
	if got, err := rolesRun(t, "project-lead"); err != nil || got["mode"] != "off" || got["revision"] != float64(0) {
		t.Fatal(got, err)
	}
	if _, err := rolesRun(t, "project-lead", "on"); err == nil || !strings.Contains(err.Error(), "--expected-revision") {
		t.Fatal(err)
	}
	if _, err := rolesRun(t, "project-lead", "auto", "--expected-revision", "0"); err == nil || !strings.Contains(err.Error(), "on or off") {
		t.Fatal(err)
	}
	if got, err := rolesRun(t, "project-lead", "on", "--expected-revision", "0"); err != nil || got["mode"] != "on" || got["on_at"] == nil {
		t.Fatal(got, err)
	}
	var got linearboard.ApplyLeadsInput
	old := rolesApplyLeads
	t.Cleanup(func() { rolesApplyLeads = old })
	rolesApplyLeads = func(_ context.Context, _ linearboard.Executor, scope bus.Request, _ string, in linearboard.ApplyLeadsInput) (linearboard.ApplyLeadsResult, error) {
		if scope.Session != "" {
			t.Fatal("applied as a session")
		}
		got = in
		return linearboard.ApplyLeadsResult{Revision: in.Revision}, nil
	}
	if _, err := rolesRun(t, "apply-leads", "--expected-revision", "3", "--replace", strings.ToUpper(rolesTeam), "--confirm"); err != nil || got.Revision != 3 || got.Replace != rolesTeam || !got.Confirm {
		t.Fatal(got, err)
	}
	if _, err := rolesRun(t, "apply-leads"); err == nil || !strings.Contains(err.Error(), "--expected-revision") {
		t.Fatal(err)
	}
	invoke(t, "register", "eng", "--instance", "eng-i")
	var o, e bytes.Buffer
	for _, args := range [][]string{{"apply-leads", "--expected-revision", "1"}, {"project-lead", "off", "--expected-revision", "1"}} {
		if err := Run(context.Background(), append([]string{"--session", "eng", "--instance", "eng-i", "linear", "roles"}, args...), &o, &e); err == nil || !strings.Contains(err.Error(), "without --session") {
			t.Fatal(args, err)
		}
	}
}
