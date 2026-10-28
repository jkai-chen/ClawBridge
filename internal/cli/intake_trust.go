package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"clawbridge/internal/bus"
	"clawbridge/internal/clirender"
	"clawbridge/internal/linearboard"
	"clawbridge/internal/linearprovider"
)

// resolveTrustee turns the Linear name or email typed after intake trust
// add|remove into the member's Linear user ID. A name that matches
// nobody, or several people, is refused with what was found.
func resolveTrustee(ctx context.Context, executor linearboard.Executor, config Config, who string) (linearprovider.Member, error) {
	members, err := trusteeMembers(ctx, executor, config)
	if err != nil {
		return linearprovider.Member{}, err
	}
	return matchTrustee(members, who, trusteeEmails())
}

// trusteeEmails is whether a member's email may be printed: never under
// an agent's environment, where the output goes to the agent.
func trusteeEmails() bool { return agentMarker(closesEnv()) == "" }

// matchTrustee finds who among the active members, by name or email,
// either case. withEmail says whether its errors may name emails.
func matchTrustee(members []linearprovider.Member, who string, withEmail bool) (linearprovider.Member, error) {
	who = strings.TrimSpace(who)
	found := map[string]linearprovider.Member{}
	inactive := 0
	for _, m := range members {
		if strings.EqualFold(strings.TrimSpace(m.Name), who) || (m.Email != "" && strings.EqualFold(m.Email, who)) {
			if !m.Active {
				// A deactivated Linear user can't add a label or
				// delegate; trusting one is a mistake waiting to happen.
				inactive++
				continue
			}
			found[m.ID] = m
		}
	}
	switch len(found) {
	case 0:
		if inactive > 0 {
			return linearprovider.Member{}, fmt.Errorf("%q is not an active member of this project's Linear teams; give their Linear user ID if you mean them anyway", who)
		}
		return linearprovider.Member{}, fmt.Errorf("no member of this project's Linear teams is named %q or has that email; check the spelling, or give their Linear user ID", who)
	case 1:
		for _, m := range found {
			return m, nil
		}
	}
	names := []string{}
	for _, m := range found {
		names = append(names, trusteeLine(m, withEmail))
	}
	sort.Strings(names)
	return linearprovider.Member{}, fmt.Errorf("%q matches %d Linear members: %s; give the one you mean by email or user ID", who, len(found), strings.Join(names, "; "))
}

// trusteeMembers lists the members of the project's bound teams through
// this person's own Linear connection (--linear-profile, or the only one).
// Nothing is stored.
var trusteeMembers = func(ctx context.Context, executor linearboard.Executor, config Config) ([]linearprovider.Member, error) {
	directory, err := linearDirectory()
	if err != nil {
		return nil, err
	}
	manager := newLinearManager(directory)
	profile := config.LinearProfile
	if profile == "" {
		profiles, e := manager.Profiles()
		if e != nil {
			return nil, e
		}
		if len(profiles) != 1 {
			return nil, errors.New("to find a Linear user by name or email, name your Linear connection with --linear-profile (clawbridge linear profiles), or give their Linear user ID")
		}
		profile = profiles[0].ID
	}
	bounded, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	service := newLinearService(directory, profile, manager, executor, linearAuthority(config))
	scope := bus.Request{Workspace: config.Workspace, Project: config.Project, Session: config.Session, Args: map[string]any{}}
	if config.Instance != "" {
		scope.Args["instance"] = config.Instance
	}
	binding, err := service.BindingV2(bounded, scope)
	if err != nil {
		return nil, err
	}
	if !binding.Configured || len(binding.LinearTeamIDs) == 0 {
		return nil, errors.New("this project has no Linear board linked, so there are no members to find by name; give their Linear user ID")
	}
	out := []linearprovider.Member{}
	for _, team := range binding.LinearTeamIDs {
		cursor := ""
		for {
			page, e := service.MembersV2(bounded, scope, team, 100, cursor)
			if e != nil {
				return nil, e
			}
			out = append(out, page.Items...)
			if page.Complete || page.NextCursor == "" {
				break
			}
			cursor = page.NextCursor
		}
	}
	return out, nil
}

// trusteeLine names a member for a person: name, email when known and
// allowed, ID. The name and email are other people's text, so they're
// escaped (R1): they print on the very screen the person confirms from.
func trusteeLine(m linearprovider.Member, withEmail bool) string {
	line := clirender.Safe(m.Name)
	if withEmail && m.Email != "" {
		line += " <" + clirender.Safe(m.Email) + ">"
	}
	return line + " (" + m.ID + ")"
}

// confirmTrustee says who a name resolved to, and changes nothing until
// --confirm. The confirmed run says it again: a rename between the runs
// could have changed who the name means.
func confirmTrustee(stderr io.Writer, op string, m linearprovider.Member, confirmed bool) error {
	verb, doing := "trust", "Trusting"
	if op == "intake_trust_remove" {
		verb, doing = "stop trusting", "No longer trusting"
	}
	line := trusteeLine(m, trusteeEmails())
	if !confirmed {
		_, _ = fmt.Fprintf(stderr, "That is %s.\n", line)
		return fmt.Errorf("nothing changed: rerun with --confirm to %s them", verb)
	}
	_, _ = fmt.Fprintf(stderr, "%s %s.\n", doing, line)
	return nil
}

// trustLinkNote says whether trust add linked the person to a ClawBridge
// principal (Q2): only a linked person's own writes through ClawBridge, from
// their own terminal, count as theirs.
func trustLinkNote(stderr io.Writer, a map[string]any, result bus.Result) {
	user, _ := a["linear_user_id"].(string)
	rows := []map[string]any{}
	switch data := result.Data.(type) {
	case []map[string]any:
		rows = data
	case []any:
		for _, v := range data {
			if row, ok := v.(map[string]any); ok {
				rows = append(rows, row)
			}
		}
	}
	linked := ""
	for _, row := range rows {
		if id, _ := row["linear_user_id"].(string); strings.EqualFold(id, user) {
			linked, _ = row["principal"].(string)
		}
	}
	switch {
	case linked != "" && trusteeEmails():
		_, _ = fmt.Fprintf(stderr, "Linked to principal %s: their own writes through ClawBridge, from their own terminal, count as theirs.\n", clirender.Safe(linked))
	case linked != "":
		_, _ = fmt.Fprintln(stderr, "Linked to a ClawBridge principal: their own writes through ClawBridge, from their own terminal, count as theirs.")
	case a["link_email"] != nil:
		_, _ = fmt.Fprintln(stderr, "Not linked: no member of this ClawBridge project has that email. Their writes through ClawBridge don't count as theirs; add --principal EMAIL to link them.")
	default:
		_, _ = fmt.Fprintln(stderr, "Not linked to a ClawBridge principal: their writes through ClawBridge don't count as theirs; add --principal EMAIL to link them.")
	}
}
