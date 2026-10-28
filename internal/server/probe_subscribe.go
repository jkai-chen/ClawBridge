package server

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/mail"
	"strings"

	"clawbridge/internal/linearapp"
	"clawbridge/internal/linearprovider"
)

// The issue subscribers live check (live/ISSUE-SUBSCRIBERS-LIVECHECK.md,
// B2): on one hand-made fixture issue, the app subscribes the owner by
// email and optionally a second person by ID, and reads the subscribers
// back. It proves the app may subscribe someone else before any server is
// started with --issue-subscribers-proven. It sends no unsubscribe: the
// owner deletes the fixture by the printed ID.

// subscribeFixtureTitle is the fixture's exact title; any other issue is
// refused before a write.
const subscribeFixtureTitle = "ClawBridge subscribe probe, will be deleted"

// subscribeFixtureQuery reads what the guard and the read-back need. The
// subscribers' emails are compared in memory only, never printed.
const subscribeFixtureQuery = `query SubscribeProbe($id: String!) { issue(id: $id) { id title team { id } comments(first: 1) { nodes { id } } subscribers(first: 50) { nodes { id email } } } }`

// subscribeProbeApp is what the probe asks of the app.
type subscribeProbeApp interface {
	GraphQL(ctx context.Context, query string, variables map[string]any, out any) error
	SubscribeIssue(ctx context.Context, issue, email, user string) (string, error)
}

// newSubscribeProbeApp builds the app identity for the probe; tests replace
// it. Like newProbeApp, it never mints.
var newSubscribeProbeApp = func(store linearapp.Store) (subscribeProbeApp, error) {
	app, err := newProbeApp(store)
	if err != nil {
		return nil, err
	}
	a, ok := app.(*linearapp.App)
	if !ok {
		return nil, errors.New("the Linear app cannot subscribe people")
	}
	return a, nil
}

type subscribeFixture struct {
	Issue *struct {
		ID       string
		Title    string
		Team     struct{ ID string }
		Comments struct{ Nodes []struct{ ID string } }
		// Subscribers' emails stay in memory: the probe prints IDs only.
		Subscribers struct {
			Nodes []struct{ ID, Email string }
		}
	}
}

func (f subscribeFixture) has(email, user string) (byEmail, byID bool) {
	for _, s := range f.Issue.Subscribers.Nodes {
		byEmail = byEmail || email != "" && strings.EqualFold(s.Email, email)
		byID = byID || user != "" && strings.EqualFold(s.ID, user)
	}
	return byEmail, byID
}

// linearAppSubscribeProbe runs the live check. It refuses, sending nothing,
// unless the issue is the fixture: the exact title, in team, with no
// comment and no subscriber at all (L1: the owner, as its creator, was
// subscribed by Linear and unsubscribes in Linear first, or the email call
// would pass as a no-op). With dryRun it stops after the guard.
func linearAppSubscribeProbe(ctx context.Context, store linearapp.Store, issue, team, email, user string, dryRun bool, stdout io.Writer) error {
	issue, team, user = strings.ToLower(issue), strings.ToLower(team), strings.ToLower(user)
	if !linearprovider.ValidID(issue) || !linearprovider.ValidID(team) || email == "" || !strings.Contains(email, "@") || (user != "" && !linearprovider.ValidID(user)) {
		return errors.New("--subscribe needs --issue and --team UUIDs, --email, and an optional --user UUID")
	}
	app, err := newSubscribeProbeApp(store)
	if err != nil {
		return err
	}
	out := map[string]any{"issue_id": issue, "team_id": team}
	emit := func(ret error) error {
		if err := json.NewEncoder(stdout).Encode(map[string]any{"subscribe": out}); err != nil {
			return err
		}
		return ret
	}
	var before subscribeFixture
	if err = app.GraphQL(ctx, subscribeFixtureQuery, map[string]any{"id": issue}, &before); err != nil {
		return probeFailure("subscribe_fixture", err)
	}
	refuse := func(why string) error {
		return emit(errors.New("linear-app-probe subscribe: refused: " + why + "; nothing was sent"))
	}
	switch {
	case before.Issue == nil || !strings.EqualFold(before.Issue.ID, issue):
		return refuse("no such issue")
	case before.Issue.Title != subscribeFixtureTitle:
		return refuse("the issue isn't the fixture: its title must be exactly " + subscribeFixtureTitle)
	case !strings.EqualFold(before.Issue.Team.ID, team):
		// L3: the fixture's team, by ID, as the probe prints it.
		out["issue_team_id"] = strings.ToLower(before.Issue.Team.ID)
		return refuse("the issue is in another team than --team")
	case len(before.Issue.Comments.Nodes) > 0:
		return refuse("the fixture has a comment")
	case len(before.Issue.Subscribers.Nodes) > 0:
		out["subscribers_before"] = len(before.Issue.Subscribers.Nodes)
		return refuse("inconclusive: the fixture already has subscribers; its creator unsubscribes in Linear first")
	}
	out["subscribers_before"] = 0
	if dryRun {
		out["dry_run"] = true
		return emit(nil)
	}
	calls := map[string]any{}
	out["calls"] = calls
	pass := true
	class, err := app.SubscribeIssue(ctx, issue, email, "")
	if err != nil {
		class = probeClass(err)
	}
	calls["email"] = class
	pass = pass && class == "subscribed"
	if user != "" {
		class, err = app.SubscribeIssue(ctx, issue, "", user)
		if err != nil {
			class = probeClass(err)
		}
		calls["user"] = class
		pass = pass && class == "subscribed"
	}
	var after subscribeFixture
	if err = app.GraphQL(ctx, subscribeFixtureQuery, map[string]any{"id": issue}, &after); err != nil || after.Issue == nil {
		out["read_back"] = "failed"
		return emit(errors.New("linear-app-probe subscribe: the read-back failed; check the fixture's subscribers in Linear, then delete it by its ID"))
	}
	byEmail, byID := after.has(email, user)
	back := map[string]any{"owner_subscribed": byEmail, "subscribers": len(after.Issue.Subscribers.Nodes)}
	pass = pass && byEmail
	if user != "" {
		back["user_subscribed"] = byID
		pass = pass && byID
	}
	out["read_back"] = back
	out["pass"] = pass
	out["cleanup"] = "the owner deletes issue " + issue + " in Linear"
	return emit(nil)
}

// readProbeEmail reads --email-stdin's one line: an email address alone.
// The error names the input, never its value.
func readProbeEmail(r io.Reader) (string, error) {
	line, err := bufio.NewReader(io.LimitReader(r, 321)).ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return "", errors.New("could not read the email from stdin")
	}
	// The read stops at 321 bytes, over an address's 320; the address alone,
	// with no display name or brackets, parses back to itself.
	email := strings.TrimSpace(line)
	a, err := mail.ParseAddress(email)
	if err != nil || a.Address != email {
		return "", errors.New("stdin holds no email address (one line, the address alone)")
	}
	return email, nil
}
