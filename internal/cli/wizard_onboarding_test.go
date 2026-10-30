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

	"clawbridge/internal/clirender"
	"clawbridge/internal/doctor"
	"clawbridge/internal/install"
	"clawbridge/internal/linearconn"
	"clawbridge/internal/remote"
	"clawbridge/internal/skill"
	"github.com/muesli/cancelreader"
)

// joinAnswers join with an invite: the URL, the email, the default device
// name and a yes; the code is read apart.
const joinAnswers = "https://service.test\nada@example.com\n\ny\n"

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
	return newOnboardingRunAfter(t, joinAnswers, answers)
}

// newOnboardingRunAfter is newOnboardingRun with its own join answers.
func newOnboardingRunAfter(t *testing.T, join, answers string) *onboardingRun {
	isolated(t)
	o := &onboardingRun{t: t, path: filepath.Join(t.TempDir(), "client.toml"), failOn: map[string]bool{}}
	o.r = wizardHooks(t, join+answers)
	opened := wizardInput
	wizardInput = func() (cancelreader.CancelReader, error) { o.r.closed = false; return opened() }
	fail := func(name string) error {
		if o.failOn[name] {
			return errors.New("synthetic " + name + " failure")
		}
		return nil
	}
	wizardMethods = func(context.Context, string) ([]string, error) { return []string{"device"}, nil }
	wizardKeyStore = func(context.Context) error { return nil }
	wizardInviteCode = func() (string, error) { return wizardCode, nil }
	wizardDeviceJoin = func(_ context.Context, p JoinParams) (JoinResult, error) {
		o.calls = append(o.calls, "join")
		route := "mode='cloud'\nserver='" + p.Server + "'\nworkspace='ws'\nprincipal='" + p.Email + "'\ncredential_source='device'\ndevice_id='dev_1'\nkey_store='keyring'\n"
		return JoinResult{DeviceID: "dev_1", Principal: p.Email, Workspace: "ws", KeyStore: p.KeyStore, ConfigPath: p.ConfigPath}, os.WriteFile(p.ConfigPath, []byte(route), 0600)
	}
	wizardProjects = func(context.Context, string) ([]string, error) { return []string{"prj"}, nil }
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
	wizardLinearConnect = func(_ context.Context, path, name, access, store string, _ io.Writer) (string, error) {
		if !o.r.closed {
			t.Fatal("terminal input shared with the Linear sign-in")
		}
		o.calls = append(o.calls, "linear:"+name+":"+access+storeCall(store))
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
	o := newOnboardingRun(t, "Yes\nYES\nWork\n READ \ny\neng\nY\ny\ncx\nyes\n\n")
	if err := o.run(); err != nil {
		t.Fatal(err, o.out.String())
	}
	o.want("join", "skill", "linear:Work:read", "doctor:lin_synthetic:",
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
	o := newOnboardingRun(t, "n\nn\ny\neng\nn\n\n\n")
	if err := o.run(); err != nil {
		t.Fatal(err)
	}
	o.want("join", "mcp-preview:claude-code:eng:", "doctor::", "tui")
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
	o.want("join", "skill", "linear:Linear:write", "mcp-preview:claude-code:eng:", "doctor::")
	for _, want := range []string{"- Agent skill: failed: synthetic skill failure", "- Linear: failed: synthetic linear failure", "- MCP for Claude Code: failed: synthetic mcp-preview failure", "- Doctor: failed: synthetic doctor failure"} {
		if !strings.Contains(o.out.String(), want) {
			t.Fatalf("output lacks %q:\n%s", want, o.out.String())
		}
	}
}

// storeCall marks a Linear connection kept anywhere but the keyring.
func storeCall(store string) string {
	if store == linearconn.StoreKeyring {
		return ""
	}
	return ":" + store
}

// Without a system key store, the Linear step says what a file means and
// keeps the connection in one only on a yes, as the device key's step does.
func TestGuidedOnboardingOffersAFileForLinearWithoutAKeyStore(t *testing.T) {
	for _, c := range []struct {
		answer string
		calls  []string
		want   string
	}{
		{"y", []string{"join", "linear:Work:read:file", "doctor:lin_synthetic:", "doctor:lin_synthetic:"}, "- Linear: connected (read access, profile lin_synthetic)"},
		{"n", []string{"join", "doctor::"}, "- Linear: skipped: no system key store"},
	} {
		// The join keeps its device key in a file too: a yes before the
		// summary's.
		o := newOnboardingRunAfter(t, "https://service.test\nada@example.com\n\ny\ny\n", "n\ny\nWork\nread\n"+c.answer+"\nn\nn\n")
		wizardKeyStore = func(context.Context) error { return errors.New("Linux Secret Service is unavailable") }
		if err := o.run(); err != nil {
			t.Fatal(err)
		}
		o.want(c.calls...)
		out := o.out.String()
		for _, want := range []string{c.want, "This machine has no system key store (Linux Secret Service is unavailable).", linearconn.FileWarning, "Keep the Linear connection in a file? [y/N]"} {
			if !strings.Contains(out, want) {
				t.Fatalf("%s: output lacks %q:\n%s", c.answer, want, out)
			}
		}
		if c.answer == "n" && !strings.Contains(out, "--key-store file") {
			t.Fatalf("the skip doesn't name --key-store file:\n%s", out)
		}
	}
}

func TestGuidedOnboardingRejectsAnUnknownAccessAndSkipsAfterInputEnds(t *testing.T) {
	o := newOnboardingRun(t, "n\ny\nWork\nadmin\ny\n")
	if err := o.run(); err != nil {
		t.Fatal(err)
	}
	// "admin" is neither read nor write; input then ends at the session name.
	o.want("join", "doctor::")
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
	o.want("join", "doctor::")
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
		blocked        string
	}{
		"local":      {path: local, outcome: "skipped: the dispatcher runs on a hosted server"},
		"already on": {path: cloud, enabled: true, outcome: "already on", calls: []string{"show"}},
		"not owner":  {path: cloud, previewErr: errors.New("project owner permission required"), outcome: "skipped: only a project owner", later: "enable --expected-revision 3 --confirm", calls: []string{"show", "enable --engine rules --expected-revision 3"}},
		"unlinked":   {path: cloud, previewErr: errors.New("link the task board before enabling the dispatcher"), outcome: "skipped: link the task board first", later: "--expected-revision 3", calls: []string{"show", "enable --engine rules --expected-revision 3"}},
		"session":    {path: cloud, previewErr: errors.New("the dispatcher is turned on or off by the project owner, not from a session"), outcome: "skipped: turn it on in your own terminal", later: "--expected-revision 3", calls: []string{"show", "enable --engine rules --expected-revision 3"}},
		// A preview that says enabling would be refused is not offered.
		"no domains": {path: cloud, blocked: "no_domains", outcome: "skipped: the task board names no Linear projects", later: "--expected-revision 3", calls: []string{"show", "enable --engine rules --expected-revision 3"}},
		"blocked":    {path: cloud, blocked: "model_projects_over_cap", outcome: "skipped: model_projects_over_cap", calls: []string{"show", "enable --engine rules --expected-revision 3"}},
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
			preview := map[string]any{"preview": true, "mode": "rules", "coordinator_before": "lead", "coordinator_after": "dispatcher", "domains": []any{"all-work"}}
			if c.blocked != "" {
				preview["blocked"] = c.blocked
			}
			return preview, c.previewErr
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
		if c.shownOneLiner != strings.Contains(out.String(), "clawbridge dispatcher disable turns it off any time") || c.shownOneLiner != strings.Contains(out.String(), "lead → dispatcher") ||
			strings.Contains(out.String(), "{") {
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

// JR1: in a real terminal Run picks the human layout; the guided setup's
// doctor step must still get the report as data.
func TestWizardDoctorGetsTheReportInATerminal(t *testing.T) {
	dir := isolated(t)
	t.Setenv("HOME", dir)
	path := filepath.Join(dir, "client.json")
	if err := os.WriteFile(path, []byte("mode = \"local\"\n# A literal string: a Windows path's backslashes are no escapes.\ndb = '"+filepath.Join(dir, "db.sqlite")+"'\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx := clirender.WithMode(context.Background(), clirender.Human) // what Run sets at a terminal
	report, err := wizardDoctor(ctx, path, "", nil)
	if err != nil && strings.Contains(err.Error(), "doctor returned no report") {
		t.Fatal(err)
	}
	if report.Status == "" {
		t.Fatalf("no report status (err %v)", err)
	}
}

// JR2: a member is told the owner sets the dispatcher, not shown a 403.
func TestOnboardingDispatcherForAMember(t *testing.T) {
	dir := t.TempDir()
	cloud := filepath.Join(dir, "cloud.toml")
	if err := os.WriteFile(cloud, []byte("mode = \"cloud\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	oldD := wizardDispatcher
	t.Cleanup(func() { wizardDispatcher = oldD })
	const want = "skipped: the project owner sets the dispatcher"
	run := func(show, preview error) (outcome, later string, calls []string) {
		wizardDispatcher = func(_ context.Context, _ string, args []string) (map[string]any, error) {
			calls = append(calls, args[0])
			if args[0] == "show" {
				return map[string]any{"enabled": false, "revision": 3}, show
			}
			return map[string]any{"preview": true, "mode": "rules"}, preview
		}
		record := func(_, o, l string) { outcome, later = o, l }
		yes := func(string) (bool, error) { return false, nil }
		if err := offerDispatcher(context.Background(), cloud, io.Discard, yes, record, func(e error) string { return "failed: " + e.Error() }); err != nil {
			t.Fatal(err)
		}
		return
	}
	forbidden := fmt.Errorf("dispatcher: %w", &remote.HTTPError{Status: 403, Code: "forbidden"})
	if o, l, calls := run(forbidden, nil); o != want || l != "" || strings.Join(calls, ",") != "show" {
		t.Fatalf("403 on show: %q %q %v", o, l, calls)
	}
	if o, l, calls := run(nil, forbidden); o != want || l != "" || strings.Join(calls, ",") != "show,enable" {
		t.Fatalf("403 on the preview: %q %q %v", o, l, calls)
	}
	if o, _, calls := run(nil, nil); o != "offered, left off" || strings.Join(calls, ",") != "show,enable" {
		t.Fatalf("owner: %q %v", o, calls)
	}
	// An owner inside a session is refused with a bare 403 too: the client
	// knows its session and says so before any call (reviewer, #303).
	t.Setenv("CLAWBRIDGE_SESSION", "worker")
	if o, l, calls := run(forbidden, forbidden); o != "skipped: turn it on in your own terminal with no session" || l != "" || len(calls) != 0 {
		t.Fatalf("in a session: %q %q %v", o, l, calls)
	}
	// A refusal whose text is kept (a local server) still names its reason
	// before the 403 fallback.
	t.Setenv("CLAWBRIDGE_SESSION", "")
	named := fmt.Errorf("dispatcher: %w", &remote.HTTPError{Status: 403, Code: "forbidden", Message: "the dispatcher is turned on or off by the project owner, not from a session"})
	if o, _, _ := run(nil, named); o != "skipped: turn it on in your own terminal with no session" {
		t.Fatalf("named 403: %q", o)
	}
}
