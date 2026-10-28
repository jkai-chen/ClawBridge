package watcher

import (
	"context"
	"slices"
	"strings"

	"clawbridge/internal/bus"
	"clawbridge/internal/linearboard"
	"clawbridge/internal/linearprovider"
)

// The subscribe pass (live/ISSUE-SUBSCRIBERS-PLAN.md §3): the project's due
// subscription rows, each subscribing its people to one issue as the app.
// It runs after the markers, within what the hour's budget leaves; the bus
// charged each row before it is handed out. Linear's text, and the owner's
// email, are never logged or recorded (B4): a row settles with a reason
// code only.

// SubscribePass is what one pass did, by count.
type SubscribePass struct {
	Done    int    `json:"done"`
	Skipped int    `json:"skipped"`
	Retry   int    `json:"retry"`
	Left    int    `json:"left"`
	Stopped string `json:"stopped,omitempty"`
}

// subscribeCheckQuery reads the one issue a row names: its team, to prove
// it is bound (B1), and which of the row's people are its active members
// (B5).
const subscribeCheckQuery = `query SubscribeCheck($id: String!, $users: [ID!]) { issue(id: $id) { id team { id members(filter: {id: {in: $users}, active: {eq: true}}, first: 10) { nodes { id } } } } }`

func (w *Watcher) subscribers(ctx context.Context, svc *linearboard.Service, scope bus.Request, pid string, fence map[string]any) (*SubscribePass, error) {
	return w.subscribePass(ctx, pid, fence, func() []string {
		bound := []string{}
		if b, err := svc.BindingV2(ctx, scope); err == nil {
			for _, t := range b.LinearTeamIDs {
				bound = append(bound, strings.ToLower(t))
			}
		}
		return bound
	})
}

// subscribePass applies the due rows; teams reads the bound teams once,
// only when a row is due.
func (w *Watcher) subscribePass(ctx context.Context, pid string, fence map[string]any, teams func() []string) (*SubscribePass, error) {
	pass := &SubscribePass{}
	due, err := w.Store.ExecuteSystem(ctx, pid, bus.Request{Operation: "subscribe_due", Args: copyArgs(fence, nil)})
	if err != nil {
		return pass, err
	}
	data, _ := due.Data.(map[string]any)
	rows, _ := data["rows"].([]any)
	pass.Left = asCount(data["left"])
	pass.Stopped, _ = data["stopped"].(string)
	if len(rows) == 0 {
		return pass, nil
	}
	bound := teams()
	for _, raw := range rows {
		row, _ := raw.(map[string]any)
		outcome, reason, calls, ownerMissing := w.subscribeRow(ctx, row, bound)
		settle := map[string]any{"issue_uuid": row["issue_uuid"], "outcome": outcome, "reason": reason, "calls": calls, "charged": row["charged"]}
		if ownerMissing {
			settle["owner_missing"] = true
		}
		if _, err := w.Store.ExecuteSystem(ctx, pid, bus.Request{Operation: "subscribe_settle", Args: copyArgs(fence, settle)}); err != nil {
			return pass, err
		}
		switch outcome {
		case "done":
			pass.Done++
		case "skipped":
			pass.Skipped++
		default:
			pass.Retry++
		}
	}
	return pass, nil
}

// subscribeRow subscribes one row's people. It returns the settle's outcome
// and reason, the Linear calls made, and whether Linear refused the owner's
// email.
func (w *Watcher) subscribeRow(ctx context.Context, row map[string]any, bound []string) (outcome, reason string, calls int, ownerMissing bool) {
	issue, _ := row["issue_uuid"].(string)
	email, _ := row["owner_email"].(string)
	wanted := []string{}
	if ids, ok := row["user_ids"].([]any); ok {
		for _, v := range ids {
			if id, ok := v.(string); ok {
				wanted = append(wanted, id)
			}
		}
	} else if ids, ok := row["user_ids"].([]string); ok {
		wanted = ids
	}
	var check struct {
		Issue *struct {
			ID   string
			Team struct {
				ID      string
				Members struct{ Nodes []struct{ ID string } }
			}
		}
	}
	calls = 1
	if err := w.App.GraphQL(ctx, subscribeCheckQuery, map[string]any{"id": issue, "users": wanted}, &check); err != nil {
		return "retry", "error", calls, false
	}
	switch {
	case check.Issue == nil || !strings.EqualFold(check.Issue.ID, issue):
		return "skipped", "not_found", calls, false
	case !slices.Contains(bound, strings.ToLower(check.Issue.Team.ID)):
		return "skipped", "not_bound", calls, false
	}
	members := map[string]bool{}
	for _, n := range check.Issue.Team.Members.Nodes {
		members[strings.ToLower(n.ID)] = true
	}
	noScope := false
	subscribe := func(email, user string) string {
		calls++
		class, err := w.App.SubscribeIssue(ctx, issue, email, user)
		if err != nil {
			return "unavailable"
		}
		if class == "scope" {
			noScope = true
		}
		return class
	}
	added, failed, left := 0, 0, false
	if email != "" {
		switch subscribe(email, "") {
		case "subscribed":
			added++
		case "not_found", "refused":
			// Linear knows no user by the owner's email.
			ownerMissing = true
		default:
			failed++
		}
	}
	for _, id := range wanted {
		if !linearprovider.ValidID(id) || !members[strings.ToLower(id)] {
			// No longer a member of the issue's team: left out (B5).
			left = true
			continue
		}
		if subscribe("", id) == "subscribed" {
			added++
		} else {
			failed++
		}
	}
	switch {
	case noScope && added == 0:
		// The app's token may not subscribe people: the live check (B2)
		// should have caught it; the row waits and says why.
		return "retry", "no_scope", calls, ownerMissing
	case failed > 0 && added == 0:
		// Nobody was added while someone could be: try the row again.
		return "retry", "error", calls, ownerMissing
	case ownerMissing && added == 0:
		// Linear refused the owner's email, which may be passing: the row
		// waits out the bus's memory of the refusal and tries again.
		return "retry", "owner_not_in_linear", calls, true
	case ownerMissing:
		return "done", "owner_not_in_linear", calls, true
	case failed > 0 || left:
		return "done", "partial", calls, false
	}
	return "done", "", calls, false
}

// copyArgs is fence plus extra, as one bus argument map.
func copyArgs(fence, extra map[string]any) map[string]any {
	out := map[string]any{}
	for k, v := range fence {
		out[k] = v
	}
	for k, v := range extra {
		out[k] = v
	}
	return out
}

func asCount(v any) int {
	switch n := v.(type) {
	case int:
		return n
	case int64:
		return int(n)
	case float64:
		return int(n)
	}
	return 0
}
