package skill

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
)

const usage = "usage: clawbridge skill install|status|uninstall [--playbook gcp|aws] [--agent claude-code|codex|all] [--dir SKILLS_DIR] [--force]"

// RunWithIO is the `clawbridge skill` command. Results are JSON; a refused
// target is reported in the JSON and also makes the command fail.
func RunWithIO(args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 || (args[0] != "install" && args[0] != "status" && args[0] != "uninstall") {
		if len(args) > 0 && (args[0] == "--help" || args[0] == "-h") {
			fmt.Fprintln(stderr, usage)
			return nil
		}
		return errors.New(usage)
	}
	command := args[0]
	f := flag.NewFlagSet("skill "+command, flag.ContinueOnError)
	f.SetOutput(stderr)
	agent := f.String("agent", "", "claude-code, codex or all (default: each agent whose configuration exists)")
	dir := f.String("dir", "", "custom skills directory; the skill goes in DIR/clawbridge")
	playbook := f.String("playbook", "", "a provisioning playbook instead of the main skill: gcp or aws")
	force := new(bool)
	if command != "status" {
		f.BoolVar(force, "force", false, "replace or remove files changed since installation")
	}
	f.Bool("json", true, "return JSON (the default output format)")
	if err := f.Parse(args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if f.NArg() != 0 {
		return errors.New("skill " + command + " accepts flags only")
	}
	if *agent != "" && *agent != "all" && *agent != "claude-code" && *agent != "codex" && *dir == "" {
		return errors.New("--agent must be claude-code, codex or all")
	}
	name := Name
	if *playbook != "" {
		if name = Playbooks[*playbook]; name == "" {
			return errors.New("--playbook must be gcp or aws")
		}
	}
	targets, err := TargetsFor(name, *agent, *dir)
	if err != nil {
		return err
	}
	var results any
	refused := 0
	switch command {
	case "status":
		results, err = Status(targets)
	case "install", "uninstall":
		var rs []Result
		if command == "install" {
			rs, err = Install(targets, *force)
		} else {
			rs, err = Uninstall(targets, *force)
		}
		for _, r := range rs {
			if r.Refused != "" {
				refused++
			}
		}
		results = rs
	}
	if err != nil {
		return err
	}
	if err = json.NewEncoder(stdout).Encode(map[string]any{"source": "skill", "skills": results}); err != nil {
		return err
	}
	if refused > 0 {
		return fmt.Errorf("skill %s refused for %d target(s); see the JSON result", command, refused)
	}
	return nil
}
