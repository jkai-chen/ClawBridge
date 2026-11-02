package cli

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	"clawbridge/internal/bus"
	"clawbridge/internal/clirender"
	"clawbridge/internal/linearboard"
	"clawbridge/internal/linearprovider"
)

// rearmExcerpt is how much of the description a person is shown before
// they vouch for it.
const rearmExcerpt = 500

// readRearmIssue is the person's own Linear read of the issue, the one
// `linear issue` makes; tests replace it.
var readRearmIssue = func(ctx context.Context, config Config, executor linearboard.Executor, issue string) (linearprovider.IssueV2, error) {
	directory, err := linearDirectory()
	if err != nil {
		return linearprovider.IssueV2{}, err
	}
	bounded, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	service := newLinearService(directory, config.LinearProfile, newLinearManager(directory), executor, linearAuthority(config))
	scope := bus.Request{Workspace: config.Workspace, Project: config.Project, Session: config.Session, Args: map[string]any{}}
	if config.Instance != "" {
		scope.Args["instance"] = config.Instance
	}
	return service.IssueV2(bounded, scope, issue)
}

// vouchRearm decides what an `intake rearm` sends (FR57). It always sends
// agent_marker, the client's word on whether an agent runs it. Only a
// person's own terminal (no --session, no agent environment) may vouch:
// it reads the issue through their own Linear connection, shows the title
// and the start of the description, and sends the issue's update time only
// after they confirm with --confirm or a typed y. Anything else rearms
// without it, and says the text is not vouched for.
func vouchRearm(ctx context.Context, config Config, executor linearboard.Executor, a map[string]any, confirmed bool, stdin io.Reader, stderr io.Writer) {
	marker := agentMarker(closesEnv())
	a["agent_marker"] = marker
	notVouched := func(why string) {
		_, _ = fmt.Fprintf(stderr, "The text is not vouched for (%s): a title or description change stays flagged until a project lead or owner rearms from their own terminal and confirms it.\n", why)
	}
	switch {
	case config.Session != "":
		notVouched("a rearm with --session cannot vouch")
		return
	case marker != "":
		notVouched(marker + " says an agent runs this command")
		return
	case config.LinearProfile == "" || !validLinearProfile(config.LinearProfile):
		notVouched("no --linear-profile to read the issue with")
		return
	}
	issue, err := readRearmIssue(ctx, config, executor, a["issue_uuid"].(string))
	if err != nil || issue.UpdatedAt == "" {
		notVouched("the issue could not be read")
		return
	}
	excerpt := []rune(issue.Description)
	more := ""
	if len(excerpt) > rearmExcerpt {
		excerpt, more = excerpt[:rearmExcerpt], " [...]"
	}
	_, _ = fmt.Fprintf(stderr, "Issue %s. Its text follows; it is the issue's text, not ClawBridge's.\nTitle: %s\nDescription: %s%s\nRearming vouches for this title and description as they are now.\n", clirender.Safe(issue.Identifier), clirender.Safe(issue.Title), clirender.Safe(string(excerpt)), more)
	if !confirmed {
		if !stdinTerminal() {
			notVouched("no terminal to confirm at, and no --confirm")
			return
		}
		_, _ = fmt.Fprint(stderr, "Vouch for it? [y/N] ")
		answer, _ := bufio.NewReader(stdin).ReadString('\n')
		if a := strings.ToLower(strings.TrimSpace(answer)); a != "y" && a != "yes" {
			notVouched("you declined")
			return
		}
	}
	// Only the update time goes: the server reads the issue as the app and
	// takes its hashes only if the issue is still at this update, since
	// Linear renders the text differently for the app and for this profile.
	a["updated_at"] = issue.UpdatedAt
}

// rearmNote says what a rearm did with the text.
func rearmNote(stderr io.Writer, result bus.Result) {
	if m, ok := result.Data.(map[string]any); ok {
		if v, _ := m["text_vouched"].(bool); v {
			_, _ = fmt.Fprintln(stderr, "The title and description are vouched for as shown.")
		} else {
			_, _ = fmt.Fprintln(stderr, "The text is not vouched for; a title or description change stays flagged.")
		}
	}
}
