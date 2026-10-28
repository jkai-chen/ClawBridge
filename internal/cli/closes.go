package cli

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"

	"clawbridge/internal/bus"
	"clawbridge/internal/linearboard"
	"clawbridge/internal/linearprovider"
)

// A3 §10.8 SF3-M1: letting agents close a project's issues is a person's
// decision, typed at a terminal. An agent in its person's shell holds the
// same credentials, so the CLI refuses without a terminal, and under an
// agent's environment.
var (
	closesStdin io.Reader = os.Stdin
	closesEnv             = os.Environ
)

// agentMarker names the environment variable that says an agent runs this
// command: Claude Code's, Codex's, or a ClawBridge session's.
func agentMarker(environ []string) string { return linearboard.AgentMarker(environ) }

// confirmAgentCloses asks the person to type each project's name, in
// order of UUID. names maps a project UUID to its Linear name.
func confirmAgentCloses(flags Config, names map[string]string, stderr io.Writer) error {
	if flags.Session != "" {
		return errors.New("letting agents close issues is never done with --session; the project owner or a lead runs it in their own terminal")
	}
	if marker := agentMarker(closesEnv()); marker != "" {
		return fmt.Errorf("letting agents close issues is a person's decision, and %s says an agent runs this command; run it yourself in a terminal", marker)
	}
	if !stdinTerminal() {
		return errors.New("letting agents close issues is confirmed at a terminal by typing the project's name; stdin is not a terminal")
	}
	ids := make([]string, 0, len(names))
	for id := range names {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	in := bufio.NewReader(closesStdin)
	for _, id := range ids {
		name := names[id]
		if name == "" {
			return fmt.Errorf("could not read the name of Linear project %s from Linear", id)
		}
		fmt.Fprintf(stderr, "Agents will close the issues of Linear project %q (%s), completing them without a person.\nType the project's name to confirm: ", name, id)
		line, err := in.ReadString('\n')
		if err != nil && line == "" {
			return errors.New("no confirmation was typed; nothing was changed")
		}
		if strings.TrimRight(line, "\r\n") != name {
			return fmt.Errorf("the name typed is not %q; nothing was changed", name)
		}
	}
	return nil
}

// linkCloses reads link's --closes UUID=agent|human pairs.
func linkCloses(pairs []string) (map[string]string, error) {
	if len(pairs) == 0 {
		return nil, nil
	}
	out := map[string]string{}
	for _, pair := range pairs {
		id, who, ok := strings.Cut(pair, "=")
		if !ok || !linearprovider.ValidID(id) || (who != linearboard.ClosesAgent && who != linearboard.ClosesHuman) {
			return nil, errors.New("--closes is UUID=agent or UUID=human")
		}
		if _, dup := out[id]; dup {
			return nil, fmt.Errorf("--closes names Linear project %s twice", id)
		}
		out[id] = who
	}
	return out, nil
}

func agentProjects(closes map[string]string) []string {
	var out []string
	for id, who := range closes {
		if who == linearboard.ClosesAgent {
			out = append(out, id)
		}
	}
	return out
}

// confirmLinkCloses has the person type the name of each project a
// confirmed link lets agents close, read from the link's own preview, so
// only a project whose rule changes is asked about.
func confirmLinkCloses(ctx context.Context, service *linearboard.Service, scope bus.Request, flags Config, in *linearboard.LinkInputV2, stderr io.Writer) error {
	in.Via = linearboard.ClosesViaCLI
	if !in.Confirm || len(agentProjects(in.Closes)) == 0 {
		return nil
	}
	preview := *in
	preview.Confirm = false
	data, err := service.LinkV2(ctx, scope, preview)
	if err != nil {
		return err
	}
	shown, _ := data.(linearboard.LinkPreviewV2)
	names := map[string]string{}
	for _, line := range shown.Closes {
		if line.Set == linearboard.ClosesAgent {
			names[line.LinearProject] = line.Name
		}
	}
	if len(names) == 0 {
		return nil
	}
	if err = confirmAgentCloses(flags, names, stderr); err != nil {
		return err
	}
	in.Via = linearboard.ClosesViaTTY
	return nil
}

// runCloses is linear closes show|set.
func runCloses(ctx context.Context, service *linearboard.Service, scope bus.Request, flags Config, v linearFlags, stderr io.Writer) (any, error) {
	if v.adminAction == "show" {
		return service.ClosesV2(ctx, scope)
	}
	if v.closesHuman {
		return service.SetClosesV2(ctx, scope, v.project, linearboard.ClosesHuman, v.revision, linearboard.ClosesViaCLI)
	}
	if !validLinearProfile(v.profile) {
		return nil, errors.New("closes set --agent reads the project's name from Linear: pass --profile")
	}
	name, err := service.ProjectNameV2(ctx, scope, v.project)
	if err != nil {
		return nil, err
	}
	if err = confirmAgentCloses(flags, map[string]string{v.project: name}, stderr); err != nil {
		return nil, err
	}
	return service.SetClosesV2(ctx, scope, v.project, linearboard.ClosesAgent, v.revision, linearboard.ClosesViaTTY)
}
