package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"clawbridge/internal/doctor"
	"clawbridge/internal/install"
	"clawbridge/internal/linearconn"
	"clawbridge/internal/skill"
)

// Each onboarding step runs the same code as its explicit command.
var wizardSkillTargets = func() ([]skill.Target, error) { return skill.Targets("", "") }
var wizardSkillInstall = func(targets []skill.Target) ([]skill.Result, error) { return skill.Install(targets, false) }
var wizardLinearConnect = func(ctx context.Context, path, name, access string, stderr io.Writer) (string, error) {
	var out bytes.Buffer
	if err := runLinear(ctx, Config{ClientConfig: path}, []string{"connect", "--name", name, "--access", access}, &out, stderr); err != nil {
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
	failed := func(e error) string { return "failed: " + wizardSafe(e.Error()) }

	fmt.Fprintln(stdout, "\nOptional setup. Each step can be skipped and run later; nothing is written without a yes.")
	agents := []string{}
	targets, targetErr := wizardSkillTargets()
	for _, t := range targets {
		agents = append(agents, t.Agent)
	}

	// 1. Agent skill.
	const skillLater = "clawbridge skill install [--agent claude-code|codex|all]"
	if targetErr != nil {
		record("Agent skill", "skipped: no Claude Code or Codex configuration found", skillLater)
	} else {
		for _, t := range targets {
			fmt.Fprintf(stdout, "  %s: %s\n", wizardAgentNames[t.Agent], wizardSafe(t.Dir))
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
				record("Agent skill", "refused: "+wizardSafe(strings.Join(refused, "; ")), "clawbridge skill status, then clawbridge skill install --force")
			default:
				record("Agent skill", "installed", "")
			}
		}
	}

	// 2. The user's own Linear account, so MCP entries can pin its profile.
	profile := ""
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
		} else {
			// No wizard reader may share stdin with the browser sign-in.
			if e = prompt.close(); e != nil {
				return e
			}
			fmt.Fprintln(stdout, "Opening Linear sign-in in your browser…")
			profile, e = wizardLinearConnect(ctx, path, name, access, stderr)
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
				outcome := "connected (" + access + " access, profile " + wizardSafe(profile) + ")"
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
		shown, _ := json.MarshalIndent(preview.Entry, "  ", "  ")
		fmt.Fprintf(stdout, "  File: %s\n  Entry: %s\n", wizardSafe(preview.Path), wizardSafe(string(shown)))
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
		outcome := "configured in " + wizardSafe(result.Path)
		if result.Backup != "" {
			outcome += " (previous file kept at " + wizardSafe(result.Backup) + ")"
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
	return wizardSafe(r.Status + " (" + strings.Join(issues, "; ") + ")")
}
