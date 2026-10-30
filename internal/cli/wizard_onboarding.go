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

type onboardingStep struct{ name, outcome, later string }

// runOnboarding offers the optional setup after a verified join. Each step is
// skippable, writes nothing without a yes, and a failed step is reported
// without undoing earlier ones. Only cancellation stops it with an error.
func runOnboarding(ctx context.Context, path string, stdout, stderr io.Writer) (retErr error) {
	prompt, err := newWizardPrompt(ctx, stdout, "guided terminal input cleanup failed; restore the terminal before retrying")
	if err != nil {
		return err
	}
	defer func() {
		if e := prompt.close(); e != nil && !errors.Is(retErr, e) {
			retErr = errors.Join(retErr, e)
		}
	}()
	steps := []onboardingStep{}
	record := func(name, outcome, later string) {
		steps = append(steps, onboardingStep{name, outcome, later})
		fmt.Fprintf(stdout, "%s: %s\n", name, outcome)
	}
	// After input ends, every remaining question is answered "no".
	ended := false
	ask := func(question, fallback string) (string, error) {
		if ended {
			return "", nil
		}
		a, e := prompt.ask(question)
		if e != nil && ctx.Err() == nil {
			ended = true
			return "", nil
		}
		if a == "" {
			a = fallback
		}
		return a, e
	}
	yes := func(question string) (bool, error) {
		a, e := ask(question+" [y/N]: ", "")
		a = strings.ToLower(a)
		return a == "y" || a == "yes", e
	}
	failed := func(e error) string { return "failed: " + clirender.Safe(e.Error()) }
	// U52: where the person is in the optional steps.
	heading := func(n int, name string) { fmt.Fprintf(stdout, "\nOptional setup %d of 5 · %s\n", n, name) }

	fmt.Fprintln(stdout, "\nOptional setup. Each step can be skipped and run later; nothing is written without a yes.")
	agents := []string{}
	targets, targetErr := wizardSkillTargets()
	for _, t := range targets {
		agents = append(agents, t.Agent)
	}

	// 1. Agent skill.
	heading(1, "Agent skill")
	const skillLater = "clawbridge skill install [--agent claude-code|codex|all]"
	if targetErr != nil {
		record("Agent skill", "skipped: no Claude Code or Codex configuration found", skillLater)
	} else {
		for _, t := range targets {
			fmt.Fprintf(stdout, "  %s: %s\n", wizardAgentNames[t.Agent], clirender.Safe(t.Dir))
		}
		ok, e := yes("Install or update the ClawBridge agent skill there?")
		switch {
		case e != nil:
			return e
		case !ok:
			record("Agent skill", "skipped", skillLater)
		default:
			results, e := wizardSkillInstall(targets)
			refused := []string{}
			for _, r := range results {
				if r.Refused != "" {
					refused = append(refused, wizardAgentNames[r.Agent]+": "+r.Refused)
				}
			}
			switch {
			case e != nil:
				record("Agent skill", failed(e), skillLater)
			case len(refused) > 0:
				record("Agent skill", "refused: "+clirender.Safe(strings.Join(refused, "; ")), "clawbridge skill status, then clawbridge skill install --force")
			default:
				record("Agent skill", "installed", "")
			}
		}
	}

	// 2. The user's own Linear account, so MCP entries can pin its profile.
	profile := ""
	heading(2, "Linear")
	const linearLater = "clawbridge linear connect --name NAME --access read, then clawbridge doctor --linear-profile PROFILE_ID"
	ok, err := yes("Connect your own Linear account (optional)?")
	if err != nil {
		return err
	}
	if !ok {
		record("Linear", "skipped", linearLater)
	} else {
		name, e := ask("Connection name [Linear]: ", "Linear")
		if e != nil {
			return e
		}
		access, e := ask("Access read/write [read] (write lets agents create and update issues): ", "read")
		if e != nil {
			return e
		}
		access = strings.ToLower(access)
		if ended {
			record("Linear", "skipped", linearLater)
		} else if access != "read" && access != "write" {
			record("Linear", "skipped: choose read or write", linearLater)
		} else if store, e := wizardLinearStore(ctx, ask, stdout); e != nil {
			return e
		} else if store == "" {
			record("Linear", "skipped: no system key store", "unlock or start a key store, or clawbridge linear connect --name NAME --access read --key-store file, then clawbridge doctor --linear-profile PROFILE_ID")
		} else {
			// No wizard reader may share stdin with the browser sign-in.
			if e = prompt.close(); e != nil {
				return e
			}
			fmt.Fprintln(stdout, "Opening Linear sign-in in your browser…")
			profile, e = wizardLinearConnect(ctx, path, name, access, store, stderr)
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if prompt, err = newWizardPrompt(ctx, stdout, "guided terminal input cleanup failed; restore the terminal before retrying"); err != nil {
				return err
			}
			if e != nil {
				profile = ""
				record("Linear", failed(e), linearLater)
			} else {
				outcome := "connected (" + access + " access, profile " + clirender.Safe(profile) + ")"
				if report, e := wizardDoctor(ctx, path, profile, nil); e != nil {
					outcome += "; doctor " + failed(e)
				} else {
					outcome += "; " + capabilitySummary(report)
				}
				record("Linear", outcome, "")
			}
		}
	}

	// 3. MCP entry per detected agent, previewed before it is written.
	type entry struct{ agent, path, session string }
	installed := []entry{}
	heading(3, "MCP")
	if len(agents) == 0 {
		record("MCP", "skipped: no Claude Code or Codex configuration found", "clawbridge mcp install --agent codex|claude-code --session NAME --dry-run")
	}
	for _, agent := range agents {
		label := "MCP for " + wizardAgentNames[agent]
		later := "clawbridge mcp install --agent " + agent + " --session NAME"
		if profile != "" {
			later += " --linear-profile " + profile
		}
		later += " --dry-run"
		ok, e := yes("Connect " + wizardAgentNames[agent] + " to ClawBridge MCP?")
		if e != nil {
			return e
		}
		if !ok {
			record(label, "skipped", later)
			continue
		}
		session, e := ask("Session name for "+wizardAgentNames[agent]+" ["+agent+"]: ", agent)
		if e != nil {
			return e
		}
		if ended {
			record(label, "skipped", later)
			continue
		}
		preview, e := wizardMCPInstall(ctx, path, agent, session, profile, true, stderr)
		if e != nil {
			record(label, failed(e), later)
			continue
		}
		if err := clirender.Write(stdout, clirender.Human, "onboarding_mcp_entry", preview); err != nil {
			return err
		}
		if !preview.Changed {
			record(label, "already configured", "")
			installed = append(installed, entry{agent, preview.Path, session})
			continue
		}
		ok, e = yes("Write this entry?")
		if e != nil {
			return e
		}
		if !ok {
			record(label, "skipped", later)
			continue
		}
		result, e := wizardMCPInstall(ctx, path, agent, session, profile, false, stderr)
		if e != nil {
			record(label, failed(e), later)
			continue
		}
		outcome := "configured in " + clirender.Safe(result.Path)
		if result.Backup != "" {
			outcome += " (previous file kept at " + clirender.Safe(result.Backup) + ")"
		}
		record(label, outcome, "")
		installed = append(installed, entry{agent, result.Path, session})
	}

	// 4. Doctor, once per MCP entry configured here (doctor checks one entry
	// per run), or once without one, then the summary.
	type doctorRun struct {
		name string
		args []string
	}
	heading(4, "Doctor")
	runs := []doctorRun{{"Doctor", nil}}
	if len(installed) > 0 {
		runs = nil
	}
	for _, m := range installed {
		runs = append(runs, doctorRun{"Doctor (" + wizardAgentNames[m.agent] + ")", []string{"--mcp-agent", m.agent, "--mcp-config", m.path, "--mcp-session", m.session}})
	}
	for _, run := range runs {
		if report, e := wizardDoctor(ctx, path, profile, run.args); e != nil {
			record(run.name, failed(e), "clawbridge doctor")
		} else {
			record(run.name, doctorSummary(report), "clawbridge doctor")
		}
	}
	heading(5, "Dispatcher")
	// 5. The dispatcher, offered to a hosted project's owner (G5,
	// ONBOARDING-DESIGN step 10): off unless they say yes, rules engine.
	if err := offerDispatcher(ctx, path, stdout, yes, record, failed); err != nil {
		return err
	}
	fmt.Fprintln(stdout, "\nSummary:")
	for _, s := range steps {
		fmt.Fprintf(stdout, "- %s: %s\n", s.name, s.outcome)
		if s.later != "" && !strings.HasPrefix(s.name, "Doctor") {
			fmt.Fprintf(stdout, "  later: %s\n", s.later)
		}
	}
	if ended {
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

func capabilitySummary(r doctor.Report) string {
	for _, c := range r.Checks {
		if c.ID == "linear.capabilities" {
			return "capabilities " + c.Status
		}
	}
	return "capabilities not reported"
}

// doctorSummary names the overall status and every check that did not pass.
func doctorSummary(r doctor.Report) string {
	issues := []string{}
	for _, c := range r.Checks {
		if c.Status != doctor.Pass {
			issues = append(issues, c.Status+" "+c.Title)
		}
	}
	if len(issues) == 0 {
		return r.Status
	}
	return clirender.Safe(r.Status + " (" + strings.Join(issues, "; ") + ")")
}

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
