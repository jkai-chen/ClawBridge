package cli

import (
	"encoding/json"
	"strconv"
	"strings"
	"time"

	"clawbridge/internal/bus"
	"clawbridge/internal/clirender"
	"clawbridge/internal/linearapp"
	"clawbridge/internal/linearboard"
)

func init() { clirender.Register("linear_issue_history", issueHistoryView) }

// issueHistoryView is `linear issue history`: one line per change, newest
// first, TIME ACTOR: CHANGE[; CHANGE…], then the cursor of the next page
// when more follow.
func issueHistoryView(v any) (clirender.Doc, bool) {
	if r, ok := v.(bus.Result); ok {
		v = r.Data
	}
	raw, err := json.Marshal(v)
	if err != nil {
		return clirender.Doc{}, false
	}
	var page linearboard.IssueHistoryPageV2
	if json.Unmarshal(raw, &page) != nil || page.IssueUUID == "" {
		return clirender.Doc{}, false
	}
	var d clirender.Doc
	if len(page.Entries) == 0 {
		d.Text("No history.")
	}
	// Whole lines: one per entry, so they read and grep as lines, and an
	// ID or the cursor is never broken across two.
	for _, e := range page.Entries {
		d.Whole(historyLine(e))
	}
	if page.HasMore && page.NextCursor != "" {
		d.Whole("next: --cursor " + page.NextCursor)
	}
	return d, true
}

// historyLine is one entry: its time in UTC to the minute, who made it
// and what it changed.
func historyLine(e linearapp.HistoryEntry) string {
	at := e.At
	if t, err := time.Parse(time.RFC3339Nano, e.At); err == nil {
		at = t.UTC().Format("2006-01-02 15:04Z")
	}
	actor := e.Actor.Kind
	if e.Actor.Name != "" {
		actor += " " + e.Actor.Name
	} else if e.Actor.ID != "" {
		actor += " " + e.Actor.ID
	}
	return at + " " + actor + ": " + strings.Join(historyChanges(e), "; ")
}

// historyChanges are an entry's changes in the order the entry lists
// them; an entry with none is "other change".
func historyChanges(e linearapp.HistoryEntry) []string {
	var out []string
	pair := func(field, from, to string) {
		// A move between two places the binding doesn't cover says only that.
		if from == historyOutside && to == historyOutside {
			out = append(out, field+" changed outside this project's binding")
			return
		}
		out = append(out, field+" "+from+" → "+to)
	}
	if c := e.State; c != nil {
		name := func(s *linearapp.HistoryState) string {
			if s == nil {
				return "none"
			}
			return firstNonEmpty(s.Name, s.ID)
		}
		pair("state", name(c.From), name(c.To))
	}
	if c := e.Assignee; c != nil {
		pair("assignee", historyNamed(c.From), historyNamed(c.To))
	}
	if l := e.Labels; l != nil {
		var parts []string
		for _, x := range l.Added {
			parts = append(parts, "+"+firstNonEmpty(x.Name, x.ID))
		}
		for _, x := range l.Removed {
			parts = append(parts, "-"+firstNonEmpty(x.Name, x.ID))
		}
		out = append(out, strings.TrimSpace("labels "+strings.Join(parts, " ")))
	}
	if c := e.Team; c != nil {
		key := func(t *linearapp.HistoryTeam) string {
			switch {
			case t == nil:
				return "none"
			case t.Outside:
				return historyOutside
			}
			return firstNonEmpty(t.Key, t.ID)
		}
		pair("team", key(c.From), key(c.To))
	}
	if c := e.Project; c != nil {
		pair("project", historyNamed(c.From), historyNamed(c.To))
	}
	// As the server does: a priority both sides name and that differs.
	if c := e.Priority; c != nil && c.From != nil && c.To != nil && *c.From != *c.To {
		pair("priority", historyPriority(c.From), historyPriority(c.To))
	}
	if c := e.Parent; c != nil {
		parent := func(p *linearapp.HistoryParent) string {
			if p == nil {
				return "none"
			}
			return firstNonEmpty(p.Identifier, p.ID)
		}
		pair("parent", parent(c.From), parent(c.To))
	}
	// Only true is a change: Linear may send false on any entry.
	if e.Archived != nil && *e.Archived {
		out = append(out, "archived")
	}
	if e.Trashed != nil && *e.Trashed {
		out = append(out, "trashed")
	}
	if len(out) == 0 {
		out = append(out, "other change")
	}
	return out
}

// historyOutside is a team or project side the server hid: it is outside
// the project's binding.
const historyOutside = "(outside this project's binding)"

func historyNamed(n *linearapp.HistoryNamed) string {
	switch {
	case n == nil:
		return "none"
	case n.Outside:
		return historyOutside
	}
	return firstNonEmpty(n.Name, n.ID)
}

// historyPriority is Linear's priority by name.
func historyPriority(p *float64) string {
	if p == nil {
		return "none"
	}
	names := []string{"none", "urgent", "high", "medium", "low"}
	if n := int(*p); float64(n) == *p && n >= 0 && n < len(names) {
		return names[n]
	}
	return strconv.FormatFloat(*p, 'f', -1, 64)
}
