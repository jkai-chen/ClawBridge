package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"clawbridge/internal/bus"
	"clawbridge/internal/linearboard"
)

const (
	cliPlanProject   = "11111111-1111-4111-8111-111111111111"
	cliPlanMilestone = "33333333-3333-4333-8333-333333333333"
)

// linear milestone create|update take the right UUID (a project, or a
// milestone with --linear-project), an operation ID for the preview too,
// --confirm with the preview's digest, a date or its clear (never both),
// and a real date; an agent never confirms without a session; nothing
// refused reaches the board.
func TestCLIMilestonePlanFlags(t *testing.T) {
	isolated(t)
	invoke(t, "setup", "--local", "--workspace", "w")
	invoke(t, "project", "create", "p")
	t.Setenv("CLAWBRIDGE_PROJECT", "p")
	old, oldSet := closesEnv, setMilestonePlan
	t.Cleanup(func() { closesEnv, setMilestonePlan = old, oldSet })
	closesEnv = func() []string { return []string{"CLAUDECODE=1"} }
	called := 0
	setMilestonePlan = func(*linearboard.Service, context.Context, bus.Request, linearboard.MilestonePlanInput) (linearboard.MilestonePlanResult, error) {
		called++
		return linearboard.MilestonePlanResult{}, nil
	}
	sha := strings.Repeat("a", 64)
	var out, errs bytes.Buffer
	for want, args := range map[string][]string{
		"project's UUID, never":      {"linear", "milestone", "create", "Roadmap", "--name", "Beta", "--operation-id", "m-1", "--profile", "x"},
		"milestone's UUID from":      {"linear", "milestone", "update", "Beta", "--linear-project", cliPlanProject, "--name", "Beta 2", "--operation-id", "m-1", "--profile", "x"},
		"requires --linear-project":  {"linear", "milestone", "update", cliPlanMilestone, "--name", "Beta 2", "--operation-id", "m-1", "--profile", "x"},
		"requires --linear-project ": {"linear", "milestone", "update", cliPlanMilestone, "--linear-project", "Roadmap", "--name", "Beta 2", "--operation-id", "m-1", "--profile", "x"},
		"requires --operation-id":    {"linear", "milestone", "create", cliPlanProject, "--name", "Beta", "--profile", "x"},
		"goes with --expected-plan":  {"linear", "milestone", "create", cliPlanProject, "--name", "Beta", "--operation-id", "m-1", "--confirm", "--profile", "x"},
		"goes with --expected-plan ": {"linear", "milestone", "create", cliPlanProject, "--name", "Beta", "--operation-id", "m-1", "--expected-plan-sha256", sha, "--profile", "x"},
		"says an agent runs":         {"linear", "milestone", "create", cliPlanProject, "--name", "Beta", "--operation-id", "m-1", "--confirm", "--expected-plan-sha256", sha, "--profile", "x"},
		"give a date or its clear":   {"linear", "milestone", "update", cliPlanMilestone, "--linear-project", cliPlanProject, "--target-date", "2027-01-15", "--clear-target-date", "--operation-id", "m-1", "--profile", "x"},
		"--target-date must be":      {"linear", "milestone", "create", cliPlanProject, "--name", "Beta", "--target-date", "2027-02-30", "--operation-id", "m-1", "--profile", "x"},
		"select --profile":           {"linear", "milestone", "create", cliPlanProject, "--name", "Beta", "--operation-id", "m-1"},
		"usage: clawbridge linear":   {"linear", "milestone", "delete", cliPlanMilestone},
		"unknown flag --clear":       {"linear", "milestone", "create", cliPlanProject, "--clear-target-date", "--name", "Beta", "--operation-id", "m-1", "--profile", "x"},
		"unknown flag --descr":       {"linear", "milestone", "create", cliPlanProject, "--description", "x", "--name", "Beta", "--operation-id", "m-1", "--profile", "x"},
	} {
		out.Reset()
		errs.Reset()
		if e := Run(context.Background(), args, &out, &errs); e == nil || !strings.Contains(e.Error()+errs.String(), strings.TrimSpace(want)) {
			t.Fatal(want, args, e, errs.String())
		}
	}
	if called != 0 {
		t.Fatal("a refused command reached the board", called)
	}
}

// The preview and the confirm print the board's answer; the flags become the
// plan's input: create's positional is the project, update's the milestone
// with --linear-project, a clear a present empty date, no --name no name.
func TestCLIMilestonePlanOutput(t *testing.T) {
	isolated(t)
	invoke(t, "setup", "--local", "--workspace", "w")
	invoke(t, "project", "create", "p")
	t.Setenv("CLAWBRIDGE_PROJECT", "p")
	old, oldSet := closesEnv, setMilestonePlan
	t.Cleanup(func() { closesEnv, setMilestonePlan = old, oldSet })
	closesEnv = func() []string { return nil }
	var got []linearboard.MilestonePlanInput
	sha := strings.Repeat("b", 64)
	setMilestonePlan = func(_ *linearboard.Service, _ context.Context, _ bus.Request, in linearboard.MilestonePlanInput) (linearboard.MilestonePlanResult, error) {
		got = append(got, in)
		r := linearboard.MilestonePlanResult{Plan: linearboard.MilestonePlanPreview{Op: "update", ProjectID: cliPlanProject, MilestoneID: cliPlanMilestone, Teams: []string{}, Changes: []linearboard.MilestonePlanChange{{Field: "targetDate", Current: "2026-12-31", Planned: "none"}}, Sends: []string{"project", "id", "targetDate"}, Flags: []string{"a flag"}, PlanSHA256: sha}, Outcome: "preview", By: "owner", Note: "You confirm this as the project owner, from your own terminal. Nothing else changes: only the fields shown are sent."}
		if in.Confirm {
			r.Outcome, r.Note = "accepted", ""
		}
		return r, nil
	}
	var out, errs bytes.Buffer
	base := []string{"linear", "milestone", "update", cliPlanMilestone, "--linear-project", cliPlanProject, "--clear-target-date", "--operation-id", "m-1", "--profile", "x"}
	if e := Run(context.Background(), base, &out, &errs); e != nil {
		t.Fatal(e, errs.String())
	}
	var printed struct {
		Data map[string]any `json:"data"`
	}
	e := json.Unmarshal(out.Bytes(), &printed)
	shown := printed.Data
	if e != nil || shown["outcome"] != "preview" || shown["by"] != "owner" || shown["plan"].(map[string]any)["plan_sha256"] != sha || !strings.Contains(out.String(), `"current":"2026-12-31"`) || !strings.Contains(out.String(), "a flag") || !strings.Contains(out.String(), "Nothing else changes") {
		t.Fatal(e, out.String())
	}
	out.Reset()
	if e := Run(context.Background(), append(base, "--confirm", "--expected-plan-sha256", sha), &out, &errs); e != nil || !strings.Contains(out.String(), `"outcome":"accepted"`) {
		t.Fatal(e, out.String())
	}
	out.Reset()
	create := []string{"linear", "milestone", "create", cliPlanProject, "--name", "  Beta ", "--target-date", "2027-01-15", "--operation-id", "m-2", "--profile", "x"}
	if e := Run(context.Background(), create, &out, &errs); e != nil {
		t.Fatal(e, errs.String())
	}
	if len(got) != 3 {
		t.Fatal(got)
	}
	u0, u1, c := got[0], got[1], got[2]
	if u0.ProjectID != cliPlanProject || u0.MilestoneID != cliPlanMilestone || u0.Name != nil || !u0.TargetDate.Present || u0.TargetDate.Value != nil || u0.Confirm || !u1.Confirm || u1.ExpectedPlanSHA256 != sha || u1.OperationID != "m-1" {
		t.Fatal(u0, u1)
	}
	if c.ProjectID != cliPlanProject || c.MilestoneID != "" || c.Name == nil || *c.Name != "  Beta " || *c.TargetDate.Value != "2027-01-15" || c.OperationID != "m-2" {
		t.Fatal(c)
	}
	// The global --project still selects the ClawBridge project, given before
	// the command; it is never the Linear project.
	out.Reset()
	got = nil
	alt := []string{"--project", "p", "linear", "milestone", "update", cliPlanMilestone, "--linear-project", cliPlanProject, "--name", "Beta 2", "--operation-id", "m-3", "--profile", "x"}
	if e := Run(context.Background(), alt, &out, &errs); e != nil || len(got) != 1 || got[0].ProjectID != cliPlanProject || got[0].MilestoneID != cliPlanMilestone || *got[0].Name != "Beta 2" || got[0].TargetDate.Present {
		t.Fatal(e, errs.String(), got)
	}
	// After the command a --project is still the global one, so an update
	// that gives only it has no Linear project.
	errs.Reset()
	if e := Run(context.Background(), []string{"linear", "milestone", "update", cliPlanMilestone, "--project", "p", "--name", "Beta 2", "--operation-id", "m-4", "--profile", "x"}, &out, &errs); e == nil || !strings.Contains(e.Error(), "requires --linear-project") {
		t.Fatal(e)
	}
	// --help prints the usage.
	out.Reset()
	if e := Run(context.Background(), []string{"linear", "milestone", "--help"}, &out, &errs); e != nil || !strings.Contains(out.String(), "milestone create PROJECT_UUID") || !strings.Contains(out.String(), "milestone update MILESTONE_UUID --linear-project") {
		t.Fatal(e, out.String())
	}
}
