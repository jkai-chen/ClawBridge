// Package onboarding runs the five optional setup steps that follow a
// verified join. It knows nothing about a terminal: the questions, the
// printing and the effects (skill install, Linear sign-in, MCP writes,
// doctor, dispatcher) are all injected, so the terminal wizard and a GUI
// drive the same code.
package onboarding

import (
	"context"
	"strings"

	"clawbridge/internal/clirender"
	"clawbridge/internal/doctor"
	"clawbridge/internal/install"
	"clawbridge/internal/skill"
)

// Prompter asks the person. It blocks until there is an answer.
type Prompter interface {
	// Yes asks a yes/no question; the "[y/N]: " suffix is the adapter's job.
	Yes(question string) (bool, error)
	Ask(question, fallback string) (string, error)
	// Ended reports that input ended: every later question answers no.
	Ended() bool
	// Suspend runs before the browser sign-in (stdin is not shared), Resume after it.
	Suspend() error
	Resume() error
}

// Output shows the person what is happening.
type Output interface {
	Line(s string)
	Heading(n int, name string)
	// Record prints "name: outcome"; Run keeps the step for the summary.
	Record(name, outcome, later string)
	MCPPreview(p install.Result) error
}

// Effects are the actions the steps take. The caller binds the profile,
// config path and writers.
type Effects struct {
	SkillTargets  func() ([]skill.Target, error)
	SkillInstall  func([]skill.Target) ([]skill.Result, error)
	LinearStore   func(ctx context.Context, ask func(string, string) (string, error)) (string, error)
	LinearConnect func(ctx context.Context, name, access, store string) (string, error)
	MCPInstall    func(ctx context.Context, agent, session, profile string, dryRun bool) (install.Result, error)
	Doctor        func(ctx context.Context, profile string, args []string) (doctor.Report, error)
	Dispatcher    func(ctx context.Context, yes func(string) (bool, error), record func(name, outcome, later string), failed func(error) string) error
	AgentNames    map[string]string
}

// Step is one line of the summary.
type Step struct{ Name, Outcome, Later string }

// Run offers the optional setup. Each step is skippable, writes nothing
// without a yes, and a failed step is reported without undoing earlier ones.
// Only cancellation (or a prompt or output failure) stops it with an error;
// the steps recorded so far are returned either way.
func Run(ctx context.Context, p Prompter, out Output, fx Effects) ([]Step, error) {
	steps := []Step{}
	record := func(name, outcome, later string) {
		steps = append(steps, Step{name, outcome, later})
		out.Record(name, outcome, later)
	}
	err := run(ctx, p, out, fx, record)
	return steps, err
}

func failed(e error) string { return "failed: " + clirender.Safe(e.Error()) }

func run(ctx context.Context, p Prompter, out Output, fx Effects, record func(name, outcome, later string)) error {
	yes, ask := p.Yes, p.Ask
	// U52: where the person is in the optional steps.
	heading := out.Heading
	out.Line("\nOptional setup. Each step can be skipped and run later; nothing is written without a yes.")
	agents := []string{}
	targets, targetErr := fx.SkillTargets()
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
			out.Line("  " + fx.AgentNames[t.Agent] + ": " + clirender.Safe(t.Dir))
		}
		ok, e := yes("Install or update the ClawBridge agent skill there?")
		switch {
		case e != nil:
			return e
		case !ok:
			record("Agent skill", "skipped", skillLater)
		default:
			results, e := fx.SkillInstall(targets)
			refused := []string{}
			for _, r := range results {
				if r.Refused != "" {
					refused = append(refused, fx.AgentNames[r.Agent]+": "+r.Refused)
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
		if p.Ended() {
			record("Linear", "skipped", linearLater)
		} else if access != "read" && access != "write" {
			record("Linear", "skipped: choose read or write", linearLater)
		} else if store, e := fx.LinearStore(ctx, ask); e != nil {
			return e
		} else if store == "" {
			record("Linear", "skipped: no system key store", "unlock or start a key store, or clawbridge linear connect --name NAME --access read --key-store file, then clawbridge doctor --linear-profile PROFILE_ID")
		} else {
			// No wizard reader may share stdin with the browser sign-in.
			if e = p.Suspend(); e != nil {
				return e
			}
			out.Line("Opening Linear sign-in in your browser…")
			profile, e = fx.LinearConnect(ctx, name, access, store)
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if err = p.Resume(); err != nil {
				return err
			}
			if e != nil {
				profile = ""
				record("Linear", failed(e), linearLater)
			} else {
				outcome := "connected (" + access + " access, profile " + clirender.Safe(profile) + ")"
				if report, e := fx.Doctor(ctx, profile, nil); e != nil {
					outcome += "; doctor " + failed(e)
				} else {
					outcome += "; " + CapabilitySummary(report)
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
		label := "MCP for " + fx.AgentNames[agent]
		later := "clawbridge mcp install --agent " + agent + " --session NAME"
		if profile != "" {
			later += " --linear-profile " + profile
		}
		later += " --dry-run"
		ok, e := yes("Connect " + fx.AgentNames[agent] + " to ClawBridge MCP?")
		if e != nil {
			return e
		}
		if !ok {
			record(label, "skipped", later)
			continue
		}
		session, e := ask("Session name for "+fx.AgentNames[agent]+" ["+agent+"]: ", agent)
		if e != nil {
			return e
		}
		if p.Ended() {
			record(label, "skipped", later)
			continue
		}
		preview, e := fx.MCPInstall(ctx, agent, session, profile, true)
		if e != nil {
			record(label, failed(e), later)
			continue
		}
		if err := out.MCPPreview(preview); err != nil {
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
		result, e := fx.MCPInstall(ctx, agent, session, profile, false)
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
		runs = append(runs, doctorRun{"Doctor (" + fx.AgentNames[m.agent] + ")", []string{"--mcp-agent", m.agent, "--mcp-config", m.path, "--mcp-session", m.session}})
	}
	for _, run := range runs {
		if report, e := fx.Doctor(ctx, profile, run.args); e != nil {
			record(run.name, failed(e), "clawbridge doctor")
		} else {
			record(run.name, DoctorSummary(report), "clawbridge doctor")
		}
	}
	heading(5, "Dispatcher")
	// 5. The dispatcher, offered to a hosted project's owner (G5,
	// ONBOARDING-DESIGN step 10): off unless they say yes, rules engine.
	return fx.Dispatcher(ctx, yes, record, failed)
}

// CapabilitySummary names the Linear capabilities check's status.
func CapabilitySummary(r doctor.Report) string {
	for _, c := range r.Checks {
		if c.ID == "linear.capabilities" {
			return "capabilities " + c.Status
		}
	}
	return "capabilities not reported"
}

// DoctorSummary names the overall status and every check that warned or
// failed; a note (info) is no issue.
func DoctorSummary(r doctor.Report) string {
	issues := []string{}
	for _, c := range r.Checks {
		if c.Status != doctor.Pass && c.Status != doctor.Info {
			issues = append(issues, c.Status+" "+c.Title)
		}
	}
	if len(issues) == 0 {
		return r.Status
	}
	return clirender.Safe(r.Status + " (" + strings.Join(issues, "; ") + ")")
}
