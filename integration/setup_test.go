package integration

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"

	"clawbridge/internal/bus"
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

type joinExecutor struct {
	mu         sync.Mutex
	resolve    [][4]string
	requests   []bus.Request
	resolveID  [2]string
	resolveErr error
	result     bus.Result
	execErr    error
}

func (e *joinExecutor) ResolveProject(_ context.Context, workspace, project, remote string) (string, string, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.resolve = append(e.resolve, [4]string{workspace, project, remote, ""})
	return e.resolveID[0], e.resolveID[1], e.resolveErr
}

func (e *joinExecutor) Execute(_ context.Context, req bus.Request) (bus.Result, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.requests = append(e.requests, req)
	return e.result, e.execErr
}

func (e *joinExecutor) operations() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([]string, len(e.requests))
	for i, req := range e.requests {
		out[i] = req.Operation
	}
	return out
}

func joinInput(path string) setup.JoinInput {
	return setup.JoinInput{
		ConfigPath: path, Workspace: "workspace", Project: "project",
		Session: "session", Instance: "instance",
		Server: "https://bus.example.test", Audience: "https://audience.example.test",
		Account:                   "person@example.test",
		ImpersonateServiceAccount: "client@example.test.iam.gserviceaccount.com",
		Principal:                 "client@example.test.iam.gserviceaccount.com",
		Cache:                     filepath.Join(filepath.Dir(path), "cache.db"),
		CredentialSource:          "gcloud",
	}
}

func verifiedWhoami(wid, pid string, input setup.JoinInput) bus.Result {
	return bus.Result{Source: "server", Data: map[string]any{
		"workspace":           map[string]any{"id": wid},
		"project":             map[string]any{"id": pid},
		"principal":           strings.ToLower(input.Principal),
		"identity_provenance": "google-id-token", "verified": true,
		"storage_mode": "cloud", "online": true,
	}}
}

func TestSetupJoinAuthorizesBeforeAtomicTrustedConfigWrite(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	original := []byte("mode = \"cloud\"\nunrelated = \"preserve-me\"\n")
	if err := os.WriteFile(path, original, 0600); err != nil {
		t.Fatal(err)
	}
	input := joinInput(path)
	ctx := context.Background()

	for name, configure := range map[string]func(*joinExecutor){
		"resolve-denied": func(e *joinExecutor) { e.resolveErr = errors.New("forbidden: no membership") },
		"whoami-denied": func(e *joinExecutor) {
			e.resolveID = [2]string{"ws_immutable", "prj_immutable"}
			e.execErr = errors.New("forbidden: no membership")
		},
		"unverified": func(e *joinExecutor) {
			e.resolveID = [2]string{"ws_immutable", "prj_immutable"}
			e.result = bus.Result{Source: "server", Data: map[string]any{
				"workspace": map[string]any{"id": "ws_immutable"},
				"project":   map[string]any{"id": "prj_immutable"},
				"principal": input.Principal, "verified": false,
				"storage_mode": "cloud", "online": true,
			}}
		},
	} {
		t.Run(name, func(t *testing.T) {
			exec := &joinExecutor{}
			configure(exec)
			if _, err := setup.Join(ctx, input, exec); err == nil {
				t.Fatal("failed/denied join succeeded")
			}
			got, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(got, original) {
				t.Fatalf("failed join changed config:\n%s", got)
			}
			for _, operation := range exec.operations() {
				if operation != "whoami" {
					t.Fatalf("join invoked mutating/unexpected operation %q", operation)
				}
			}
		})
	}

	exec := &joinExecutor{resolveID: [2]string{"ws_immutable", "prj_immutable"}}
	exec.result = verifiedWhoami(exec.resolveID[0], exec.resolveID[1], input)
	result, err := setup.Join(ctx, input, exec)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Changed || result.Workspace != "ws_immutable" || result.Project != "prj_immutable" {
		t.Fatalf("join result does not prove immutable scope: %+v", result)
	}
	if got := exec.operations(); len(got) != 1 || got[0] != "whoami" {
		t.Fatalf("successful join operations=%v, want only whoami", got)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, required := range []string{
		`mode = 'cloud'`, `workspace = 'ws_immutable'`, `project = 'prj_immutable'`,
		`server = 'https://bus.example.test'`, `audience = 'https://audience.example.test'`,
		`unrelated = 'preserve-me'`, `session = 'session'`, `instance = 'instance'`,
	} {
		if !strings.Contains(string(raw), required) {
			t.Fatalf("trusted config omits %s:\n%s", required, raw)
		}
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0600 {
		t.Fatalf("trusted config mode=%o, want 600", info.Mode().Perm())
	}

	before := append([]byte(nil), raw...)
	again, err := setup.Join(ctx, input, exec)
	if err != nil {
		t.Fatal(err)
	}
	if again.Changed {
		t.Fatal("identical join rewriting config was reported as changed")
	}
	after, _ := os.ReadFile(path)
	if !bytes.Equal(before, after) {
		t.Fatal("idempotent join rewrote config bytes")
	}
}

func TestSetupADeniedJoinPreservesMalformedAndLocalTrustedConfig(t *testing.T) {
	dir := t.TempDir()
	input := joinInput(filepath.Join(dir, "config.toml"))
	input.ConfigPath = filepath.Join(dir, "config.toml")
	exec := &joinExecutor{resolveID: [2]string{"ws", "prj"}}
	exec.result = verifiedWhoami("ws", "prj", input)
	for name, original := range map[string][]byte{
		"malformed": []byte("not valid = ["),
		"local":     []byte("mode = \"local\"\n"),
	} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(dir, name+".toml")
			input.ConfigPath = path
			if err := os.WriteFile(path, original, 0644); err != nil {
				t.Fatal(err)
			}
			if _, err := setup.Join(context.Background(), input, exec); err == nil {
				t.Fatalf("join accepted %s trusted config", name)
			}
			got, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(got, original) {
				t.Fatalf("failed %s join changed bytes:\n%s", name, got)
			}
			info, _ := os.Stat(path)
			if info.Mode().Perm() != 0644 {
				t.Fatalf("failed join changed config permissions to %o", info.Mode().Perm())
			}
		})
	}
}
