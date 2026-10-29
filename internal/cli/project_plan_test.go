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

// linear project update takes a team or plan fields, never both (Q3), a
// date or its clear, never both, an operation ID for the preview too, and
// --confirm with the preview's digest; an agent never confirms a plan
// without a session.
func TestCLIProjectPlanFlags(t *testing.T) {
	isolated(t)
	invoke(t, "setup", "--local", "--workspace", "w")
	invoke(t, "project", "create", "p")
	t.Setenv("CLAWBRIDGE_PROJECT", "p")
	const project, team = "11111111-1111-4111-8111-111111111111", "22222222-2222-4222-8222-222222222222"
	old, oldSet := closesEnv, setProjectPlan
	t.Cleanup(func() { closesEnv, setProjectPlan = old, oldSet })
	closesEnv = func() []string { return []string{"CLAUDECODE=1"} }
	called := 0
	setProjectPlan = func(*linearboard.Service, context.Context, bus.Request, linearboard.ProjectPlanInput) (linearboard.ProjectPlanResult, error) {
		called++
		return linearboard.ProjectPlanResult{}, nil
	}
	sha := strings.Repeat("a", 64)
	var out, errs bytes.Buffer
	for want, args := range map[string][]string{
		"never both in one call":    {"linear", "project", "update", project, "--add-team", team, "--priority", "high", "--operation-id", "p-1", "--profile", "x"},
		"never both in one call ":   {"linear", "project", "update", project, "--add-team", team, "--clear-start-date", "--operation-id", "p-1", "--profile", "x"},
		"give a date or its clear":  {"linear", "project", "update", project, "--target-date", "2027-01-15", "--clear-target-date", "--operation-id", "p-1", "--profile", "x"},
		"give a date or its clear ": {"linear", "project", "update", project, "--start-date", "2027-01-15", "--clear-start-date", "--operation-id", "p-1", "--profile", "x"},
		"requires --operation-id":   {"linear", "project", "update", project, "--priority", "high", "--profile", "x"},
		"goes with --expected-plan": {"linear", "project", "update", project, "--priority", "high", "--operation-id", "p-1", "--confirm", "--profile", "x"},
		"says an agent runs":        {"linear", "project", "update", project, "--priority", "high", "--operation-id", "p-1", "--confirm", "--expected-plan-sha256", sha, "--profile", "x"},
		"priority is 0-4":           {"linear", "project", "update", project, "--priority", "highest", "--operation-id", "p-1", "--profile", "x"},
		"--target-date must be":     {"linear", "project", "update", project, "--target-date", "2027-02-30", "--operation-id", "p-1", "--profile", "x"},
		"project's UUID, never":     {"linear", "project", "update", "Roadmap", "--priority", "high", "--operation-id", "p-1", "--profile", "x"},
		"requires --add-team":       {"linear", "project", "update", project, "--operation-id", "p-1", "--profile", "x"},
		"select --profile":          {"linear", "project", "update", project, "--priority", "high", "--operation-id", "p-1"},
	} {
		out.Reset()
		errs.Reset()
		if e := Run(context.Background(), args, &out, &errs); e == nil || !strings.Contains(e.Error()+errs.String(), strings.TrimSpace(want)) {
			t.Fatal(args, e, errs.String())
		}
	}
	if called != 0 {
		t.Fatal("a refused command reached the board", called)
	}
}

// The preview and the confirm print the board's answer as FR10's add
// does; the flags become the plan's input: a priority name its number, a
// clear a present empty date.
func TestCLIProjectPlanOutput(t *testing.T) {
	isolated(t)
	invoke(t, "setup", "--local", "--workspace", "w")
	invoke(t, "project", "create", "p")
	t.Setenv("CLAWBRIDGE_PROJECT", "p")
	const project = "11111111-1111-4111-8111-111111111111"
	old, oldSet := closesEnv, setProjectPlan
	t.Cleanup(func() { closesEnv, setProjectPlan = old, oldSet })
	closesEnv = func() []string { return nil }
	var got []linearboard.ProjectPlanInput
	sha := strings.Repeat("b", 64)
	setProjectPlan = func(_ *linearboard.Service, _ context.Context, _ bus.Request, in linearboard.ProjectPlanInput) (linearboard.ProjectPlanResult, error) {
		got = append(got, in)
		r := linearboard.ProjectPlanResult{Plan: linearboard.ProjectPlanPreview{ProjectID: project, Teams: []string{}, Changes: []linearboard.ProjectPlanChange{{Field: "priority", Current: "medium (3)", Planned: "high (2)"}, {Field: "targetDate", Current: "2026-12-31", Planned: "none"}}, Sends: []string{"id", "priority", "targetDate"}, PlanSHA256: sha}, Outcome: "preview", By: "owner", Note: "You confirm this as the project owner, from your own terminal. Nothing else changes: only the fields shown are sent."}
		if in.Confirm {
			r.Outcome, r.Note = "accepted", ""
		}
		return r, nil
	}
	var out, errs bytes.Buffer
	base := []string{"linear", "project", "update", project, "--priority", "high", "--clear-target-date", "--operation-id", "p-1", "--profile", "x"}
	if e := Run(context.Background(), base, &out, &errs); e != nil {
		t.Fatal(e, errs.String())
	}
	var printed struct {
		Data map[string]any `json:"data"`
	}
	e := json.Unmarshal(out.Bytes(), &printed)
	shown := printed.Data
	if e != nil || shown["outcome"] != "preview" || shown["by"] != "owner" || shown["plan"].(map[string]any)["plan_sha256"] != sha || !strings.Contains(out.String(), `"current":"medium (3)"`) || !strings.Contains(out.String(), "Nothing else changes") {
		t.Fatal(e, out.String())
	}
	out.Reset()
	if e := Run(context.Background(), append(base, "--confirm", "--expected-plan-sha256", sha), &out, &errs); e != nil || !strings.Contains(out.String(), `"outcome":"accepted"`) {
		t.Fatal(e, out.String())
	}
	if len(got) != 2 || *got[0].Priority != 2 || !got[0].TargetDate.Present || got[0].TargetDate.Value != nil || got[0].StartDate.Present || got[0].Confirm || !got[1].Confirm || got[1].ExpectedPlanSHA256 != sha || got[1].OperationID != "p-1" || got[1].ProjectID != project {
		t.Fatal(got)
	}
}
