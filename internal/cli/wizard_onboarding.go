package cli

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"

	"clawbridge/internal/clirender"
	"clawbridge/internal/doctor"
	"clawbridge/internal/install"
	"clawbridge/internal/linearconn"
	"clawbridge/internal/skill"
	"clawbridge/internal/uiflow/onboarding"
)

// Each onboarding step runs the same code as its explicit command.
var wizardSkillTargets = func() ([]skill.Target, error) { return skill.Targets("", "") }
var wizardSkillInstall = func(targets []skill.Target) ([]skill.Result, error) { return skill.Install(targets, false) }
var wizardLinearConnect = func(ctx context.Context, path, name, access, store string, stderr io.Writer) (string, error) {
	var out bytes.Buffer
	if err := runLinear(ctx, Config{ClientConfig: path}, []string{"connect", "--name", name, "--access", access, "--key-store", store}, &out, stderr); err != nil {
		return "", err
	}
	var result struct {
		Data linearconn.Profile `json:"data"`
	}
	if json.Unmarshal(out.Bytes(), &result) != nil || result.Data.ID == "" {
		return "", errors.New("Linear connection returned no profile")
	}
	return result.Data.ID, nil
}
var wizardMCPInstall = func(ctx context.Context, path, agent, session, profile string, dryRun bool, stderr io.Writer) (install.Result, error) {
	args := []string{"--agent", agent}
	if dryRun {
		args = append(args, "--dry-run")
	}
	var out bytes.Buffer
	var r install.Result
	if err := runInstall(ctx, Config{ClientConfig: path, Session: session, LinearProfile: profile}, args, &out, stderr); err != nil {
		return r, err
	}
	if err := json.Unmarshal(out.Bytes(), &r); err != nil {
		return r, errors.New("MCP installation returned no result")
	}
	return r, nil
}
var wizardDoctor = func(ctx context.Context, path, profile string, args []string) (doctor.Report, error) {
	var out bytes.Buffer
	// The report is parsed, so ask for JSON whatever the terminal is: Run
	// chose the human layout for a person at one.
	ctx = clirender.WithMode(ctx, clirender.JSON)
	err := runDoctorCommand(ctx, Config{ClientConfig: path, LinearProfile: profile}, args, &out, io.Discard)
	var r doctor.Report
	if json.Unmarshal(out.Bytes(), &r) != nil || r.Status == "" {
		if err == nil {
			err = errors.New("doctor returned no report")
		}
		return r, err
	}
	return r, nil // A report with failures is still a report to show.
}

// wizardDispatcher runs `clawbridge dispatcher ARGS` on the profile; tests
// replace it. It is set in init: Run reaches onboarding, which uses it.
var wizardDispatcher func(ctx context.Context, path string, args []string) (map[string]any, error)

func init() { wizardDispatcher = runWizardDispatcher }

// runWizardDispatcher returns the command's data.
func runWizardDispatcher(ctx context.Context, path string, args []string) (map[string]any, error) {
	var out bytes.Buffer
	if err := Run(ctx, append([]string{"--client-config", path, "dispatcher"}, args...), &out, io.Discard); err != nil {
		return nil, err
	}
	var r struct {
		Data map[string]any `json:"data"`
	}
	if json.Unmarshal(out.Bytes(), &r) != nil || r.Data == nil {
		return nil, errors.New("the dispatcher returned no answer")
	}
	return r.Data, nil
}

var wizardAgentNames = map[string]string{"claude-code": "Claude Code", "codex": "Codex"}

const wizardCloseMsg = "guided terminal input cleanup failed; restore the terminal before retrying"

// terminalPrompter answers onboarding's questions from the wizard prompt.
type terminalPrompter struct {
	ctx    context.Context
	stdout io.Writer
	prompt *wizardPrompt
	ended  bool
}

// Ask answers "" after input ends, so every remaining question is "no".
func (t *terminalPrompter) Ask(question, fallback string) (string, error) {
	if t.ended {
		return "", nil
	}
	a, e := t.prompt.ask(question)
	if e != nil && t.ctx.Err() == nil {
		t.ended = true
		return "", nil
	}
	if a == "" {
		a = fallback
	}
	return a, e
}

func (t *terminalPrompter) Yes(question string) (bool, error) {
	a, e := t.Ask(question+" [y/N]: ", "")
	a = strings.ToLower(a)
	return a == "y" || a == "yes", e
}

func (t *terminalPrompter) Ended() bool { return t.ended }

func (t *terminalPrompter) Suspend() error { return t.prompt.close() }

func (t *terminalPrompter) Resume() error {
	p, err := newWizardPrompt(t.ctx, t.stdout, wizardCloseMsg)
	if err != nil {
		return err
	}
	t.prompt = p
	return nil
}

// terminalOutput prints onboarding's lines to stdout.
type terminalOutput struct{ stdout io.Writer }

func (o terminalOutput) Line(s string) { fmt.Fprintln(o.stdout, s) }

func (o terminalOutput) Heading(n int, name string) {
	fmt.Fprintf(o.stdout, "\nOptional setup %d of 5 · %s\n", n, name)
}

func (o terminalOutput) Record(name, outcome, later string) {
	fmt.Fprintf(o.stdout, "%s: %s\n", name, outcome)
}

func (o terminalOutput) MCPPreview(p install.Result) error {
	return clirender.Write(o.stdout, clirender.Human, "onboarding_mcp_entry", p)
}

// runOnboarding offers the optional setup after a verified join. Each step is
// skippable, writes nothing without a yes, and a failed step is reported
// without undoing earlier ones. Only cancellation stops it with an error.
// The steps live in uiflow/onboarding; this is their terminal adapter.
func runOnboarding(ctx context.Context, path string, stdout, stderr io.Writer) (retErr error) {
	prompt, err := newWizardPrompt(ctx, stdout, wizardCloseMsg)
	if err != nil {
		return err
	}
	p := &terminalPrompter{ctx: ctx, stdout: stdout, prompt: prompt}
	defer func() {
		if e := p.prompt.close(); e != nil && !errors.Is(retErr, e) {
			retErr = errors.Join(retErr, e)
		}
	}()
	// The package vars are read at call time, so tests can replace them.
	fx := onboarding.Effects{
		SkillTargets: func() ([]skill.Target, error) { return wizardSkillTargets() },
		SkillInstall: func(t []skill.Target) ([]skill.Result, error) { return wizardSkillInstall(t) },
		LinearStore: func(ctx context.Context, ask func(string, string) (string, error)) (string, error) {
			return wizardLinearStore(ctx, ask, stdout)
		},
		LinearConnect: func(ctx context.Context, name, access, store string) (string, error) {
			return wizardLinearConnect(ctx, path, name, access, store, stderr)
		},
		MCPInstall: func(ctx context.Context, agent, session, profile string, dryRun bool) (install.Result, error) {
			return wizardMCPInstall(ctx, path, agent, session, profile, dryRun, stderr)
		},
		Doctor: func(ctx context.Context, profile string, args []string) (doctor.Report, error) {
			return wizardDoctor(ctx, path, profile, args)
		},
		Dispatcher: func(ctx context.Context, yes func(string) (bool, error), record func(name, outcome, later string), failed func(error) string) error {
			return offerDispatcher(ctx, path, stdout, yes, record, failed)
		},
		AgentNames: wizardAgentNames,
	}
	steps, err := onboarding.Run(ctx, p, terminalOutput{stdout}, fx)
	if err != nil {
		return err
	}
	fmt.Fprintln(stdout, "\nSummary:")
	for _, s := range steps {
		fmt.Fprintf(stdout, "- %s: %s\n", s.Name, s.Outcome)
		if s.Later != "" && !strings.HasPrefix(s.Name, "Doctor") {
			fmt.Fprintf(stdout, "  later: %s\n", s.Later)
		}
	}
	if p.ended {
		fmt.Fprintln(stdout, "Input ended; the remaining steps were skipped.")
	}
	return nil
}

// dispatcherSkips are the enable preview's refusals that mean "not for
// this run", each with what to tell the user.
var dispatcherSkips = []struct{ text, outcome string }{
	{"project owner permission required", "skipped: only a project owner turns it on"},
	{"link the task board", "skipped: link the task board first (clawbridge linear link)"},
	{"not from a session", "skipped: turn it on in your own terminal with no session"},
}

const dispatcherOwnerSkip = "skipped: the project owner sets the dispatcher"

// dispatcherRefusal is the skip a refusal of show or the enable preview
// means. The named refusals come first (a local server's errors keep their
// text; a session's refusal is a 403 too); any other 403 is a member's: the
// project owner's setting, with nothing to do later.
func dispatcherRefusal(err error, later string) (outcome, hint string, ok bool) {
	for _, s := range dispatcherSkips {
		if strings.Contains(err.Error(), s.text) {
			return s.outcome, later, true
		}
	}
	if ownersSetting(err) {
		return dispatcherOwnerSkip, "", true
	}
	return "", "", false
}

// ownersSetting reports a refusal that means the dispatcher is the project
// owner's to set: a 403. The client doesn't know its project role (neither
// the join nor whoami carries it), so the server's refusal tells a member.
func ownersSetting(err error) bool {
	var refused interface {
		Rejection() (status int, code, message string)
	}
	if !errors.As(err, &refused) {
		return false
	}
	status, code, _ := refused.Rejection()
	return status == http.StatusForbidden || code == "forbidden"
}

// offerDispatcher is onboarding's last step: it shows a hosted project's
// owner what the dispatcher does and the enable preview, and turns it on
// in the rules engine only on a yes. It runs the explicit commands.
func offerDispatcher(ctx context.Context, path string, stdout io.Writer, yes func(string) (bool, error), record func(name, outcome, later string), failed func(error) string) error {
	const name = "Dispatcher"
	later := "clawbridge dispatcher show, then clawbridge dispatcher enable --expected-revision N --confirm"
	const model = "model mode: after model setup, see docs/dispatcher.md (Model mode)"
	c, err := readConfig(path, false)
	if err != nil || c.Mode != "cloud" {
		record(name, "skipped: the dispatcher runs on a hosted server", "")
		return nil
	}
	// The server refuses the switch from a session with a bare 403 (its
	// text isn't kept for a 403), so the client says so itself, first.
	if c.Session != "" || os.Getenv("CLAWBRIDGE_SESSION") != "" {
		record(name, "skipped: turn it on in your own terminal with no session", "")
		return nil
	}
	shown, err := wizardDispatcher(ctx, path, []string{"show"})
	if err != nil {
		if outcome, hint, ok := dispatcherRefusal(err, later); ok {
			record(name, outcome, hint)
			return nil
		}
		record(name, failed(err), later)
		return nil
	}
	if shown["enabled"] == true {
		record(name, "already on", "")
		return nil
	}
	revision := fmt.Sprint(shown["revision"])
	if f, ok := shown["revision"].(float64); ok {
		revision = strconv.FormatFloat(f, 'f', -1, 64)
	}
	later = "clawbridge dispatcher show, then clawbridge dispatcher enable --expected-revision " + clirender.Safe(revision) + " --confirm"
	preview, err := wizardDispatcher(ctx, path, []string{"enable", "--engine", "rules", "--expected-revision", revision})
	if err != nil {
		if outcome, hint, ok := dispatcherRefusal(err, later); ok {
			record(name, outcome, hint)
			return nil
		}
		record(name, failed(err), later)
		return nil
	}
	// Enabling would be refused: say why instead of asking.
	if blocked := previewWord(preview["blocked"]); blocked != "-" {
		why := map[string]string{"no_domains": "skipped: the task board names no Linear projects; set domains first (clawbridge dispatcher domains set)"}[blocked]
		record(name, cmp.Or(why, "skipped: "+clirender.Safe(blocked)), later)
		return nil
	}
	fmt.Fprintln(stdout, "\nThe dispatcher coordinates this project for you: it confirms claims from sessions you trust, routes notices and releases abandoned claims. It stays off unless you say yes, and clawbridge dispatcher disable turns it off any time. Turning it on:")
	if err = clirender.Write(stdout, clirender.Human, "onboarding_dispatcher_preview", preview); err != nil {
		return err
	}
	ok, err := yes("Turn the dispatcher on now (rules engine)?")
	switch {
	case err != nil:
		return err
	case !ok:
		record(name, "offered, left off", later)
		return nil
	}
	if _, err = wizardDispatcher(ctx, path, []string{"enable", "--engine", "rules", "--expected-revision", revision, "--confirm"}); err != nil {
		record(name, failed(err), later)
		return nil
	}
	record(name, "on (rules engine); turn it off with clawbridge dispatcher disable", model)
	return nil
}

func doctorSummary(r doctor.Report) string { return onboarding.DoctorSummary(r) }

// wizardLinearStore is where the Linear connection is kept: the system key
// store, or, where there is none, a file once the person says yes, as the
// device key's is. Empty means no.
func wizardLinearStore(ctx context.Context, ask func(string, string) (string, error), stdout io.Writer) (string, error) {
	e := wizardKeyStore(ctx)
	if e == nil {
		return linearconn.StoreKeyring, nil
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	// Headless Linux, WSL or a container: no system key store.
	fmt.Fprintf(stdout, "This machine has no system key store (%s).\nClawBridge can keep the Linear connection in a plain file under ~/.clawbridge/linear, readable only by you. But %s.\n", clirender.Safe(e.Error()), linearconn.FileWarning)
	answer, err := ask("Keep the Linear connection in a file? [y/N]: ", "")
	if err != nil {
		return "", err
	}
	if a := strings.ToLower(answer); a != "y" && a != "yes" {
		return "", nil
	}
	return linearconn.StoreFile, nil
}
