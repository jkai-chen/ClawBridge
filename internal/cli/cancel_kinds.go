package cli

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"

	"clawbridge/internal/bus"
	"clawbridge/internal/linearboard"
)

// AGENT-CANCEL R3: letting agents cancel or mark Duplicate issues in
// projects of a kind is a person's decision, typed at a terminal, as who
// closes is (confirmAgentCloses).

// cancelKindsPending is said with every preview until the enforcement
// ships.
const cancelKindsPending = "Until the enforcement ships, agents still never cancel; the setting only records the choice."

// confirmCancelKinds shows what the list allows and asks the person to
// type the kind names, comma-separated, exactly as shown.
func confirmCancelKinds(flags Config, kinds []string, stderr io.Writer) error {
	if flags.Session != "" {
		return errors.New("letting agents cancel issues is never done with --session; the project owner or a lead runs it in their own terminal")
	}
	if marker := agentMarker(closesEnv()); marker != "" {
		return fmt.Errorf("letting agents cancel issues is a person's decision, and %s says an agent runs this command; run it yourself in a terminal", marker)
	}
	if !stdinTerminal() {
		return errors.New("letting agents cancel issues is confirmed at a terminal by typing the kind names; stdin is not a terminal")
	}
	want := strings.Join(kinds, ", ")
	fmt.Fprintf(stderr, "Agents will be allowed to cancel or mark Duplicate issues in the Linear projects of kind %s (projects of these kinds), without a person.\n%s\nType the kind names, comma-separated, to confirm (%s): ", want, cancelKindsPending, want)
	line, err := bufio.NewReader(closesStdin).ReadString('\n')
	if err != nil && line == "" {
		return errors.New("no confirmation was typed; nothing was changed")
	}
	var typed []string
	for _, name := range strings.Split(strings.TrimRight(line, "\r\n"), ",") {
		typed = append(typed, strings.TrimSpace(name))
	}
	if !slices.Equal(typed, kinds) {
		return fmt.Errorf("the names typed are not %q; nothing was changed", want)
	}
	return nil
}

// runCancelKinds is linear cancel-kinds show|set.
func runCancelKinds(ctx context.Context, service *linearboard.Service, scope bus.Request, flags Config, v linearFlags, stderr io.Writer) (any, error) {
	if v.adminAction == "show" {
		return service.CancelKindsV2(ctx, scope)
	}
	if v.cancelNone {
		return service.SetCancelKindsV2(ctx, scope, nil, v.revision, linearboard.ClosesViaCLI)
	}
	view, err := service.CancelKindsV2(ctx, scope)
	if err != nil {
		return nil, err
	}
	kinds, err := linearboard.CancelKindsMatch(strings.Split(v.cancelKinds, ","), view.Kinds)
	if err != nil {
		return nil, err
	}
	if len(kinds) == 0 {
		return nil, errors.New("--kinds names no kind; --none lets no agent cancel")
	}
	if err = confirmCancelKinds(flags, kinds, stderr); err != nil {
		return nil, err
	}
	return service.SetCancelKindsV2(ctx, scope, kinds, v.revision, linearboard.ClosesViaTTY)
}

// refuseAgentCancelFile refuses a status-policy file that lists any kind
// in agent_may_cancel_kinds under --session or an agent's environment,
// changed or not: who may cancel is a person's decision. It runs before
// anything is sent.
func refuseAgentCancelFile(flags Config, raw []byte) error {
	var file map[string]json.RawMessage
	if json.Unmarshal(raw, &file) != nil {
		return nil
	}
	named := false
	for key, value := range file {
		var kinds []string
		if strings.EqualFold(key, "agent_may_cancel_kinds") && json.Unmarshal(value, &kinds) == nil {
			named = named || len(kinds) > 0
		}
	}
	if !named {
		return nil
	}
	marker := "--session"
	if flags.Session == "" {
		if marker = agentMarker(closesEnv()); marker == "" {
			return nil
		}
	}
	return fmt.Errorf(`who may cancel is a person's decision, and %s says an agent runs this command; leave "agent_may_cancel_kinds" out of the file (the current list is kept) or have the person run it in their own terminal`, marker)
}
