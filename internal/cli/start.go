package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"

	"clawbridge/internal/clirender"
)

// Start is the first-run screen (live/ONBOARDING-DESIGN.md §1): what
// ClawBridge is for, and the three ways to begin.
const startText = `ClawBridge connects your coding agents (Claude Code, Codex) so they can message each other, claim
Linear issues, and work as a team. Pick how you want to start:

  1. Just this machine     Local messaging between agents on this computer. No server, no account.
  2. Join a team server    Someone invited you (a server address and an invite code).
  3. Set up my own server  Host ClawBridge for your team (a small cloud VM, about $15–30/month, this machine, or your own server).
`

// startCommands is what Start prints without a terminal: the command behind
// each choice.
const startCommands = `
Without a terminal, run the command for your choice:
  1. clawbridge setup --local --name WORKSPACE --project PROJECT, then clawbridge project create PROJECT
  2. with an invite link: clawbridge join (paste the link); with an invite code: clawbridge --server URL join --email EMAIL (asks for the code)
  3. see docs/operations/first-deployment.md (a cloud VM) or docs/operations/local-host.md (this machine or your LAN);
     check each step with clawbridge cloud check
Run clawbridge onboard in a terminal to choose interactively, or clawbridge help for every command.
`

const ownServerText = `
Your own server, in order. Each step is done when its check passes:
  1. Choose the cloud (Google Cloud, AWS, or a server you already have over SSH), region and domain.
  2. See the cost estimate and say yes.
  3. Provision the VM, IP, disk, firewall, DNS and backups.
  4. Install the server (env file, secrets generated on the VM, TLS).   clawbridge cloud check server version
  5. Create the workspace and join it: clawbridge cloud bootstrap.       clawbridge cloud check workspace
  6. The first project, the Linear app, invites, and the dispatcher offer.
To do it yourself, follow docs/operations/first-deployment.md and run the check after each step. With
Claude Code or Codex installed, clawbridge onboard can hand the steps to your agent instead.
Secrets you hold are entered with clawbridge cloud secret set KEY, never through an agent.
On this machine or your LAN instead (no cloud): see docs/operations/local-host.md (deploy/local-host.sh up).
`

var startSlug = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)

// defaultProject names the first local project after the current directory
// when that makes a valid slug.
func defaultProject() string {
	if wd, err := os.Getwd(); err == nil {
		if name := strings.ToLower(filepath.Base(wd)); startSlug.MatchString(name) {
			return name
		}
	}
	return "my-project"
}

// runOnboard reopens Start at any time; `clawbridge onboard`.
func runOnboard(ctx context.Context, flags Config, args []string, stdout, stderr io.Writer) error {
	if _, err := commandFlags("onboard", args, stderr, func(*flag.FlagSet) {}); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if !reflect.DeepEqual(flags, Config{ClientConfig: flags.ClientConfig}) {
		return errors.New("onboard accepts only --client-config; enter the rest in the prompts")
	}
	path, err := selectedClientPath(flags)
	if err != nil {
		return err
	}
	return runStart(ctx, path, stdout, stderr)
}

func runStart(ctx context.Context, path string, stdout, stderr io.Writer) (retErr error) {
	if wizardTerminal(stdout) != nil {
		_, err := io.WriteString(stdout, startText+startCommands)
		return err
	}
	prompt, err := newWizardPrompt(ctx, stdout, "guided terminal input cleanup failed; nothing was set up, restore the terminal before retrying")
	if err != nil {
		return err
	}
	defer func() {
		if e := prompt.close(); e != nil && !errors.Is(retErr, e) {
			retErr = errors.Join(retErr, e)
		}
	}()
	fmt.Fprint(stdout, startText+"\n")
	choice, err := askUntil(prompt.ask, stdout, "Choose 1, 2 or 3: ", func(a string) (string, error) {
		if a != "1" && a != "2" && a != "3" {
			return "", errors.New("choose 1, 2 or 3")
		}
		return a, nil
	})
	if err != nil {
		return err
	}
	switch choice {
	case "1":
		return startLocal(ctx, prompt, path, stdout, stderr)
	case "2":
		fmt.Fprintln(stdout, "\nA workspace is your team's space on the server; a project is one codebase in it, which your agents share.")
		if err = prompt.close(); err != nil {
			return err
		}
		return runWizard(ctx, Config{ClientConfig: path}, nil, stdout, stderr)
	case "3":
		return startOwnServer(ctx, prompt, path, stdout, stderr)
	}
	return nil
}

// startLocal runs the same commands as setup --local and project create,
// then the optional agent setup.
func startLocal(ctx context.Context, prompt *wizardPrompt, path string, stdout, stderr io.Writer) error {
	fmt.Fprintln(stdout, "\nLocal messaging on this machine. A workspace holds your projects; a project is one codebase your agents share.")
	workspace, err := prompt.ask("Workspace name [my-team]: ")
	if err != nil {
		return err
	}
	if workspace = strings.TrimSpace(workspace); workspace == "" {
		workspace = "my-team"
	}
	fallback := defaultProject()
	project, err := askUntil(prompt.ask, stdout, "First project ["+fallback+"]: ", askProjectSlug(fallback))
	if err != nil {
		return err
	}
	if err = prompt.close(); err != nil {
		return err
	}
	selector := []string{"--client-config", path}
	if err = Run(ctx, append(selector, "setup", "--local", "--name", workspace, "--project", project), io.Discard, stderr); err != nil {
		return fmt.Errorf("local setup failed; nothing else was changed: %w", err)
	}
	if err = Run(ctx, append(selector, "project", "create", project), io.Discard, stderr); err != nil {
		return fmt.Errorf("the workspace is set up, but creating the project failed (retry: clawbridge project create %s): %w", project, err)
	}
	fmt.Fprintf(stdout, "Workspace %s and project %s are ready.\n", clirender.Safe(workspace), clirender.Safe(project))
	if err = runOnboarding(ctx, path, stdout, stderr); err != nil {
		return fmt.Errorf("local setup is saved, but the optional setup stopped: %w", err)
	}
	fmt.Fprintln(stdout, `
Your agents can now register and message each other. To try it here:
  clawbridge register me
  clawbridge sessions`)
	return openNow(ctx, path, stdout, stderr)
}

// askProjectSlug checks a project name as it is typed; empty is fallback.
func askProjectSlug(fallback string) func(string) (string, error) {
	return func(a string) (string, error) {
		if a == "" {
			a = fallback
		}
		if !startSlug.MatchString(a) {
			return "", errors.New("a project name is lowercase letters, digits and dashes")
		}
		return a, nil
	}
}
