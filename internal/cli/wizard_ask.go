package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"clawbridge/internal/clirender"
)

// askTries is how many answers a guided question takes before the flow
// stops (TUI-UX-AUDIT U45).
const askTries = 3

// askUntil asks until check accepts an answer, and says why after each
// wrong one, so one typo no longer ends the flow (U45, U46). check returns
// the value to use, or why the answer can't be used; the reason never
// repeats the answer. After askTries wrong answers, or when input ends
// after a wrong one, it stops with the last reason.
func askUntil(ask func(string) (string, error), out io.Writer, question string, check func(string) (string, error)) (string, error) {
	var why error
	for try := 1; try <= askTries; try++ {
		answer, err := ask(question)
		if err != nil {
			if why != nil && errors.Is(err, io.EOF) {
				break
			}
			return "", err
		}
		value, e := check(answer)
		if e == nil {
			return value, nil
		}
		why = e
		if try < askTries {
			fmt.Fprintf(out, "  %s. Try again.\n", clirender.Safe(e.Error()))
		}
	}
	return "", fmt.Errorf("%w; run clawbridge onboard to start again", why)
}

// The numbered questions' choices. A word means one choice only, or the
// answer would depend on map order (TestPickWordsAreUnique).
var (
	cloudChoices = map[string][]string{"gcp": {"1", "gcp"}, "aws": {"2", "aws"}, "local": {"3", "local"}}
	shapeChoices = map[string][]string{"solo": {"1", "solo"}, "lan": {"2", "lan"}}
)

// pick reads a numbered choice: its number, or one of its words.
func pick(answer, fallback string, choices map[string][]string) (string, bool) {
	if answer == "" {
		answer = fallback
	}
	answer = strings.ToLower(answer)
	for value, words := range choices {
		for _, w := range words {
			if answer == w {
				return value, true
			}
		}
	}
	return "", false
}

// openNow ends every Start path (U48): after the summary it waits for the
// person (U52), and opens ClawBridge on a yes. Ended input is a no. The
// prompt is closed first: no wizard reader may share stdin with the TUI.
func openNow(ctx context.Context, path string, stdout, stderr io.Writer) error {
	prompt, err := newWizardPrompt(ctx, stdout, "guided terminal input cleanup failed; everything is saved, restore the terminal and run clawbridge")
	if err != nil {
		return err
	}
	answer, err := prompt.ask("\nOpen ClawBridge now? [Y/n]: ")
	if e := prompt.close(); e != nil {
		return e
	}
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		answer = "n"
	}
	if a := strings.ToLower(answer); a != "" && a != "y" && a != "yes" {
		fmt.Fprintln(stdout, "Run clawbridge any time to open it.")
		return nil
	}
	if err = wizardTUI(context.WithValue(ctx, guidedRouteKey{}, true), Config{ClientConfig: path}, stdout, stderr); err != nil {
		return fmt.Errorf("everything is saved, but ClawBridge didn't open: %w", err)
	}
	return nil
}
