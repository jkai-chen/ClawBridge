package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"clawbridge/internal/bus"
)

// cancelKindsProject is a local project p with a linked board, the kinds
// Engineering and Research, and a status policy at revision 1.
func cancelKindsProject(t *testing.T) {
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
	defer s.Close()
	for _, r := range []bus.Request{
		{Operation: "taskboard_set_v2", Args: map[string]any{"expected_revision": 0, "mode": "project_set", "linear_team_ids": []string{rolesTeam}, "linear_default_team_id": rolesTeam, "linear_project_ids": []string{"3333aaaa-3333-4333-8333-333333333331"}}},
		{Operation: "project_kind_set", Args: map[string]any{"expected_revision": 0, "kinds": []string{"Engineering", "Research"}}},
		{Operation: "status_policy_set", Args: map[string]any{"expected_revision": 0, "status_policy": map[string]any{"enabled": true}}},
	} {
		r.Workspace, r.Project = "w", "p"
		if _, err = s.Execute(context.Background(), r); err != nil {
			t.Fatal(r.Operation, err)
		}
	}
}

func cancelKindsRun(t *testing.T, stderr *bytes.Buffer, args ...string) (map[string]any, error) {
	t.Helper()
	var out bytes.Buffer
	if stderr == nil {
		stderr = &bytes.Buffer{}
	}
	if err := Run(context.Background(), append([]string{"--json", "linear", "cancel-kinds"}, args...), &out, stderr); err != nil {
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

// show lists the kinds; set --kinds previews and has the person type the
// names; --none needs no typing.
func TestCLICancelKindsShowAndSet(t *testing.T) {
	cancelKindsProject(t)
	got, err := cancelKindsRun(t, nil, "show")
	if err != nil || got["revision"] != float64(1) || len(got["agent_may_cancel_kinds"].([]any)) != 0 || len(got["kinds"].([]any)) != 2 || got["line"] != "Agents never cancel issues." {
		t.Fatal(got, err)
	}
	stderr := typing(t, true, "Engineering, Research\n")
	if got, err = cancelKindsRun(t, stderr, "set", "--kinds", "research,engineering", "--expected-revision", "1"); err == nil {
		t.Fatal("typed in another order than shown:", got)
	}
	stderr = typing(t, true, "Research, Engineering\n")
	if got, err = cancelKindsRun(t, stderr, "set", "--kinds", "research,engineering", "--expected-revision", "1"); err != nil || got["revision"] != float64(2) {
		t.Fatal(got, err)
	}
	if p := stderr.String(); !strings.Contains(p, "projects of kind Research, Engineering") || strings.Contains(p, "Until the enforcement ships") {
		t.Fatal("preview:", p)
	}
	if got, _ = cancelKindsRun(t, nil, "show"); got["line"] != "Agents may cancel or mark Duplicate in projects of kind Research, Engineering." {
		t.Fatal(got)
	}
	typing(t, false, "")
	if got, err = cancelKindsRun(t, nil, "set", "--none", "--expected-revision", "2"); err != nil || got["agent_may_cancel_kinds"] != nil {
		t.Fatal(got, err)
	}
}

// Refused: a session, an agent's environment, stdin that is no terminal,
// a name that is no kind, and a set without one of --kinds or --none.
func TestCLICancelKindsRefusals(t *testing.T) {
	cancelKindsProject(t)
	invoke(t, "register", "eng", "--instance", "eng-i")
	for name, c := range map[string]struct {
		terminal bool
		environ  []string
		args     []string
		want     string
	}{
		"session":      {true, nil, []string{"--session", "eng", "--instance", "eng-i"}, "never done with --session"},
		"claude code":  {true, []string{"CLAUDECODE=1"}, nil, "CLAUDECODE says an agent runs this command"},
		"not a tty":    {false, nil, nil, "stdin is not a terminal"},
		"unknown kind": {true, nil, []string{"--kinds", "Ops"}, `"Ops" is not a kind of this project`},
		"both":         {true, nil, []string{"--kinds", "Engineering", "--none"}, "one of --kinds A[,B] or --none"},
		"no revision":  {true, nil, []string{"--kinds", "Engineering", "--expected-revision", "-1"}, "--expected-revision from cancel-kinds show"},
	} {
		stderr := typing(t, c.terminal, "Engineering\n", c.environ...)
		args := []string{"--json"}
		set := []string{"linear", "cancel-kinds", "set", "--kinds", "Engineering", "--expected-revision", "1"}
		if len(c.args) > 0 && c.args[0] == "--session" {
			args = append(args, c.args...)
		} else if len(c.args) > 0 {
			set = append([]string{"linear", "cancel-kinds", "set", "--expected-revision", "1"}, c.args...)
		}
		var out bytes.Buffer
		if err := Run(context.Background(), append(args, set...), &out, stderr); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Fatal(name, err)
		}
	}
	if got, _ := cancelKindsRun(t, nil, "show"); got["revision"] != float64(1) {
		t.Fatal("a refusal changed the policy:", got)
	}
}

// A status-policy file that lists kinds is refused under an agent or a
// session, changed or not; an empty list or none is not.
func TestStatusPolicyFileWithCancelKindsIsNotAnAgentsToRun(t *testing.T) {
	file := []byte(`{"enabled": true, "Agent_May_Cancel_Kinds": ["Engineering"]}`)
	typing(t, true, "", "CLAUDECODE=1")
	if err := refuseAgentCloseFile(Config{}, file); err == nil || !strings.Contains(err.Error(), "who may cancel is a person's decision, and CLAUDECODE") {
		t.Fatal(err)
	}
	typing(t, true, "")
	if err := refuseAgentCloseFile(Config{Session: "s"}, file); err == nil || !strings.Contains(err.Error(), "--session") {
		t.Fatal(err)
	}
	if err := refuseAgentCloseFile(Config{}, file); err != nil {
		t.Fatal("a person:", err)
	}
	typing(t, true, "", "CLAUDECODE=1")
	if err := refuseAgentCloseFile(Config{}, []byte(`{"enabled": true, "agent_may_cancel_kinds": []}`)); err != nil {
		t.Fatal("an empty list:", err)
	}
}
