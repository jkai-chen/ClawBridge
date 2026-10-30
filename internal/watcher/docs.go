package watcher

import (
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
// content, as the provider's list_documents reads them. It asks for one end
// of the list, first or last 50, and reads which end it got from the page's
// own order (docPageEnd), so a burst of more than a page is read oldest end
// first over several turns, whichever way Linear orders it.
const documentsQuery = `query Documents($since: DateTimeOrDuration!, $first: Int, $last: Int) { documents(first: $first, last: $last, includeArchived: true, orderBy: updatedAt, filter: { updatedAt: { gt: $since } }) { nodes { id updatedAt team { id } project { id } initiative { id } issue { id } } pageInfo { hasNextPage hasPreviousPage } } }`

// docBurstMargin is how far below the newest change of a cut-off oldest
// page the cursor moves: the app's since is read to the millisecond
// (docSinceLayout), and a change in that same millisecond left off the page
// is read again, not skipped. Each document's own seen time keeps it from
// being told twice. More than a page in one millisecond can't move the
// cursor: the bus says burst_stalled.
const (
	docBurstMargin = time.Millisecond
	docSinceLayout = "2006-01-02T15:04:05.000Z07:00"
	docPageSize    = 50
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
	listStalled := false
	var sinceAt time.Time
	if t, e := time.Parse(time.RFC3339Nano, since); e == nil {
		sinceAt, since = t.UTC(), t.UTC().Format(time.RFC3339)
	}
	if page, oldest, stalled, e := w.documentsChanged(ctx, svc, since, sinceAt); e != nil {
		report["error"] = "list_failed"
	} else {
		list := []any{}
		for _, d := range page.Items {
			list = append(list, map[string]any{"id": d.ID, "updated_at": d.UpdatedAt, "team_id": d.TeamID, "project_id": d.ProjectID, "initiative_id": d.InitiativeID, "issue_id": d.IssueID})
		}
		report["documents"] = list
		// One call a tick: a page with more behind it leaves the cursor
		// where it is (W-L1), and what was told is not told again; an
		// oldest-first page moves it past what it read.
		report["complete"] = page.Complete || page.NextCursor == ""
		if advance := burstAdvance(page, oldest); advance != "" {
			report["advance_to"] = advance
		}
		listStalled = stalled
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
	if stalled, _ := data["stalled"].(bool); stalled || listStalled {
		poll.Error = "burst_stalled"
	}
	return poll, nil
}

// documentsChanged is the list call: one app query, or, once Linear has
// refused the query, the provider's list for the rest of this process.
// oldest says the page is known to hold the oldest changes since; stalled,
// that a cut-off page's changes all share one time, so no end is known.
func (w *Watcher) documentsChanged(ctx context.Context, svc *linearboard.Service, since string, sinceAt time.Time) (page linearprovider.Page[linearprovider.DocRef], oldest, stalled bool, err error) {
	w.mu.Lock()
	provider := w.docsByProvider
	w.mu.Unlock()
	if !provider {
		// The app's list reads since to the millisecond, so a burst moves
		// the cursor to within one of what it read (docBurstMargin).
		appSince := since
		if !sinceAt.IsZero() {
			appSince = sinceAt.Format(docSinceLayout)
		}
		page, oldest, stalled, err = w.documentsByApp(ctx, appSince)
		if !linearapp.IsQueryInvalid(err) {
			return page, oldest, stalled, err
		}
		w.mu.Lock()
		w.docsByProvider = true
		w.mu.Unlock()
	}
	page, err = svc.DocumentsChangedSince(ctx, since)
	return page, false, false, err
}

// docPageEnd reads a page's order, which is the list's (a page keeps the
// list's order whichever end it was cut from): 1 oldest first, -1 newest
// first, 0 when every change shares one time or the times disagree.
func docPageEnd(items []linearprovider.DocRef) int {
	end := 0
	for k := 1; k < len(items); k++ {
		a, errA := time.Parse(time.RFC3339Nano, items[k-1].UpdatedAt)
		b, errB := time.Parse(time.RFC3339Nano, items[k].UpdatedAt)
		if errA != nil || errB != nil {
			return 0
		}
		step := b.Compare(a)
		if step == 0 {
			continue
		}
		if end != 0 && step != end {
			return 0
		}
		end = step
	}
	return end
}

// burstAdvance is where a cut-off oldest-first page moves the cursor: below
// its newest change by docBurstMargin, everything changed since was on the
// page. "" for a complete page, the provider's newest-first one, or a page
// whose times don't parse.
func burstAdvance(page linearprovider.Page[linearprovider.DocRef], oldest bool) string {
	if page.Complete || !oldest || len(page.Items) == 0 {
		return ""
	}
	var newest time.Time
	for _, d := range page.Items {
		at, err := time.Parse(time.RFC3339Nano, d.UpdatedAt)
		if err != nil {
			return ""
		}
		if at.After(newest) {
			newest = at
		}
	}
	return newest.Add(-docBurstMargin).UTC().Format(time.RFC3339Nano)
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

// documentsByApp reads one end of the list: last 50 until a page shows the
// list runs oldest first, then first 50 (Watcher.docsFirst). oldest says
// the page is the oldest end; a cut-off page from the newest end turns the
// next read to the other end, and so does one whose order can't be read,
// which is stalled.
func (w *Watcher) documentsByApp(ctx context.Context, since string) (page linearprovider.Page[linearprovider.DocRef], oldest, stalled bool, err error) {
	w.mu.Lock()
	fromFirst := w.docsFirst
	w.mu.Unlock()
	vars := map[string]any{"since": since, "first": nil, "last": docPageSize}
	if fromFirst {
		vars["first"], vars["last"] = docPageSize, nil
	}
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
			PageInfo struct {
				HasNextPage     bool `json:"hasNextPage"`
				HasPreviousPage bool `json:"hasPreviousPage"`
			} `json:"pageInfo"`
		} `json:"documents"`
	}
	if err = w.App.GraphQL(ctx, documentsQuery, vars, &out); err != nil {
		return page, false, false, err
	}
	// A page with more behind it, at either end, must say so, or the bus
	// would read it as complete and move past what it hasn't seen.
	page.Complete = !out.Documents.PageInfo.HasNextPage && !out.Documents.PageInfo.HasPreviousPage
	if !page.Complete {
		page.NextCursor = "more"
	}
	for _, n := range out.Documents.Nodes {
		if !linearprovider.ValidID(n.ID) {
			return linearprovider.Page[linearprovider.DocRef]{}, false, false, errors.New("a document's ID is not a Linear ID")
		}
		ref := linearprovider.DocRef{ID: n.ID, UpdatedAt: n.UpdatedAt}
		for dst, r := range map[*string]*idRef{&ref.TeamID: n.Team, &ref.ProjectID: n.Project, &ref.InitiativeID: n.Initiative, &ref.IssueID: n.Issue} {
			if *dst, err = r.id(); err != nil {
				return linearprovider.Page[linearprovider.DocRef]{}, false, false, err
			}
		}
		page.Items = append(page.Items, ref)
	}
	if page.Complete {
		return page, false, false, nil
	}
	// The cut must be at the far end from the one asked for: more before
	// a last page, more after a first one. A page cut at the other end was
	// served from the wrong end, so nothing about it is trusted: it is told,
	// the cursor stays, and the poll is stalled.
	info := out.Documents.PageInfo
	cutAtFarEnd := info.HasPreviousPage && !info.HasNextPage
	if fromFirst {
		cutAtFarEnd = info.HasNextPage && !info.HasPreviousPage
	}
	if !cutAtFarEnd {
		return page, false, true, nil
	}
	end := docPageEnd(page.Items)
	// last of a newest-first list, or first of an oldest-first one, is
	// the oldest end.
	oldest = (end == -1 && !fromFirst) || (end == 1 && fromFirst)
	if !oldest {
		w.mu.Lock()
		w.docsFirst = !fromFirst
		w.mu.Unlock()
	}
	return page, oldest, end == 0, nil
}
