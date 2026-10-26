package watcher

import (
	"context"
	"time"

	"clawbridge/internal/bus"
	"clawbridge/internal/linearboard"
)

// Document change notices (live/LINEAR-DOCS-DESIGN.md §4): after the
// comment watch, markers, intake and health, a tick makes the one
// document call the bus hands out, if any (LD-M2): a workspace-wide list
// of documents changed since the project's cursor, or one read of a
// document ClawBridge published. It reads as the Linear app; the bus
// filters the list to the project's scope and tells the sessions.

// DocPoll is what one tick's document call did.
type DocPoll struct {
	Action string `json:"action"`
	Told   int    `json:"told,omitempty"`
	Edited bool   `json:"edited,omitempty"`
	Error  string `json:"error,omitempty"`
}

// docs makes the tick's document call, if one is due.
func (w *Watcher) docs(ctx context.Context, svc *linearboard.Service, pid string, fence map[string]any) (*DocPoll, error) {
	fenced := func(a map[string]any) map[string]any {
		for k, v := range fence {
			a[k] = v
		}
		return a
	}
	r, err := w.Store.ExecuteSystem(ctx, pid, bus.Request{Operation: "doc_watch_due", Args: fenced(map[string]any{})})
	if err != nil {
		return nil, err
	}
	due, _ := r.Data.(map[string]any)
	action, _ := due["action"].(string)
	if action != "list" && action != "verify" {
		return nil, nil
	}
	poll := &DocPoll{Action: action}
	if action == "verify" {
		id, _ := due["document_id"].(string)
		report := fenced(map[string]any{"document_id": id})
		if digest, e := svc.PublishedDocumentDigest(ctx, id); e != nil {
			report["error"] = "unreadable"
		} else {
			report["content_sha256"] = digest
		}
		out, e := w.Store.ExecuteSystem(ctx, pid, bus.Request{Operation: "doc_verify_report", Args: report})
		if e != nil {
			return poll, e
		}
		data, _ := out.Data.(map[string]any)
		poll.Edited, _ = data["edited"].(bool)
		poll.Error, _ = report["error"].(string)
		return poll, nil
	}
	since, _ := due["since"].(string)
	report := fenced(map[string]any{"polled_at": due["polled_at"]})
	if t, e := time.Parse(time.RFC3339Nano, since); e == nil {
		since = t.UTC().Format(time.RFC3339)
	}
	if page, e := svc.DocumentsChangedSince(ctx, since); e != nil {
		report["error"] = "list_failed"
	} else {
		list := []any{}
		for _, d := range page.Items {
			list = append(list, map[string]any{"id": d.ID, "updated_at": d.UpdatedAt, "team_id": d.TeamID, "project_id": d.ProjectID, "initiative_id": d.InitiativeID, "issue_id": d.IssueID})
		}
		report["documents"] = list
		// One call a tick: a page with more behind it leaves the cursor
		// where it is (W-L1), and what was told is not told again.
		report["complete"] = page.Complete || page.NextCursor == ""
	}
	out, e := w.Store.ExecuteSystem(ctx, pid, bus.Request{Operation: "doc_watch_report", Args: report})
	if e != nil {
		return poll, e
	}
	data, _ := out.Data.(map[string]any)
	if n, ok := data["told"].(int); ok {
		poll.Told = n
	}
	poll.Error, _ = report["error"].(string)
	return poll, nil
}
