package integration

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"

	"clawbridge/internal/setup"
)

type recordingRunner struct {
	mu      sync.Mutex
	calls   [][]string
	failAt  int
	failure error
}

func (r *recordingRunner) Run(_ context.Context, args []string) ([]byte, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, append([]string(nil), args...))
	if r.failAt > 0 && len(r.calls) == r.failAt {
		return nil, r.failure
	}
	return []byte("synthetic"), nil
}

func (r *recordingRunner) commands() [][]string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([][]string, len(r.calls))
	for i, call := range r.calls {
		out[i] = append([]string(nil), call...)
	}
	return out
}

func setupCreateInput() setup.CreateInput {
	return setup.CreateInput{
		Project: "claw-bridge-dev", Zone: "europe-west2-a",
		VM: "instance-claw-bridge-dev", MachineType: "g1-small",
		DiskType: "pd-balanced", DiskSizeGB: 10,
		ImageFamily: "debian-12", ImageProject: "debian-cloud",
		Network: "default", Subnetwork: "default",
		FirewallRule:     "allow-iap-ssh",
		ServiceAccountID: "clawbridge-client", HumanAccount: "person@example.test",
		Audience:  "https://clawbridge.claw-bridge-dev.internal",
		Workspace: "pilot", SnapshotPolicy: "clawbridge-daily",
		SnapshotRetentionDays: 7, SnapshotStartTimeUTC: "02:00",
		Cost: setup.MonthlyCost{
			Currency: "GBP", ComputeMinor: 1000, DiskMinor: 200, SnapshotMinor: 50,
			IPv4Minor: 300, EgressMinor: 100, TaxMinor: 330,
			PricingBasis: "Synthetic reviewer fixture", EstimatedMonth: "2026-10",
		},
	}
}

func TestSetupPlanAndApplyAreDeterministicAndFenced(t *testing.T) {
	input := setupCreateInput()
	first, err := setup.BuildPlan(input)
	if err != nil {
		t.Fatal(err)
	}
	second, err := setup.BuildPlan(input)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(first, second) {
		t.Fatal("same setup input produced different plans")
	}
	if first.Project != input.Project || first.Region != "europe-west2" ||
		first.VM != input.VM || first.Cost.TotalMonthlyMinor != 1980 ||
		!strings.Contains(first.Confirmation, input.Project+"/"+input.VM+"/"+input.Zone) {
		t.Fatalf("plan omitted explicit scope/cost: %+v", first)
	}
	if len(first.Operations) == 0 {
		t.Fatal("plan contains no operations")
	}
	for _, operation := range first.Operations {
		if len(operation.Command) == 0 || operation.Command[0] != "gcloud" {
			t.Fatalf("invalid operation command: %+v", operation)
		}
		joined := strings.Join(operation.Command, "\x00")
		for _, required := range []string{
			"--project=" + input.Project,
			"--account=" + input.HumanAccount,
		} {
			if !strings.Contains(joined, required) {
				t.Fatalf("%s does not pin %s: %v", operation.Name, required, operation.Command)
			}
		}
	}

	ctx := context.Background()
	for name, phrase := range map[string]string{
		"empty": "", "wrong": "CREATE another-project/another-vm/europe-west2-a GBP 19.80 plan=deadbeef0000",
		"case": strings.ToLower(first.Confirmation),
	} {
		t.Run("refuse-"+name, func(t *testing.T) {
			runner := &recordingRunner{}
			if _, err := setup.Apply(ctx, input, first, phrase, runner); err == nil {
				t.Fatal("mismatched confirmation was accepted")
			}
			if calls := runner.commands(); len(calls) != 0 {
				t.Fatalf("refused apply ran commands: %v", calls)
			}
		})
	}

	changed := input
	changed.Zone = "europe-west2-b"
	runner := &recordingRunner{}
	if _, err := setup.Apply(ctx, changed, first, first.Confirmation, runner); err == nil {
		t.Fatal("changed input inherited confirmation for an old plan")
	}
	if calls := runner.commands(); len(calls) != 0 {
		t.Fatalf("stale plan ran commands: %v", calls)
	}

	result, err := setup.Apply(ctx, input, first, first.Confirmation, runner)
	if err != nil {
		t.Fatal(err)
	}
	got := runner.commands()
	if len(got) != len(first.Operations) || len(result.Executed) != len(first.Operations) {
		t.Fatalf("applied %d commands/%d results, want %d", len(got), len(result.Executed), len(first.Operations))
	}
	for i, operation := range first.Operations {
		if !reflect.DeepEqual(got[i], operation.Command) {
			t.Fatalf("command %d differs from reviewed plan:\n got %v\nwant %v", i, got[i], operation.Command)
		}
	}
	if result.Fingerprint != first.Fingerprint {
		t.Fatalf("result fingerprint=%q, plan=%q", result.Fingerprint, first.Fingerprint)
	}
}

func TestSetupApplyStopsAtFirstFailure(t *testing.T) {
	input := setupCreateInput()
	plan, err := setup.BuildPlan(input)
	if err != nil {
		t.Fatal(err)
	}
	runner := &recordingRunner{failAt: 4, failure: errors.New("synthetic command failure")}
	result, err := setup.Apply(context.Background(), input, plan, plan.Confirmation, runner)
	if err == nil || !strings.Contains(err.Error(), plan.Operations[3].Name) {
		t.Fatalf("failure not attributable: %v", err)
	}
	if got := len(runner.commands()); got != 4 {
		t.Fatalf("ran %d commands after failure at 4", got)
	}
	if len(result.Executed) != 3 {
		t.Fatalf("reported %d completed commands, want 3", len(result.Executed))
	}
}

func TestSetupRejectsMissingExplicitProjectAndCost(t *testing.T) {
	for name, mutate := range map[string]func(*setup.CreateInput){
		"project": func(in *setup.CreateInput) { in.Project = "" },
		"zone":    func(in *setup.CreateInput) { in.Zone = "" },
		"cost":    func(in *setup.CreateInput) { in.Cost = setup.MonthlyCost{} },
	} {
		t.Run(name, func(t *testing.T) {
			input := setupCreateInput()
			mutate(&input)
			if plan, err := setup.BuildPlan(input); err == nil {
				t.Fatalf("missing %s accepted: %+v", name, plan)
			}
		})
	}
}
