package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
)

type handoverStub struct {
	agents               map[string]bool
	gcloud               [][]string
	gcloudErr            map[string]error
	ran                  []string // bin, dir, prompt
	env                  []string
	installed            []string
	installErr, checkErr error
	checked              []string
	checkConfig          string
}

// stubHandover replaces every outside effect of the hand-over: no real
// agent, gcloud, skill folder or server is touched.
func stubHandover(t *testing.T) *handoverStub {
	t.Helper()
	h := &handoverStub{agents: map[string]bool{"claude": true}, gcloudErr: map[string]error{}}
	a, b, c, d, e := lookAgent, runAgent, runGcloud, installPlaybook, runHandoverChecks
	t.Cleanup(func() { lookAgent, runAgent, runGcloud, installPlaybook, runHandoverChecks = a, b, c, d, e })
	lookAgent = func(bin string) (string, error) {
		if h.agents[bin] {
			return "/usr/bin/" + bin, nil
		}
		return "", exec.ErrNotFound
	}
	runAgent = func(_ context.Context, bin, dir string, env []string, prompt string) error {
		h.ran, h.env = []string{bin, dir, prompt}, env
		return nil
	}
	runGcloud = func(_ context.Context, args ...string) ([]byte, error) {
		h.gcloud = append(h.gcloud, args)
		key := strings.Join(args, " ")
		if args[0] == "config" && args[1] == "get" {
			return []byte("person@example.test\n"), nil
		}
		for prefix, err := range h.gcloudErr {
			if strings.HasPrefix(key, prefix) {
				return nil, err
			}
		}
		return nil, nil
	}
	installPlaybook = func(agent string) error {
		h.installed = append(h.installed, agent)
		return h.installErr
	}
	runHandoverChecks = func(_ context.Context, args []string, _, _ io.Writer) error {
		h.checked = args
		h.checkConfig = os.Getenv("CLOUDSDK_ACTIVE_CONFIG_NAME")
		return h.checkErr
	}
	return h
}

// Defaults: account from gcloud, zone REGION-b, owner = account, bucket
// PROJECT-clawbridge-backups.
const handoverInputs = "3\n\nexample-project\neurope-west1\n\nbridge.example.test\n\n\n\n"

func readState(t *testing.T, home string) onboardingState {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(home, "state", "onboarding", "onboarding.json"))
	if err != nil {
		t.Fatal(err)
	}
	var s onboardingState
	if err = json.Unmarshal(raw, &s); err != nil {
		t.Fatal(err)
	}
	return s
}

func TestHandoverRunsTheAgentThenTheChecks(t *testing.T) {
	home := isolated(t)
	wizardHooks(t, handoverInputs+"y\n")
	h := stubHandover(t)
	var out bytes.Buffer
	if err := Run(context.Background(), []string{"onboard"}, &out, io.Discard); err != nil {
		t.Fatal(err, out.String())
	}
	dir := filepath.Join(home, "state", "onboarding", "provision")
	if !reflect.DeepEqual(h.ran, []string{"claude", dir, "Read TASK.md in this directory and follow it."}) {
		t.Fatalf("agent: %v", h.ran)
	}
	if !reflect.DeepEqual(h.installed, []string{"claude-code"}) {
		t.Fatalf("playbook: %v", h.installed)
	}
	found := false
	for _, e := range h.env {
		found = found || e == "CLOUDSDK_ACTIVE_CONFIG_NAME=clawbridge-onboarding"
	}
	if !found {
		t.Fatal("the agent does not run in the dedicated gcloud configuration")
	}
	wantGcloud := [][]string{
		{"config", "get", "account"},
		{"config", "configurations", "describe", "clawbridge-onboarding"},
		{"--configuration=clawbridge-onboarding", "config", "set", "account", "person@example.test"},
		{"--configuration=clawbridge-onboarding", "config", "set", "project", "example-project"},
		{"--configuration=clawbridge-onboarding", "config", "set", "compute/region", "europe-west1"},
		{"--configuration=clawbridge-onboarding", "config", "set", "compute/zone", "europe-west1-b"},
	}
	if !reflect.DeepEqual(h.gcloud, wantGcloud) {
		t.Fatalf("gcloud: %v", h.gcloud)
	}
	task, err := os.ReadFile(filepath.Join(dir, "TASK.md"))
	if err != nil || !strings.Contains(string(task), "clawbridge-provision-gcp") || !strings.Contains(string(task), "Backup bucket: example-project-clawbridge-backups") {
		t.Fatalf("TASK.md: %s %v", task, err)
	}
	// Windows has no POSIX modes.
	if info, _ := os.Stat(dir); runtime.GOOS != "windows" && info.Mode().Perm() != 0o700 {
		t.Fatalf("work dir mode %v", info.Mode())
	}
	if h.checkConfig != "clawbridge-onboarding" || os.Getenv("CLOUDSDK_ACTIVE_CONFIG_NAME") != "" {
		t.Fatalf("checks ran under configuration %q; after them it is %q", h.checkConfig, os.Getenv("CLOUDSDK_ACTIVE_CONFIG_NAME"))
	}
	if !reflect.DeepEqual(h.checked, []string{"infra", "server", "version", "--server", "https://bridge.example.test", "--vm", "clawbridge-1", "--project", "example-project", "--zone", "europe-west1-b"}) {
		t.Fatalf("checks: %v", h.checked)
	}
	s := readState(t, home)
	if s.Step != "signin" || s.Owner != "person@example.test" || s.Size != "e2-small" {
		t.Fatalf("state: %+v", s)
	}
	if info, _ := os.Stat(filepath.Join(home, "state", "onboarding", "onboarding.json")); runtime.GOOS != "windows" && info.Mode().Perm() != 0o600 {
		t.Fatalf("state mode %v", info.Mode())
	}
	if !strings.Contains(out.String(), "real controls") || !strings.Contains(out.String(), "cloud check workspace") {
		t.Fatal(out.String())
	}
}

// A failed check leaves the step to hand over again; the agent's own word
// counts for nothing.
func TestHandoverFailedCheckKeepsTheStep(t *testing.T) {
	home := isolated(t)
	wizardHooks(t, handoverInputs+"y\n")
	h := stubHandover(t)
	h.checkErr = errors.New("cloud check failed")
	var out bytes.Buffer
	if err := Run(context.Background(), []string{"onboard"}, &out, io.Discard); err != nil {
		t.Fatal(err)
	}
	if s := readState(t, home); s.Step != "provision" || !strings.Contains(out.String(), "Not done yet") {
		t.Fatalf("%+v %s", s, out.String())
	}
	// Resuming goes straight to the hand-over with the saved choices.
	wizardHooks(t, "3\n\ny\n")
	h2 := stubHandover(t)
	out.Reset()
	if err := Run(context.Background(), []string{"onboard"}, &out, io.Discard); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "Saved progress: project example-project") || len(h2.ran) == 0 {
		t.Fatalf("resume: %s %v", out.String(), h2.ran)
	}
}

func TestHandoverDeclinedOrRefused(t *testing.T) {
	// Declining shows the written steps and runs nothing.
	isolated(t)
	wizardHooks(t, handoverInputs+"n\n")
	h := stubHandover(t)
	var out bytes.Buffer
	if err := Run(context.Background(), []string{"onboard"}, &out, io.Discard); err != nil || h.ran != nil || h.installed != nil || !strings.Contains(out.String(), ownServerText) {
		t.Fatalf("%v %v %s", err, h.ran, out.String())
	}
	// A bad input is refused before anything is saved or run.
	home := isolated(t)
	wizardHooks(t, "3\n\nexample-project\neurope-west1\nus-east1-b\nbridge.example.test\n\n\n\n")
	h = stubHandover(t)
	if err := Run(context.Background(), []string{"onboard"}, io.Discard, io.Discard); err == nil || !strings.Contains(err.Error(), "zone must be in the region") || h.ran != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(home, "state", "onboarding")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("refused inputs were saved")
	}
	// The playbook or gcloud failing stops before the agent starts.
	for name, edit := range map[string]func(*handoverStub){
		"playbook refused": func(h *handoverStub) { h.installErr = errors.New("refused") },
		"no configuration": func(h *handoverStub) { h.gcloudErr["config configurations"] = errors.New("x") },
		"account not set": func(h *handoverStub) {
			h.gcloudErr["--configuration=clawbridge-onboarding config set account"] = errors.New("x")
		},
	} {
		isolated(t)
		wizardHooks(t, handoverInputs+"y\n")
		h = stubHandover(t)
		edit(h)
		if err := Run(context.Background(), []string{"onboard"}, io.Discard, io.Discard); err == nil || h.ran != nil {
			t.Fatalf("%s: %v %v", name, err, h.ran)
		}
	}
}

// Codex is offered when both are installed, and chosen by "2".
func TestHandoverOffersCodex(t *testing.T) {
	isolated(t)
	wizardHooks(t, handoverInputs+"2\ny\n")
	h := stubHandover(t)
	h.agents["codex"] = true
	if err := Run(context.Background(), []string{"onboard"}, io.Discard, io.Discard); err != nil || h.ran[0] != "codex" || h.installed[0] != "codex" {
		t.Fatalf("%v %v %v", err, h.ran, h.installed)
	}
}

func TestValidInputs(t *testing.T) {
	ok := onboardingState{Account: "a@example.test", Project: "example-project", Region: "europe-west1", Zone: "europe-west1-b", Domain: "bridge.example.test", Owner: "o@example.test", Size: "e2-small", Bucket: "example-project-clawbridge-backups"}
	if err := validInputs(&ok); err != nil {
		t.Fatal(err)
	}
	for name, edit := range map[string]func(*onboardingState){
		"account":        func(s *onboardingState) { s.Account = "nobody" },
		"project":        func(s *onboardingState) { s.Project = "Example" },
		"region":         func(s *onboardingState) { s.Region = "europe" },
		"zone format":    func(s *onboardingState) { s.Zone = "europe-west1" },
		"zone elsewhere": func(s *onboardingState) { s.Zone = "us-east1-b" },
		"domain":         func(s *onboardingState) { s.Domain = "https://bridge.example.test" },
		"owner":          func(s *onboardingState) { s.Owner = "o@" },
		"size":           func(s *onboardingState) { s.Size = "n2-standard-64" },
		"bucket":         func(s *onboardingState) { s.Bucket = "gs://x" },
	} {
		s := ok
		edit(&s)
		if validInputs(&s) == nil {
			t.Fatalf("%s accepted", name)
		}
	}
}

// A provisioned server resumes at sign-in: no second hand-over.
func TestHandoverResumesAtSignin(t *testing.T) {
	isolated(t)
	if err := saveOnboarding(&onboardingState{Cloud: "gcp", Step: "signin", Account: "a@example.test", Project: "example-project", Region: "europe-west1", Zone: "europe-west1-b", Domain: "bridge.example.test", Owner: "o@example.test", Size: "e2-small", Bucket: "example-backups"}); err != nil {
		t.Fatal(err)
	}
	wizardHooks(t, "3\n\n")
	h := stubHandover(t)
	var out bytes.Buffer
	if err := Run(context.Background(), []string{"onboard"}, &out, io.Discard); err != nil {
		t.Fatal(err)
	}
	if h.ran != nil || h.installed != nil || !strings.Contains(out.String(), "--bucket example-backups --project example-project") {
		t.Fatalf("%v %s", h.ran, out.String())
	}
}

// A saved file is checked again on resume: its values reach argv.
func TestHandoverRefusesInvalidSavedProgress(t *testing.T) {
	isolated(t)
	if err := saveOnboarding(&onboardingState{Cloud: "gcp", Step: "provision", Account: "a@example.test", Project: "example-project", Region: "europe-west1", Zone: "europe-west1-b", Domain: "bridge.example.test --flag", Owner: "o@example.test", Size: "e2-small", Bucket: "example-backups"}); err != nil {
		t.Fatal(err)
	}
	wizardHooks(t, "3\n\n")
	h := stubHandover(t)
	if err := Run(context.Background(), []string{"onboard"}, io.Discard, io.Discard); err == nil || !strings.Contains(err.Error(), "start over") || h.ran != nil || h.gcloud != nil {
		t.Fatalf("%v %v %v", err, h.ran, h.gcloud)
	}
}
