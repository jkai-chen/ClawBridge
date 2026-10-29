package cli

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

// project team-adds lists the adds (none yet), without a profile; project
// update takes the project's and the team's UUIDs only, and --confirm goes
// with the preview's plan digest and an operation ID; an agent never
// confirms without a session.
func TestCLIProjectAddTeam(t *testing.T) {
	isolated(t)
	invoke(t, "setup", "--local", "--workspace", "w")
	invoke(t, "project", "create", "p")
	t.Setenv("CLAWBRIDGE_PROJECT", "p")
	var out, errs bytes.Buffer
	if e := Run(context.Background(), []string{"linear", "project", "team-adds"}, &out, &errs); e != nil || !strings.Contains(out.String(), `"added":0`) || !strings.Contains(out.String(), `"with_changes":0`) {
		t.Fatal(e, out.String(), errs.String())
	}
	const project, team = "11111111-1111-4111-8111-111111111111", "22222222-2222-4222-8222-222222222222"
	old := closesEnv
	t.Cleanup(func() { closesEnv = old })
	closesEnv = func() []string { return []string{"CLAUDECODE=1"} }
	for want, args := range map[string][]string{
		"requires --add-team":            {"linear", "project", "update", project, "--profile", "x"},
		"never sent":                     {"linear", "project", "update", project, "--add-team", "Platform", "--profile", "x"},
		"project's UUID, never":          {"linear", "project", "update", "P-ENG-1", "--add-team", team, "--profile", "x"},
		"goes with --expected-plan":      {"linear", "project", "update", project, "--add-team", team, "--confirm", "--profile", "x"},
		"--confirm requires":             {"linear", "project", "update", project, "--add-team", team, "--confirm", "--expected-plan-sha256", strings.Repeat("a", 64), "--profile", "x"},
		"says an agent runs":             {"linear", "project", "update", project, "--add-team", team, "--confirm", "--expected-plan-sha256", strings.Repeat("a", 64), "--operation-id", "t-1", "--profile", "x"},
		"project create|status [set]|cr": {"linear", "project", "remove-team", project},
		"unknown flag --remove-team":     {"linear", "project", "update", project, "--remove-team", team, "--profile", "x"},
	} {
		out.Reset()
		errs.Reset()
		if e := Run(context.Background(), args, &out, &errs); e == nil || !strings.Contains(e.Error()+errs.String(), want) {
			t.Fatal(args, e, errs.String())
		}
	}
}
