package cli

import (
	"bytes"
	"context"
	"errors"
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
	// Skill yes; Linear yes as "Work" with default read access; Claude Code
	// yes as session "eng" and written; Codex no.
	o := newOnboardingRun(t, "y\ny\nWork\n\ny\neng\ny\nn\n")
	if err := o.run(); err != nil {
		t.Fatal(err, o.out.String())
	}
	o.want("keyring", "join", "skill", "linear:Work:read", "doctor:lin_synthetic:",
		"mcp-preview:claude-code:eng:lin_synthetic", "mcp-write:claude-code:eng:lin_synthetic",
		"doctor:lin_synthetic:--mcp-agent claude-code --mcp-config /synthetic/claude-code.json --mcp-session eng", "tui")
	out := o.out.String()
	for _, want := range []string{
		"- Agent skill: installed",
		"- Linear: connected (read access, profile lin_synthetic); capabilities pass",
		"- MCP for Claude Code: configured in /synthetic/claude-code.json",
		"- MCP for Codex: skipped",
		"later: clawbridge mcp install --agent codex --session NAME --linear-profile lin_synthetic --dry-run",
		"- Doctor: warn (warn Agent skill)",
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
