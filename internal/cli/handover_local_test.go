package cli

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
)

// Start's third choice, then "This machine or my LAN", then the shape.
const (
	localSoloInputs = "3\n3\n\nowner@example.test\n\n"
	localLANInputs  = "3\n3\n2\nowner@example.test\nteam_a\nclawbridge.lan\n192.0.2.10\n"
	localSoloUp     = "sh clawbridge/deploy/local-host.sh up --owner owner@example.test --workspace my-team </dev/null"
	localLANUp      = "sh clawbridge/deploy/local-host.sh up --lan clawbridge.lan --bind 192.0.2.10 --owner owner@example.test --workspace team_a </dev/null"
)

var (
	localGate  = []string{"server", "version", "--local", "--server", "http://127.0.0.1:8443"}
	localTrust = []string{"trust", "--local"}
)

// localHost runs the local path as on Linux or macOS.
func localHost(t *testing.T) {
	t.Helper()
	old := localGOOS
	localGOOS = "linux"
	t.Cleanup(func() { localGOOS = old })
}

func localScript(home string) string {
	return "sh '" + filepath.Join(home, "state", "onboarding", "provision", "clawbridge", "deploy", "local-host.sh") + "'"
}

func TestLocalHandoverSolo(t *testing.T) {
	home := isolated(t)
	localHost(t)
	withVersion(t, "v0.3.10")
	wizardHooks(t, localSoloInputs+"y\n")
	h := stubHandover(t)
	scrubbed := secretsInEnv(t)
	var out bytes.Buffer
	if err := Run(context.Background(), []string{"onboard"}, &out, io.Discard); err != nil {
		t.Fatal(err, out.String())
	}
	for _, want := range []string{"Where? 1. Google Cloud 2. AWS 3. This machine or my LAN (no cloud) 4. A server I already have (SSH and Docker) [1]: ",
		"Who uses it? 1. Just me, on this machine 2. My team, on our LAN [1]: ", "Workspace name [my-team]: ",
		"clawbridge-provision-local", "It creates no cloud resources and costs nothing"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("missing %q:\n%s", want, out.String())
		}
	}
	if strings.Contains(out.String(), "LAN host") {
		t.Fatal("solo asked for the LAN host")
	}
	dir := filepath.Join(home, "state", "onboarding", "provision")
	if !reflect.DeepEqual(h.installedCloud, []string{"local"}) || !reflect.DeepEqual(h.installed, []string{"claude-code"}) {
		t.Fatalf("playbook: %v %v", h.installedCloud, h.installed)
	}
	if !reflect.DeepEqual(h.ran, []string{"claude", dir, "Read TASK.md in this directory and follow it."}) {
		t.Fatalf("agent: %v", h.ran)
	}
	scrubbed(h.env)
	task, err := os.ReadFile(filepath.Join(dir, "TASK.md"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"    " + localSoloUp + "\n", "git clone --depth 1 --branch v0.3.10 https://github.com/jkai-chen/ClawBridge clawbridge",
		"clawbridge cloud check server version --local --server http://127.0.0.1:8443\n", "Server URL: http://127.0.0.1:8443", "clawbridge-provision-local"} {
		if !strings.Contains(string(task), want) {
			t.Fatalf("TASK.md misses %q:\n%s", want, task)
		}
	}
	if strings.Contains(string(task), "trust") || strings.Contains(string(task), localSentinel) {
		t.Fatalf("TASK.md: %s", task)
	}
	// Shape A: the gate only, no trust run.
	if !reflect.DeepEqual(h.checks, [][]string{localGate}) {
		t.Fatalf("checks: %v", h.checks)
	}
	s := readState(t, home)
	if s != (onboardingState{Version: "v0.3.10", Cloud: "local", Step: "signin", Shape: "solo", Owner: "owner@example.test", Workspace: "my-team"}) {
		t.Fatalf("state: %+v", s)
	}
	if info, _ := os.Stat(filepath.Join(home, "state", "onboarding", "onboarding.json")); runtime.GOOS != "windows" && info.Mode().Perm() != 0o600 {
		t.Fatalf("state mode %v", info.Mode())
	}
	// The sign-in steps, with this machine's paths quoted for a shell, and no
	// trust step or cloud bootstrap.
	for _, want := range []string{localScript(home) + " invite-owner --workspace 'my-team'",
		"clawbridge join --server 'http://127.0.0.1:8443' --email 'owner@example.test'", "run clawbridge onboard again"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("missing %q:\n%s", want, out.String())
		}
	}
	if strings.Contains(out.String(), " trust\n") || strings.Contains(out.String(), "bootstrap") {
		t.Fatal(out.String())
	}
}

// Shape B: the gate checks the server on 127.0.0.1:8443; then trust runs for
// information only, and its untrusted_ca leads into the sign-in steps.
func TestLocalHandoverLANUntrustedGoesOnToSignin(t *testing.T) {
	home := isolated(t)
	localHost(t)
	withVersion(t, "v0.3.10")
	wizardHooks(t, localLANInputs+"y\n")
	h := stubHandover(t)
	h.checkFn = func(args []string) error {
		if args[0] == "trust" {
			return errors.New("cloud check failed")
		}
		return nil
	}
	var out bytes.Buffer
	if err := Run(context.Background(), []string{"onboard"}, &out, io.Discard); err != nil {
		t.Fatal(err, out.String())
	}
	task, _ := os.ReadFile(filepath.Join(home, "state", "onboarding", "provision", "TASK.md"))
	for _, want := range []string{"    " + localLANUp + "\n", "--branch v0.3.10",
		"    clawbridge cloud check server version --local --server http://127.0.0.1:8443\n", "    clawbridge cloud check trust --local\n",
		"untrusted_ca", "dns", "Server URL: https://clawbridge.lan", "LAN address to publish on: 192.0.2.10"} {
		if !strings.Contains(string(task), want) {
			t.Fatalf("TASK.md misses %q:\n%s", want, task)
		}
	}
	if !reflect.DeepEqual(h.checks, [][]string{localGate, localTrust}) {
		t.Fatalf("checks: %v", h.checks)
	}
	s := readState(t, home)
	if s.Step != "signin" || s.Shape != "lan" || s.Domain != "clawbridge.lan" || s.Bind != "192.0.2.10" || s.Workspace != "team_a" {
		t.Fatalf("state: %+v", s)
	}
	script := localScript(home)
	trust, invite := strings.Index(out.String(), script+" trust\n"), strings.Index(out.String(), script+" invite-owner --workspace 'team_a'")
	if trust < 0 || invite < trust || !strings.Contains(out.String(), "clawbridge join --server 'https://clawbridge.lan' --email 'owner@example.test'") ||
		!strings.Contains(out.String(), "untrusted_ca here is expected until you trust the server's root: the first sign-in step below") ||
		!strings.Contains(out.String(), "If clawbridge.lan doesn't resolve on a computer, point it at 192.0.2.10: in your router's DNS, or with the hosts-file entry 192.0.2.10 clawbridge.lan") ||
		strings.Contains(out.String(), "Not done yet") {
		t.Fatal(out.String())
	}
}

// A failing gate leaves the step to hand over again, and in shape B trust
// never runs.
func TestLocalHandoverFailedCheckKeepsTheStep(t *testing.T) {
	for name, inputs := range map[string]string{"solo": localSoloInputs, "lan": localLANInputs} {
		home := isolated(t)
		localHost(t)
		wizardHooks(t, inputs+"y\n")
		h := stubHandover(t)
		h.checkErr = errors.New("cloud check failed")
		var out bytes.Buffer
		if err := Run(context.Background(), []string{"onboard"}, &out, io.Discard); err != nil {
			t.Fatal(name, err)
		}
		if !reflect.DeepEqual(h.checks, [][]string{localGate}) {
			t.Fatalf("%s: checks %v", name, h.checks)
		}
		if s := readState(t, home); s.Step != "provision" || !strings.Contains(out.String(), "Not done yet") ||
			!strings.Contains(out.String(), "docs/operations/local-host.md") || strings.Contains(out.String(), "invite-owner") {
			t.Fatalf("%s: %+v %s", name, s, out.String())
		}
	}
}

// A development build has no release tag: the agent stops and asks.
func TestLocalTaskOnADevBuild(t *testing.T) {
	withVersion(t, "dev")
	task := localTaskText(&onboardingState{Cloud: "local", Shape: "solo", Owner: "owner@example.test", Workspace: "my-team"})
	if strings.Contains(task, "git clone") || !strings.Contains(task, "development build") || !strings.Contains(task, "ask the user for a release tag") ||
		!strings.Contains(task, localSoloUp) {
		t.Fatal(task)
	}
	for _, v := range []string{"v0.3", "0.3.10", "v0.3.10-rc1", "v01.2.3"} {
		withVersion(t, v)
		if strings.Contains(localTaskText(&onboardingState{Cloud: "local", Shape: "solo"}), "git clone") {
			t.Fatalf("%s taken for a release", v)
		}
	}
}

func TestLocalSignedIn(t *testing.T) {
	isolated(t)
	path := filepath.Join(t.TempDir(), "config.toml")
	solo := &onboardingState{Cloud: "local", Shape: "solo"}
	lan := &onboardingState{Cloud: "local", Shape: "lan", Domain: "clawbridge.lan", Bind: "192.0.2.10"}
	for server, want := range map[string][2]bool{
		"http://127.0.0.1:8443":   {true, false},
		"http://127.0.0.1:8443/":  {true, false},
		"https://clawbridge.lan":  {false, true},
		"https://127.0.0.1:8443":  {false, false},
		"http://clawbridge.lan":   {false, false},
		"https://other.lan":       {false, false},
		"http://127.0.0.1:9443":   {false, false},
		"https://clawbridge.lan/": {false, true},
	} {
		if err := writeConfigValues(path, map[string]any{"mode": "cloud", "server": server, "workspace": "my-team"}); err != nil {
			t.Fatal(err)
		}
		if got := [2]bool{signedIn(path, solo), signedIn(path, lan)}; got != want {
			t.Fatalf("%s: %v", server, got)
		}
	}
	// No workspace yet: not signed in.
	if err := writeConfigValues(path, map[string]any{"mode": "cloud", "server": "http://127.0.0.1:8443"}); err != nil {
		t.Fatal(err)
	}
	if signedIn(path, solo) {
		t.Fatal("signed in without a workspace")
	}
}

// Saved local progress is checked again on resume; a tampered value never
// reaches TASK.md.
func TestLocalResume(t *testing.T) {
	isolated(t)
	localHost(t)
	ok := onboardingState{Cloud: "local", Step: "provision", Shape: "lan", Owner: "owner@example.test", Workspace: "my-team", Domain: "clawbridge.lan", Bind: "192.0.2.10"}
	if err := saveOnboarding(&ok); err != nil {
		t.Fatal(err)
	}
	wizardHooks(t, "3\n\ny\n")
	h := stubHandover(t)
	var out bytes.Buffer
	if err := Run(context.Background(), []string{"onboard"}, &out, io.Discard); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "Saved progress: LAN host clawbridge.lan on 192.0.2.10, step provision") || len(h.ran) == 0 ||
		strings.Contains(out.String(), "Who uses it") {
		t.Fatalf("resume: %s %v", out.String(), h.ran)
	}
	for name, edit := range map[string]func(*onboardingState){
		"host flag":     func(s *onboardingState) { s.Domain = "clawbridge.lan --flag" },
		"bind":          func(s *onboardingState) { s.Bind = "0.0.0.0" },
		"workspace":     func(s *onboardingState) { s.Workspace = "My Team" },
		"owner":         func(s *onboardingState) { s.Owner = "owner@example.test; rm" },
		"shape":         func(s *onboardingState) { s.Shape = "cluster" },
		"solo with LAN": func(s *onboardingState) { s.Shape = "solo" },
	} {
		isolated(t)
		s := ok
		edit(&s)
		if err := saveOnboarding(&s); err != nil {
			t.Fatal(err)
		}
		wizardHooks(t, "3\n\ny\n")
		h := stubHandover(t)
		if err := Run(context.Background(), []string{"onboard"}, io.Discard, io.Discard); err == nil || !strings.Contains(err.Error(), "start over") || h.ran != nil || h.installed != nil {
			t.Fatalf("%s: %v %v", name, err, h.ran)
		}
	}
	// A server already up resumes at sign-in: no second hand-over.
	home := isolated(t)
	s := ok
	s.Step, s.Shape, s.Domain, s.Bind = "signin", "solo", "", ""
	if err := saveOnboarding(&s); err != nil {
		t.Fatal(err)
	}
	wizardHooks(t, "3\n\n")
	h = stubHandover(t)
	out.Reset()
	if err := Run(context.Background(), []string{"onboard"}, &out, io.Discard); err != nil {
		t.Fatal(err)
	}
	if h.ran != nil || h.installed != nil || !strings.Contains(out.String(), "Saved progress: this machine, solo, step signin") ||
		!strings.Contains(out.String(), localScript(home)+" invite-owner --workspace 'my-team'") {
		t.Fatalf("%v %s", h.ran, out.String())
	}
}

// On native Windows the local choice is refused before its questions, and
// saved local progress before the agent starts.
func TestLocalRefusesWindows(t *testing.T) {
	old := localGOOS
	localGOOS = "windows"
	t.Cleanup(func() { localGOOS = old })
	home := isolated(t)
	wizardHooks(t, "3\n3\n")
	h := stubHandover(t)
	var out bytes.Buffer
	if err := Run(context.Background(), []string{"onboard"}, &out, io.Discard); err == nil || !strings.Contains(err.Error(), "run clawbridge onboard inside WSL, where deploy/local-host.sh runs") ||
		strings.Contains(out.String(), "Who uses it") || h.ran != nil {
		t.Fatalf("%v %s", err, out.String())
	}
	if _, err := os.Stat(filepath.Join(home, "state", "onboarding")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("progress saved")
	}
	isolated(t)
	if err := saveOnboarding(&onboardingState{Cloud: "local", Step: "provision", Shape: "solo", Owner: "owner@example.test", Workspace: "my-team"}); err != nil {
		t.Fatal(err)
	}
	wizardHooks(t, "3\n\ny\n")
	h = stubHandover(t)
	if err := Run(context.Background(), []string{"onboard"}, io.Discard, io.Discard); err == nil || !strings.Contains(err.Error(), "WSL") || h.ran != nil {
		t.Fatal(err)
	}
}

// Without an agent, the written steps name the local-host guide.
func TestLocalNoAgentNamesTheGuide(t *testing.T) {
	isolated(t)
	wizardHooks(t, "3\n")
	h := stubHandover(t)
	h.agents = nil
	var out bytes.Buffer
	if err := Run(context.Background(), nil, &out, io.Discard); err != nil ||
		!strings.Contains(out.String(), "On this machine or your LAN instead (no cloud): see docs/operations/local-host.md (deploy/local-host.sh up).") {
		t.Fatal(err, out.String())
	}
	if !strings.Contains(startText, "this machine, or your own server).") || !strings.Contains(startCommands, "docs/operations/local-host.md") {
		t.Fatal(startText + startCommands)
	}
}

func TestLocalInputValidation(t *testing.T) {
	ok := onboardingState{Cloud: "local", Shape: "lan", Owner: "owner@example.test", Workspace: "my-team", Domain: "clawbridge.lan", Bind: "192.0.2.10"}
	old := localGOOS
	localGOOS = "linux"
	t.Cleanup(func() { localGOOS = old })
	for _, edit := range []func(*onboardingState){
		func(*onboardingState) {},
		func(s *onboardingState) { s.Domain = "192.0.2.10" },
		func(s *onboardingState) { s.Domain = "ClawBridge-1.lan" },
		func(s *onboardingState) { s.Domain = strings.Repeat("a", 253) },
		func(s *onboardingState) { s.Workspace = "0team_a-b" },
		func(s *onboardingState) { s.Workspace = strings.Repeat("a", 63) },
		func(s *onboardingState) { s.Shape, s.Domain, s.Bind = "solo", "", "" },
	} {
		s := ok
		edit(&s)
		if err := validInputs(&s); err != nil {
			t.Fatalf("%+v: %v", s, err)
		}
	}
	for name, edit := range map[string]func(*onboardingState){
		"host leading dot":     func(s *onboardingState) { s.Domain = ".clawbridge.lan" },
		"host leading dash":    func(s *onboardingState) { s.Domain = "-clawbridge.lan" },
		"host trailing dot":    func(s *onboardingState) { s.Domain = "clawbridge.lan." },
		"host double dot":      func(s *onboardingState) { s.Domain = "clawbridge..lan" },
		"host space":           func(s *onboardingState) { s.Domain = "clawbridge lan" },
		"host too long":        func(s *onboardingState) { s.Domain = strings.Repeat("a", 254) },
		"host empty":           func(s *onboardingState) { s.Domain = "" },
		"bind every address":   func(s *onboardingState) { s.Bind = "0.0.0.0" },
		"bind loopback":        func(s *onboardingState) { s.Bind = "127.0.0.1" },
		"bind not IPv4":        func(s *onboardingState) { s.Bind = "2001:db8::1" },
		"bind mapped":          func(s *onboardingState) { s.Bind = "::ffff:192.0.2.10" },
		"bind name":            func(s *onboardingState) { s.Bind = "clawbridge.lan" },
		"bind short":           func(s *onboardingState) { s.Bind = "192.0.2" },
		"workspace upper":      func(s *onboardingState) { s.Workspace = "My-team" },
		"workspace dot":        func(s *onboardingState) { s.Workspace = "my.team" },
		"workspace leading _":  func(s *onboardingState) { s.Workspace = "_team" },
		"workspace too long":   func(s *onboardingState) { s.Workspace = strings.Repeat("a", 64) },
		"owner":                func(s *onboardingState) { s.Owner = "owner" },
		"unknown shape":        func(s *onboardingState) { s.Shape = "" },
		"solo with a LAN host": func(s *onboardingState) { s.Shape, s.Bind = "solo", "" },
	} {
		s := ok
		edit(&s)
		if validInputs(&s) == nil {
			t.Fatalf("%s accepted", name)
		}
	}
	// As typed: a wrong answer is asked again with the reason, and three
	// stop before anything is saved.
	home := isolated(t)
	wizardHooks(t, "3\n3\n2\nowner@example.test\n\n.clawbridge.lan\nclawbridge..lan\nclawbridge lan\n")
	h := stubHandover(t)
	var out bytes.Buffer
	if err := Run(context.Background(), []string{"onboard"}, &out, io.Discard); err == nil || !strings.Contains(err.Error(), "no '..'") || h.ran != nil ||
		strings.Count(out.String(), "Try again.") != 2 {
		t.Fatalf("%v %s", err, out.String())
	}
	if _, err := os.Stat(filepath.Join(home, "state", "onboarding")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("refused inputs were saved")
	}
	isolated(t)
	wizardHooks(t, "3\n3\n2\nowner@example.test\n\nclawbridge.lan\n0.0.0.0\n127.0.0.1\nlan\n")
	h = stubHandover(t)
	if err := Run(context.Background(), []string{"onboard"}, io.Discard, io.Discard); err == nil || !strings.Contains(err.Error(), "not 0.0.0.0 or 127.0.0.1") || h.ran != nil {
		t.Fatal(err)
	}
	isolated(t)
	wizardHooks(t, "3\n3\n\nowner@example.test\nMy Team\nmy.team\n-team\n")
	h = stubHandover(t)
	if err := Run(context.Background(), []string{"onboard"}, io.Discard, io.Discard); err == nil || !strings.Contains(err.Error(), "workspace name") || h.ran != nil {
		t.Fatal(err)
	}
}

// The up command never runs with a terminal: the sign-in code stays out of
// the agent.
func TestLocalUpRunsWithoutATerminal(t *testing.T) {
	for _, s := range []*onboardingState{
		{Cloud: "local", Shape: "solo", Owner: "owner@example.test", Workspace: "my-team"},
		{Cloud: "local", Shape: "lan", Owner: "owner@example.test", Workspace: "my-team", Domain: "clawbridge.lan", Bind: "192.0.2.10"},
	} {
		task := localTaskText(s)
		if !strings.Contains(task, "    sh clawbridge/deploy/local-host.sh "+localUpArgs(s)+" </dev/null\n") || !strings.Contains(task, "never in an interactive terminal") {
			t.Fatal(task)
		}
	}
}

// "n" to a local hand-over prints the local steps with the user's values,
// not the cloud steps.
func TestLocalDeclinedShowsTheLocalSteps(t *testing.T) {
	isolated(t)
	localHost(t)
	withVersion(t, "v0.3.10")
	wizardHooks(t, localLANInputs+"n\n")
	h := stubHandover(t)
	var out bytes.Buffer
	if err := Run(context.Background(), []string{"onboard"}, &out, io.Discard); err != nil || h.ran != nil || h.installed != nil {
		t.Fatalf("%v %v", err, h.ran)
	}
	for _, want := range []string{"git clone --depth 1 --branch v0.3.10 https://github.com/jkai-chen/ClawBridge clawbridge",
		"sh clawbridge/deploy/local-host.sh up --lan clawbridge.lan --bind 192.0.2.10 --owner owner@example.test --workspace team_a",
		"clawbridge cloud check server version --local --server http://127.0.0.1:8443", "sh clawbridge/deploy/local-host.sh trust",
		"invite-owner --workspace team_a", "clawbridge join --server https://clawbridge.lan --email owner@example.test",
		"docs/operations/local-host.md"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("missing %q:\n%s", want, out.String())
		}
	}
	if strings.Contains(out.String(), ownServerText) || strings.Contains(out.String(), "Provision the VM") {
		t.Fatal(out.String())
	}
}

// The LAN host is saved in lower case, typed or resumed, so signedIn compares
// one form; local-host.sh's rule itself takes either case.
func TestLocalHostIsLowercased(t *testing.T) {
	home := isolated(t)
	localHost(t)
	if !validLocalHost("ClawBridge-1.lan") {
		t.Fatal("upper case refused: valid_host allows it")
	}
	wizardHooks(t, "3\n3\n2\nowner@example.test\n\nClawBridge-1.lan\n192.0.2.10\nn\n")
	stubHandover(t)
	if err := Run(context.Background(), []string{"onboard"}, io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}
	s := readState(t, home)
	if s.Domain != "clawbridge-1.lan" {
		t.Fatalf("saved %q", s.Domain)
	}
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := writeConfigValues(path, map[string]any{"mode": "cloud", "server": "https://clawbridge-1.lan", "workspace": "my-team"}); err != nil {
		t.Fatal(err)
	}
	if !signedIn(path, &s) {
		t.Fatal("not signed in")
	}
	// Saved in upper case (by hand): lowered on resume.
	r := onboardingState{Cloud: "local", Shape: "lan", Owner: "owner@example.test", Workspace: "my-team", Domain: "ClawBridge-1.LAN", Bind: "192.0.2.10"}
	if err := validInputs(&r); err != nil || r.Domain != "clawbridge-1.lan" || !signedIn(path, &r) {
		t.Fatalf("%v %q", err, r.Domain)
	}
}
