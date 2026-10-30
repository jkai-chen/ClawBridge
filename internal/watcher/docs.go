package watcher

import (
	"cmp"
	"context"
	"errors"
	"time"

	"clawbridge/internal/bus"
	"clawbridge/internal/linearapp"
	"clawbridge/internal/linearboard"
	"clawbridge/internal/linearprovider"
)

// documentsQuery is the list through the app: the documents changed since,
// workspace-wide, archived ones too, with their parents and never their
// content, as the provider's list_documents reads them.
const documentsQuery = `query Documents($since: DateTimeOrDuration!) { documents(first: 50, includeArchived: true, orderBy: updatedAt, filter: { updatedAt: { gt: $since } }) { nodes { id updatedAt team { id } project { id } initiative { id } issue { id } } pageInfo { hasNextPage endCursor } } }`

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
	if page, e := w.documentsChanged(ctx, svc, since); e != nil {
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

// documentsChanged is the list call: one app query, or, once Linear has
// refused the query, the provider's list for the rest of this process.
func (w *Watcher) documentsChanged(ctx context.Context, svc *linearboard.Service, since string) (linearprovider.Page[linearprovider.DocRef], error) {
	w.mu.Lock()
	provider := w.docsByProvider
	w.mu.Unlock()
	if !provider {
		page, err := w.documentsByApp(ctx, since)
		if !linearapp.IsQueryInvalid(err) {
			return page, err
		}
		w.mu.Lock()
		w.docsByProvider = true
		w.mu.Unlock()
	}
	return svc.DocumentsChangedSince(ctx, since)
}

type idRef struct {
	ID string `json:"id"`
}

func (r *idRef) id() (string, error) {
	if r == nil {
		return "", nil
	}
	if !linearprovider.ValidID(r.ID) {
		return "", errors.New("a document's parent is not a Linear ID")
	}
	return r.ID, nil
}

func (w *Watcher) documentsByApp(ctx context.Context, since string) (linearprovider.Page[linearprovider.DocRef], error) {
	var out struct {
		Documents struct {
			Nodes []struct {
				ID         string `json:"id"`
				UpdatedAt  string `json:"updatedAt"`
				Team       *idRef `json:"team"`
				Project    *idRef `json:"project"`
				Initiative *idRef `json:"initiative"`
				Issue      *idRef `json:"issue"`
			} `json:"nodes"`
			PageInfo pageInfo `json:"pageInfo"`
		} `json:"documents"`
	}
	if err := w.App.GraphQL(ctx, documentsQuery, map[string]any{"since": since}, &out); err != nil {
		return linearprovider.Page[linearprovider.DocRef]{}, err
	}
	// A page with more behind it must say so with its cursor, or the bus
	// would read it as complete and move past what it hasn't seen.
	page := linearprovider.Page[linearprovider.DocRef]{Complete: !out.Documents.PageInfo.HasNextPage}
	if !page.Complete {
		page.NextCursor = cmp.Or(out.Documents.PageInfo.EndCursor, "more")
	}
	for _, n := range out.Documents.Nodes {
		if !linearprovider.ValidID(n.ID) {
			return linearprovider.Page[linearprovider.DocRef]{}, errors.New("a document's ID is not a Linear ID")
		}
		ref := linearprovider.DocRef{ID: n.ID, UpdatedAt: n.UpdatedAt}
		var err error
		for dst, r := range map[*string]*idRef{&ref.TeamID: n.Team, &ref.ProjectID: n.Project, &ref.InitiativeID: n.Initiative, &ref.IssueID: n.Issue} {
			if *dst, err = r.id(); err != nil {
				return linearprovider.Page[linearprovider.DocRef]{}, err
			}
		}
		page.Items = append(page.Items, ref)
	}
	return page, nil
}
