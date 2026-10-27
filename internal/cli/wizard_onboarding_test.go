package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"clawbridge/internal/doctor"
	"clawbridge/internal/install"
	"clawbridge/internal/skill"
	"github.com/muesli/cancelreader"
)

const joinAnswers = "https://service.test\nws\nprj\ndevice\ny\n"

// onboardingRun joins through the wizard with stubbed boundaries and then
// answers the onboarding questions. It returns the boundary calls and output.
type onboardingRun struct {
	t      *testing.T
	r      *wizardReader
	calls  []string
	out    bytes.Buffer
	path   string
	failOn map[string]bool
}

func newOnboardingRun(t *testing.T, answers string) *onboardingRun {
	isolated(t)
	o := &onboardingRun{t: t, path: filepath.Join(t.TempDir(), "client.toml"), failOn: map[string]bool{}}
	o.r = wizardHooks(t, joinAnswers+answers)
	opened := wizardInput
	wizardInput = func() (cancelreader.CancelReader, error) { o.r.closed = false; return opened() }
	fail := func(name string) error {
		if o.failOn[name] {
			return errors.New("synthetic " + name + " failure")
		}
		return nil
	}
	wizardKeyring = func(context.Context) error { o.calls = append(o.calls, "keyring"); return nil }
	wizardJoin = func(_ context.Context, c Config, _ []string, _, _ io.Writer) error {
		o.calls = append(o.calls, "join")
		return os.WriteFile(c.ClientConfig, []byte("published-route"), 0600)
	}
	wizardTUI = func(context.Context, Config, io.Writer, io.Writer) error {
		o.calls = append(o.calls, "tui")
		return nil
	}
	wizardSkillTargets = func() ([]skill.Target, error) {
		return []skill.Target{{Agent: "claude-code", Dir: "/synthetic/claude/skills/clawbridge"}, {Agent: "codex", Dir: "/synthetic/codex/skills/clawbridge"}}, nil
	}
	wizardSkillInstall = func(targets []skill.Target) ([]skill.Result, error) {
		o.calls = append(o.calls, "skill")
		return nil, fail("skill")
	}
	wizardLinearConnect = func(_ context.Context, path, name, access string, _ io.Writer) (string, error) {
		if !o.r.closed {
			t.Fatal("terminal input shared with the Linear sign-in")
		}
		o.calls = append(o.calls, "linear:"+name+":"+access)
		return "lin_synthetic", fail("linear")
	}
	wizardMCPInstall = func(_ context.Context, path, agent, session, profile string, dryRun bool, _ io.Writer) (install.Result, error) {
		mode := "write"
		if dryRun {
			mode = "preview"
		}
		o.calls = append(o.calls, "mcp-"+mode+":"+agent+":"+session+":"+profile)
		return install.Result{Path: "/synthetic/" + agent + ".json", Changed: true, DryRun: dryRun, Entry: map[string]any{"command": "clawbridge"}}, fail("mcp-" + mode)
	}
	wizardDoctor = func(_ context.Context, _, profile string, args []string) (doctor.Report, error) {
		o.calls = append(o.calls, "doctor:"+profile+":"+strings.Join(args, " "))
		return doctor.Report{Status: doctor.Warn, Checks: []doctor.Check{{ID: "linear.capabilities", Title: "Linear capabilities", Status: doctor.Pass}, {ID: "skill.version", Title: "Agent skill", Status: doctor.Warn}}}, fail("doctor")
	}
	return o
}

func (o *onboardingRun) run() error {
	return Run(context.Background(), []string{"--client-config", o.path, "setup", "wizard"}, &o.out, io.Discard)
}

func (o *onboardingRun) want(calls ...string) {
	o.t.Helper()
	if got := strings.Join(o.calls, ","); got != strings.Join(calls, ",") {
		o.t.Fatalf("calls\n got %s\nwant %s", got, strings.Join(calls, ","))
	}
}

func TestGuidedOnboardingRunsEachConfirmedStepInOrder(t *testing.T) {
	// Skill Yes; Linear YES as "Work" with READ access; Claude Code yes as
	// session "eng" and written; Codex yes as "cx" and written. Answers are
	// case-insensitive.
	o := newOnboardingRun(t, "Yes\nYES\nWork\n READ \ny\neng\nY\ny\ncx\nyes\n")
	if err := o.run(); err != nil {
		t.Fatal(err, o.out.String())
	}
	o.want("keyring", "join", "skill", "linear:Work:read", "doctor:lin_synthetic:",
		"mcp-preview:claude-code:eng:lin_synthetic", "mcp-write:claude-code:eng:lin_synthetic",
		"mcp-preview:codex:cx:lin_synthetic", "mcp-write:codex:cx:lin_synthetic",
		"doctor:lin_synthetic:--mcp-agent claude-code --mcp-config /synthetic/claude-code.json --mcp-session eng",
		"doctor:lin_synthetic:--mcp-agent codex --mcp-config /synthetic/codex.json --mcp-session cx", "tui")
	out := o.out.String()
	for _, want := range []string{
		"- Agent skill: installed",
		"- Linear: connected (read access, profile lin_synthetic); capabilities pass",
		"- MCP for Claude Code: configured in /synthetic/claude-code.json",
		"- MCP for Codex: configured in /synthetic/codex.json",
		"- Doctor (Claude Code): warn (warn Agent skill)",
		"- Doctor (Codex): warn (warn Agent skill)",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("output lacks %q:\n%s", want, out)
		}
	}
}

func TestGuidedOnboardingWritesNothingWithoutAYes(t *testing.T) {
	// Every question declined, including a previewed MCP entry.
	o := newOnboardingRun(t, "n\nn\ny\neng\nn\n\n")
	if err := o.run(); err != nil {
		t.Fatal(err)
	}
	o.want("keyring", "join", "mcp-preview:claude-code:eng:", "doctor::", "tui")
	for _, want := range []string{"later: clawbridge skill install", "later: clawbridge linear connect", "later: clawbridge mcp install --agent claude-code", "later: clawbridge mcp install --agent codex"} {
		if !strings.Contains(o.out.String(), want) {
			t.Fatalf("output lacks %q:\n%s", want, o.out.String())
		}
	}
}

func TestGuidedOnboardingReportsFailuresAndContinues(t *testing.T) {
	o := newOnboardingRun(t, "y\ny\n\nwrite\ny\neng\nn\n")
	for _, step := range []string{"skill", "linear", "mcp-preview", "doctor"} {
		o.failOn[step] = true
	}
	if err := o.run(); err != nil {
		t.Fatal(err)
	}
	// A failed Linear connection pins no profile; a failed preview writes nothing.
	o.want("keyring", "join", "skill", "linear:Linear:write", "mcp-preview:claude-code:eng:", "doctor::", "tui")
	for _, want := range []string{"- Agent skill: failed: synthetic skill failure", "- Linear: failed: synthetic linear failure", "- MCP for Claude Code: failed: synthetic mcp-preview failure", "- Doctor: failed: synthetic doctor failure"} {
		if !strings.Contains(o.out.String(), want) {
			t.Fatalf("output lacks %q:\n%s", want, o.out.String())
		}
	}
}

func TestGuidedOnboardingRejectsAnUnknownAccessAndSkipsAfterInputEnds(t *testing.T) {
	o := newOnboardingRun(t, "n\ny\nWork\nadmin\ny\n")
	if err := o.run(); err != nil {
		t.Fatal(err)
	}
	// "admin" is neither read nor write; input then ends at the session name.
	o.want("keyring", "join", "doctor::", "tui")
	for _, want := range []string{"- Linear: skipped: choose read or write", "- MCP for Claude Code: skipped", "- MCP for Codex: skipped", "Input ended; the remaining steps were skipped."} {
		if !strings.Contains(o.out.String(), want) {
			t.Fatalf("output lacks %q:\n%s", want, o.out.String())
		}
	}
}

func TestGuidedOnboardingWithoutAgentsOffersCommands(t *testing.T) {
	o := newOnboardingRun(t, "n\n")
	wizardSkillTargets = func() ([]skill.Target, error) { return nil, errors.New("no agents") }
	if err := o.run(); err != nil {
		t.Fatal(err)
	}
	o.want("keyring", "join", "doctor::", "tui")
	for _, want := range []string{"- Agent skill: skipped: no Claude Code or Codex configuration found", "- MCP: skipped: no Claude Code or Codex configuration found"} {
		if !strings.Contains(o.out.String(), want) {
			t.Fatalf("output lacks %q:\n%s", want, o.out.String())
		}
	}
}

// G5: onboarding's last step offers a hosted project's owner the dispatcher
// in the rules engine, says what it does and how to turn it off, and turns
// it on only on a yes, with the revision it showed. Anything else is a
// skip or a failure with the commands for later; nothing is written.
func TestOnboardingOffersTheDispatcher(t *testing.T) {
	dir := t.TempDir()
	cloud, local := filepath.Join(dir, "cloud.toml"), filepath.Join(dir, "local.toml")
	if err := os.WriteFile(cloud, []byte("mode = \"cloud\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(local, []byte("mode = \"local\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	old := wizardDispatcher
	t.Cleanup(func() { wizardDispatcher = old })
	for name, c := range map[string]struct {
		path           string
		enabled        bool
		previewErr     error
		confirmErr     error
		answer         bool
		outcome, later string
		calls          []string
		shownOneLiner  bool
		revision       any
	}{
		"local":      {path: local, outcome: "skipped: the dispatcher runs on a hosted server"},
		"already on": {path: cloud, enabled: true, outcome: "already on", calls: []string{"show"}},
		"not owner":  {path: cloud, previewErr: errors.New("project owner permission required"), outcome: "skipped: only a project owner", later: "enable --expected-revision 3 --confirm", calls: []string{"show", "enable --engine rules --expected-revision 3"}},
		"unlinked":   {path: cloud, previewErr: errors.New("link the task board before enabling the dispatcher"), outcome: "skipped: link the task board first", later: "--expected-revision 3", calls: []string{"show", "enable --engine rules --expected-revision 3"}},
		"session":    {path: cloud, previewErr: errors.New("the dispatcher is turned on or off by the project owner, not from a session"), outcome: "skipped: turn it on in your own terminal", later: "--expected-revision 3", calls: []string{"show", "enable --engine rules --expected-revision 3"}},
		"declined":   {path: cloud, outcome: "offered, left off", later: "enable --expected-revision 3 --confirm", calls: []string{"show", "enable --engine rules --expected-revision 3"}, shownOneLiner: true},
		"enabled":    {path: cloud, answer: true, outcome: "on (rules engine); turn it off with clawbridge dispatcher disable", later: "model mode", calls: []string{"show", "enable --engine rules --expected-revision 3", "enable --engine rules --expected-revision 3 --confirm"}, shownOneLiner: true},
		"conflict":   {path: cloud, answer: true, confirmErr: errors.New("the dispatcher policy changed: current revision is 4"), outcome: "failed: the dispatcher policy changed", later: "--expected-revision 3 --confirm", calls: []string{"show", "enable --engine rules --expected-revision 3", "enable --engine rules --expected-revision 3 --confirm"}, shownOneLiner: true},
		// A revision read from JSON is a float64: from 1,000,000 up it
		// must still print as an integer.
		"big revision": {path: cloud, revision: float64(1_000_000), answer: true, outcome: "on (rules engine)", later: "model mode", calls: []string{"show", "enable --engine rules --expected-revision 1000000", "enable --engine rules --expected-revision 1000000 --confirm"}, shownOneLiner: true},
	} {
		if c.revision == nil {
			c.revision = 3
		}
		var calls []string
		wizardDispatcher = func(_ context.Context, path string, args []string) (map[string]any, error) {
			if path != c.path {
				t.Fatal(name, "another profile:", path)
			}
			calls = append(calls, strings.Join(args, " "))
			switch {
			case args[0] == "show":
				return map[string]any{"enabled": c.enabled, "revision": c.revision}, nil
			case strings.HasSuffix(strings.Join(args, " "), "--confirm"):
				return map[string]any{"enabled": true}, c.confirmErr
			}
			return map[string]any{"coordinator_before": "lead", "domains": []any{"all-work"}}, c.previewErr
		}
		var out bytes.Buffer
		var outcome, later string
		asked := false
		yes := func(q string) (bool, error) { asked = true; return c.answer, nil }
		record := func(n, o, l string) { outcome, later = o, l }
		failed := func(e error) string { return "failed: " + e.Error() }
		if err := offerDispatcher(context.Background(), c.path, &out, yes, record, failed); err != nil {
			t.Fatal(name, err)
		}
		if !strings.HasPrefix(outcome, c.outcome) || !strings.Contains(later, c.later) || strings.Join(calls, ",") != strings.Join(c.calls, ",") || asked != c.shownOneLiner {
			t.Fatalf("%s: %q later %q calls %v asked %v", name, outcome, later, calls, asked)
		}
		if c.shownOneLiner != strings.Contains(out.String(), "clawbridge dispatcher disable turns it off any time") || c.shownOneLiner != strings.Contains(out.String(), "all-work") {
			t.Fatalf("%s: %s", name, out.String())
		}
	}
	// A cancelled question stops onboarding, as every step's does.
	wizardDispatcher = func(context.Context, string, []string) (map[string]any, error) {
		return map[string]any{"enabled": false, "revision": 0}, nil
	}
	stop := errors.New("cancelled")
	if err := offerDispatcher(context.Background(), cloud, io.Discard, func(string) (bool, error) { return false, stop }, func(string, string, string) {}, func(e error) string { return e.Error() }); !errors.Is(err, stop) {
		t.Fatal(err)
	}
}

// The step's commands, for real on a local store: show reads the policy,
// and the enable preview's refusal for an unlinked task board is one the
// step turns into a skip.
func TestWizardDispatcherRunsTheCommands(t *testing.T) {
	isolated(t)
	path := filepath.Join(t.TempDir(), "client.toml")
	for _, args := range [][]string{{"setup", "--local", "--workspace", "w", "--project", "p"}, {"project", "create", "p"}} {
		if err := Run(context.Background(), append([]string{"--client-config", path}, args...), io.Discard, io.Discard); err != nil {
			t.Fatal(args, err)
		}
	}
	shown, err := runWizardDispatcher(context.Background(), path, []string{"show"})
	if err != nil || shown["enabled"] != false || fmt.Sprint(shown["revision"]) != "0" {
		t.Fatal(shown, err)
	}
	_, err = runWizardDispatcher(context.Background(), path, []string{"enable", "--engine", "rules", "--expected-revision", "0"})
	if err == nil || !strings.Contains(err.Error(), dispatcherSkips[1].text) {
		t.Fatal("the unlinked refusal changed:", err)
	}
}
