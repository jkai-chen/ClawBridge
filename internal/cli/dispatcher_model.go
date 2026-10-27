package cli

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"clawbridge/internal/bus"
	"clawbridge/internal/dispatchermodel"
)

// How setup waits for its test (§11.3): the agent's test call has 30 s, and
// starting the agent takes some more.
var (
	modelTestPoll = 2 * time.Second
	modelTestWait = 60 * time.Second
)

// errKeyInArgs refuses a command line that looks like it carries a key.
var errKeyInArgs = errors.New("an argument looks like a key: keys are read only at the hidden prompt, or from stdin with --key-stdin. Nothing was sent")

// runModelSetup is `dispatcher model setup` (§11.1), in the owner's own
// terminal: provider, model, key, caps, the egress disclosure, then the
// setup and its test. Every rule is the dispatchermodel package's. The key
// is read only at a hidden prompt, or from stdin with --key-stdin (then
// every other answer comes from flags), and lives only in memory.
func runModelSetup(ctx context.Context, executor interface {
	Execute(context.Context, bus.Request) (bus.Result, error)
}, config Config, a map[string]any, stdout io.Writer) error {
	exec := func(ctx context.Context, op string, args map[string]any) (map[string]any, error) {
		r, err := executor.Execute(ctx, bus.Request{Operation: op, Workspace: config.Workspace, Project: config.Project, Session: config.Session, Args: args})
		m, _ := r.Data.(map[string]any)
		return m, err
	}
	keyStdin, _ := a["key_stdin"].(bool)
	interactive := !keyStdin
	if interactive && !stdinTerminal() {
		return errors.New("dispatcher model setup asks for the key at a hidden prompt, so it needs a terminal; in a script, pass --key-stdin with --provider, --model, --daily-tokens and --confirm-egress")
	}
	if keyStdin && stdinTerminal() {
		// Typed at a terminal, the key would echo: that is what the hidden
		// prompt is for.
		return errors.New("--key-stdin reads a pipe or a file, not a terminal, where the key would show as you type it; run setup without --key-stdin for the hidden prompt")
	}
	in := bufio.NewReader(cloudStdin)
	ask := func(prompt string) (string, error) {
		fmt.Fprint(stdout, prompt)
		line, err := in.ReadString('\n')
		if err != nil && line == "" {
			return "", errors.New("setup stopped: no answer. Nothing was set up")
		}
		return strings.TrimSpace(line), nil
	}
	needFlag := func(flag string) error {
		return fmt.Errorf("with --key-stdin, %s is required: stdin carries only the key", flag)
	}

	// 1. The provider, from the list the agent can run.
	var provider dispatchermodel.Provider
	if id := str(a, "provider"); id != "" {
		p, ok := dispatchermodel.LookupProvider(id)
		if !ok {
			return fmt.Errorf("--provider must be one of: %s", providerIDs())
		}
		provider = p
	} else if !interactive {
		return needFlag("--provider")
	} else {
		fmt.Fprintln(stdout, "Providers:")
		list := dispatchermodel.Providers()
		for i, p := range list {
			fmt.Fprintf(stdout, "  %d. %s (%s)\n", i+1, p.Name, p.ID)
		}
		answer, err := ask("Provider (number or ID): ")
		if err != nil {
			return err
		}
		if n, e := strconv.Atoi(answer); e == nil && n >= 1 && n <= len(list) {
			provider = list[n-1]
		} else if p, ok := dispatchermodel.LookupProvider(answer); ok {
			provider = p
		} else {
			return errors.New("setup stopped: not a provider from the list. Nothing was set up")
		}
	}
	baseURL := str(a, "base_url")
	switch {
	case !provider.Custom && baseURL != "":
		return fmt.Errorf("--base-url is only for openai-compatible; %s's is fixed", provider.ID)
	case provider.Custom && baseURL == "" && !interactive:
		return needFlag("--base-url")
	case provider.Custom && baseURL == "":
		var err error
		if baseURL, err = ask("Base URL (https://…/v1): "); err != nil {
			return err
		}
	}
	if provider.Custom {
		if err := dispatchermodel.ValidateBaseURL(baseURL); err != nil {
			return err
		}
	}

	// 2. The model: no default.
	model := str(a, "model")
	if model == "" && !interactive {
		return needFlag("--model")
	}
	if model == "" {
		// The agent's own catalogue, cheapest first; any ID can be typed.
		shown, more := dispatchermodel.ModelHints(provider.ID)
		prompt := "Model ID, as " + provider.Name + " names it (no default): "
		if len(shown) > 0 {
			fmt.Fprintln(stdout, "Models:")
			for i, m := range shown {
				// The catalogue's text, escaped like any text from outside.
				fmt.Fprintf(stdout, "  %d. %s\n", i+1, wizardSafe(dispatchermodel.HintLine(m)))
			}
			fmt.Fprintln(stdout, "  "+dispatchermodel.HintsNote(more))
			prompt = "Model (number or ID, no default): "
		}
		answer, err := ask(prompt)
		if err != nil {
			return err
		}
		model = dispatchermodel.ModelAnswer(answer, shown)
		if model == "" {
			return errors.New("setup stopped: no model. Nothing was set up")
		}
	}
	if err := dispatchermodel.ValidateModel(model); err != nil {
		return err
	}

	// 3. The key: a hidden prompt, or stdin.
	var key []byte
	var err error
	if keyStdin {
		key, err = io.ReadAll(io.LimitReader(cloudStdin, 16<<10+2))
	} else {
		fmt.Fprintf(stdout, "%s (hidden): ", provider.KeyHint)
		key, err = readHidden()
		fmt.Fprintln(stdout)
	}
	defer clear(key)
	if err != nil {
		return errors.New("the key could not be read. Nothing was set up")
	}

	// 4. The caps, before anything is enabled.
	caps := dispatchermodel.DefaultCaps()
	if err := caps.ParseCaps(str(a, "caps")); err != nil {
		return err
	}
	if daily, _ := a["daily_tokens"].(int64); daily != 0 {
		if err := caps.Set("daily_tokens", daily); err != nil {
			return err
		}
	}
	if interactive {
		fmt.Fprintln(stdout, "Caps (Enter keeps the value shown):")
		for _, f := range dispatchermodel.CapFields {
			current := strconv.FormatInt(caps.Get(f.Key), 10)
			if caps.Get(f.Key) == 0 {
				current = "required"
			}
			answer, err := ask(fmt.Sprintf("  %s [%s]: ", f.Label, current))
			if err != nil {
				return err
			}
			if answer == "" {
				continue
			}
			n, e := strconv.ParseInt(strings.ReplaceAll(answer, ",", ""), 10, 64)
			if e != nil {
				return fmt.Errorf("%s must be a number. Nothing was set up", f.Key)
			}
			if err := caps.Set(f.Key, n); err != nil {
				return err
			}
		}
	}
	if err := caps.Validate(); err != nil {
		if !interactive && caps.DailyTokens == 0 {
			return needFlag("--daily-tokens")
		}
		return err
	}

	// 5. The egress disclosure, confirmed.
	fmt.Fprintln(stdout, dispatchermodel.Disclosure(provider))
	confirmed, _ := a["confirm_egress"].(bool)
	if !confirmed && !interactive {
		return needFlag("--confirm-egress")
	}
	if !confirmed {
		answer, err := ask(`Type "yes" to confirm: `)
		if err != nil {
			return err
		}
		if answer != "yes" {
			return errors.New("setup stopped: the disclosure was not confirmed. Nothing was set up")
		}
	}

	// 6. Setup, and its test in your agent.
	revision, err := dispatchermodel.Setup(ctx, exec, dispatchermodel.Request{Provider: provider, BaseURL: baseURL, Model: model, Key: key, Caps: caps, EgressConfirmed: true})
	if err != nil {
		return err
	}
	fmt.Fprintf(stdout, "Testing the key in your agent (revision %d); this can take up to a minute.\n", revision)
	outcome, err := dispatchermodel.WaitForTest(ctx, exec, revision, modelTestPoll, modelTestWait)
	if err != nil {
		return err
	}
	if provider.Custom {
		fmt.Fprintln(stdout, dispatchermodel.OutcomeText(outcome, baseURL))
	} else {
		fmt.Fprintln(stdout, dispatchermodel.OutcomeText(outcome, provider.BaseURL))
	}
	switch outcome {
	case "active":
		// 7. Enabling stays per project, with its own preview.
		fmt.Fprintln(stdout, "Next, per project: clawbridge dispatcher enable --engine model --expected-revision N (a preview first), or as the rules' advisor: clawbridge dispatcher model advise on --expected-revision N")
		return nil
	case "pending":
		// Not a success: a script can tell it from one by the exit code.
		return &ExitCodeError{Code: 3, Err: errors.New("the setup test hasn't finished; check with: clawbridge dispatcher model status")}
	}
	return errors.New("model setup failed (" + outcome + "); nothing was enabled")
}

// ExitCodeError is an error that ends the command with its own exit code
// instead of 1.
type ExitCodeError struct {
	Code int
	Err  error
}

func (e *ExitCodeError) Error() string { return e.Err.Error() }
func (e *ExitCodeError) Unwrap() error { return e.Err }

func providerIDs() string {
	var ids []string
	for _, p := range dispatchermodel.Providers() {
		ids = append(ids, p.ID)
	}
	return strings.Join(ids, ", ")
}
