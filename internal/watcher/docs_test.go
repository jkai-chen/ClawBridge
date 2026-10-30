package watcher

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"clawbridge/internal/bus"
	"clawbridge/internal/linearprovider"
)

const (
	docTeam    = "d0c00000-1111-4000-8000-000000000001"
	docForeign = "d0c00000-2222-4000-8000-000000000002"
	docPublish = "d0c00000-3333-4000-8000-000000000003"
	strangerTm = "99999999-9999-4999-8999-999999999999"
)

func (p fakeProvider) DocumentsUpdatedSince(_ context.Context, since, _ string) (linearprovider.Page[linearprovider.DocRef], error) {
	p.f.events = append(p.f.events, "docs-list")
	p.f.providerDocLists++
	return linearprovider.Page[linearprovider.DocRef]{Items: p.f.docRefs, Complete: !p.f.docMore, NextCursor: map[bool]string{true: "more"}[p.f.docMore]}, nil
}
func (p fakeProvider) GetDocument(_ context.Context, id string) (linearprovider.Document, error) {
	p.f.events = append(p.f.events, "docs-get")
	return linearprovider.Document{ID: id, Content: p.f.docContent[id]}, nil
}

func (e *env) docPolicy() {
	e.t.Helper()
	if _, err := e.s.Execute(context.Background(), bus.Request{Operation: "doc_policy_set", Workspace: "w", Project: "p", Args: map[string]any{"expected_revision": 0, "doc_policy": map[string]any{"initiatives": []any{}}}}); err != nil {
		e.t.Fatal(err)
	}
}

func (e *env) docCalls() int {
	n := 0
	for _, ev := range e.f.events {
		if strings.HasPrefix(ev, "docs-") {
			n++
		}
	}
	return n
}

// The document watch is off without a policy. With one, its first tick only
// starts the cursor; a due tick makes one list call, after the comment
// watch, and the coordinator hears of in-scope documents only.
func TestDocWatchListsAfterTheCommentWatch(t *testing.T) {
	e := setup(t)
	proposed := e.call("assignment_propose", "eng", map[string]any{"issue_uuid": issueA, "issue_identifier": "FIX-1"})
	e.call("assignment_confirm", "coord", map[string]any{"target_session": "eng", "revision": proposed["assignment"].(map[string]any)["revision"]})
	e.f.docRefs = []linearprovider.DocRef{{ID: docTeam, UpdatedAt: "2026-10-26T10:00:00Z", TeamID: team}, {ID: docForeign, UpdatedAt: "2026-10-26T10:00:00Z", TeamID: strangerTm}}
	e.dueNow()
	e.tick()
	if e.docCalls() != 0 {
		t.Fatal("listed without a document policy")
	}
	e.docPolicy()
	if tick := e.tick(); tick.Docs != nil || e.docCalls() != 0 {
		t.Fatal("the first tick listed", tick.Docs)
	}
	e.dueNow()
	e.f.events = nil
	tick := e.tick()
	if tick.Error != "" || tick.Docs == nil || tick.Docs.Action != "list" || tick.Docs.Told != 1 || e.docCalls() != 1 {
		t.Fatal(tick.Docs, tick.Error, e.f.events)
	}
	last := -1
	for i, ev := range e.f.events {
		if ev == "comments" {
			last = i
		}
	}
	if last < 0 || e.f.events[len(e.f.events)-1] != "docs-list" {
		t.Fatal("the document call did not come after the comment watch:", e.f.events)
	}
	notes := e.inbox("coord", "linear-doc")
	if len(notes) != 1 || !strings.Contains(fmt.Sprint(notes[0]["body"]), docTeam) || strings.Contains(fmt.Sprint(notes[0]["body"]), docForeign) {
		t.Fatal(notes)
	}
	// Within the five minutes, ticks make no document call at all.
	for i := 0; i < 5; i++ {
		e.tick()
	}
	if e.docCalls() != 1 {
		t.Fatal("more than one document call in the window:", e.docCalls())
	}
}

// A tick never makes more than one document call, even with a published
// document to read back and a list due; an edited one is flagged.
func TestDocWatchOneCallPerTickAndPublishedEdits(t *testing.T) {
	e := setup(t)
	e.docPolicy()
	sum := sha256.Sum256([]byte("as published"))
	digest := hex.EncodeToString(sum[:])
	commit := strings.Repeat("a", 40)
	for _, state := range []string{"pending", "published"} {
		a := map[string]any{"parent_kind": "team", "parent_id": team, "source_path": "docs/plan.md", "part_number": 1, "part_count": 1, "generation": 1, "state": state, "publish_token": "aaaaaaaaaaaa", "expect_token": "", "source_commit": commit, "source_sha256": digest}
		if state == "published" {
			a["expect_token"], a["document_id"], a["published_sha256"] = "aaaaaaaaaaaa", docPublish, digest
		}
		if _, err := e.s.Execute(context.Background(), bus.Request{Operation: "doc_publication_put", Workspace: "w", Project: "p", Args: a}); err != nil {
			t.Fatal(err)
		}
	}
	e.tick()
	e.f.docRefs = []linearprovider.DocRef{{ID: docPublish, UpdatedAt: "2026-10-26T10:00:00Z", TeamID: team}}
	e.f.docContent = map[string]string{docPublish: "as published, then edited in Linear"}
	e.dueNow()
	e.f.events = nil
	if tick := e.tick(); tick.Docs == nil || tick.Docs.Action != "list" || e.docCalls() != 1 {
		t.Fatal(tick.Docs)
	}
	e.dueNow()
	e.f.events = nil
	tick := e.tick()
	if tick.Docs == nil || tick.Docs.Action != "verify" || !tick.Docs.Edited || e.docCalls() != 1 {
		t.Fatal("verify tick", tick.Docs, e.f.events)
	}
	found := false
	for _, n := range e.inbox("coord", "linear-doc") {
		found = found || strings.Contains(fmt.Sprint(n["body"]), "published_doc_edited")
	}
	if !found {
		t.Fatal("the edit was not flagged")
	}
}

// docCursor is the project's document list cursor.
func (e *env) docCursor() string {
	e.t.Helper()
	var at string
	e.queryRow(`SELECT cursor_time FROM watch_cursors WHERE source='doc-workspace' AND subject=''`, &at)
	return at
}

// The list is one app query, not the provider's list. A page with more
// behind it moves the cursor only to just below its newest change; a full
// one moves it to the poll.
func TestDocWatchListsThroughTheApp(t *testing.T) {
	e := setup(t)
	base := e.burstDocs(3, func(i int) time.Duration { return time.Duration(i) * 10 * time.Second })
	e.f.docMore = true
	e.dueNow()
	from := len(e.f.queries)
	if tick := e.tick(); tick.Docs == nil || tick.Docs.Action != "list" || tick.Docs.Error != "" {
		t.Fatal(tick.Docs)
	}
	if queries(e, from, documentsQuery) != 1 || e.f.providerDocLists != 0 {
		t.Fatalf("%d app queries, %d provider lists", queries(e, from, documentsQuery), e.f.providerDocLists)
	}
	partial := e.docCursor()
	if want := base.Add(20*time.Second - docBurstMargin).Format("2006-01-02T15:04:05.000000000Z"); partial != want {
		t.Fatal("a page with more behind it moved the cursor to", partial, "not", want)
	}
	e.f.docMore = false
	e.dueNow()
	e.tick()
	if e.docCursor() <= partial {
		t.Fatal("a full page left the cursor")
	}
}

// When Linear refuses the query, the list goes through the provider, for
// the rest of the process: the app isn't asked again.
func TestDocWatchFallsBackToTheProvider(t *testing.T) {
	e := setup(t)
	e.docPolicy()
	e.tick()
	e.f.docRefs = []linearprovider.DocRef{{ID: docTeam, UpdatedAt: "2026-10-26T10:00:00Z", TeamID: team}}
	e.f.refuseDocs = true
	for i := range 2 {
		e.dueNow()
		from := len(e.f.queries)
		tick := e.tick()
		if tick.Docs == nil || tick.Docs.Action != "list" || tick.Docs.Error != "" || e.f.providerDocLists != i+1 {
			t.Fatalf("tick %d: %+v, %d provider lists", i, tick.Docs, e.f.providerDocLists)
		}
		if want := map[int]int{0: 1, 1: 0}[i]; queries(e, from, documentsQuery) != want {
			t.Fatalf("tick %d asked the app %d times", i, queries(e, from, documentsQuery))
		}
	}
}

// A row whose ID or parent isn't a Linear ID fails the list, which tells
// nothing and keeps the cursor.
func TestDocWatchAppRowsAreChecked(t *testing.T) {
	for name, ref := range map[string]linearprovider.DocRef{
		"id":     {ID: "not-an-id", UpdatedAt: "2026-10-26T10:00:00Z", TeamID: team},
		"parent": {ID: docTeam, UpdatedAt: "2026-10-26T10:00:00Z", TeamID: "team-1"},
	} {
		t.Run(name, func(t *testing.T) {
			e := setup(t)
			e.docPolicy()
			e.tick()
			e.f.docRefs = []linearprovider.DocRef{ref}
			start := e.docCursor()
			e.dueNow()
			if tick := e.tick(); tick.Docs == nil || tick.Docs.Error != "list_failed" || e.docCursor() != start {
				t.Fatal(tick.Docs, e.docCursor(), start)
			}
		})
	}
}

// A burst of more than one page of changes is read oldest first over
// several turns, each moving the cursor past what it read: every document
// is told exactly once, and the last turn's full page moves the cursor to
// its poll.
func TestDocWatchReadsABurstOverSeveralTurns(t *testing.T) {
	for name, c := range map[string]struct {
		ascending bool
		turns     int
		ends      []string
	}{
		// Newest first: last is the oldest end from the start.
		"newest first": {false, 3, []string{"last", "last", "last"}},
		// Oldest first: last is the newest end, told but not moved past;
		// the next reads take first, the oldest end.
		"oldest first": {true, 4, []string{"last", "first", "first", "first"}},
	} {
		t.Run(name, func(t *testing.T) {
			e := setup(t)
			const burst = 120
			base := e.burstDocs(burst, func(i int) time.Duration { return time.Duration(i) * 10 * time.Second })
			e.f.docAscending, e.f.docEnds = c.ascending, nil
			told, turns := 0, 0
			for turns < 8 && told < burst {
				e.dueNow()
				tick := e.tick()
				if tick.Docs == nil || tick.Docs.Action != "list" || tick.Docs.Error != "" {
					t.Fatal(turns, tick.Docs)
				}
				turns++
				told += tick.Docs.Told
			}
			if told != burst || turns > c.turns || !slices.Equal(e.f.docEnds[:turns], c.ends[:turns]) {
				t.Fatalf("told %d of %d in %d turns, reading %v", told, burst, turns, e.f.docEnds)
			}
			var seen int
			e.queryRow(`SELECT COUNT(*) FROM watch_cursors WHERE source='doc'`, &seen)
			if seen != burst {
				t.Fatalf("%d documents seen of %d", seen, burst)
			}
			// Drain to complete: the cursor reaches past the burst, and a
			// later turn tells nothing.
			for range 3 {
				e.dueNow()
				if tick := e.tick(); tick.Docs == nil || tick.Docs.Told != 0 || tick.Docs.Error != "" {
					t.Fatal("told again:", tick.Docs)
				}
			}
			if e.docCursor() < base.Add(burst*10*time.Second).Format("2006-01-02T15:04:05") {
				t.Fatal("the cursor stayed at", e.docCursor())
			}
		})
	}
}

// burstDocs back-dates the document cursor and lists n in-scope documents
// changed after it, each at at(i) past a base time.
func (e *env) burstDocs(n int, at func(i int) time.Duration) time.Time {
	e.t.Helper()
	e.docPolicy()
	e.tick()
	base := time.Now().UTC().Add(-2 * time.Hour).Truncate(time.Second)
	e.sql(`UPDATE watch_cursors SET cursor_time=? WHERE source='doc-workspace' AND subject=''`, base.Add(-time.Hour).Format("2006-01-02T15:04:05.000000000Z"))
	e.f.docSince = true
	for i := range n {
		id := fmt.Sprintf("d0c00000-0000-4000-8000-%012d", i)
		e.f.docRefs = append(e.f.docRefs, linearprovider.DocRef{ID: id, UpdatedAt: base.Add(at(i)).Format(docSinceLayout), TeamID: team})
	}
	return base
}

// A document changed in the same second as the newest on a cut-off page,
// but left off it, is read on the next turn, not skipped.
func TestDocWatchBurstKeepsATieAtTheEdge(t *testing.T) {
	e := setup(t)
	e.burstDocs(51, func(i int) time.Duration { return time.Duration(min(i, 49)) * 10 * time.Second })
	told := 0
	for range 3 {
		e.dueNow()
		told += e.tick().Docs.Told
	}
	if told != 51 {
		t.Fatal("told", told, "of 51")
	}
}

// The provider's list is newest first, so a cut-off page of it says
// nothing about older changes: the cursor stays.
func TestDocWatchProviderBurstKeepsTheCursor(t *testing.T) {
	e := setup(t)
	e.burstDocs(5, func(i int) time.Duration { return time.Duration(i) * 10 * time.Second })
	e.f.refuseDocs, e.f.docMore = true, true
	start := e.docCursor()
	e.dueNow()
	if tick := e.tick(); tick.Docs == nil || tick.Docs.Error != "" || e.f.providerDocLists != 1 || e.docCursor() != start {
		t.Fatal(tick.Docs, e.f.providerDocLists, e.docCursor(), start)
	}
}

// A burst inside one second is read to the millisecond: 60 documents a
// millisecond apart are told in two polls.
func TestDocWatchBurstWithinOneSecond(t *testing.T) {
	e := setup(t)
	e.burstDocs(60, func(i int) time.Duration { return time.Duration(i) * time.Millisecond })
	told := 0
	for range 2 {
		e.dueNow()
		tick := e.tick()
		if tick.Docs == nil || tick.Docs.Error != "" {
			t.Fatal(tick.Docs)
		}
		told += tick.Docs.Told
	}
	if told != 60 {
		t.Fatal("told", told, "of 60 in two polls")
	}
}

// More than a page in one millisecond can't move the cursor: the poll
// says burst_stalled instead of staying silent.
func TestDocWatchBurstInOneMillisecondIsReported(t *testing.T) {
	e := setup(t)
	e.burstDocs(51, func(int) time.Duration { return 0 })
	e.dueNow()
	e.tick()
	start := e.docCursor()
	e.dueNow()
	if tick := e.tick(); tick.Docs == nil || tick.Docs.Error != "burst_stalled" || e.docCursor() != start {
		t.Fatal(tick.Docs, e.docCursor(), start)
	}
}

// docPageEnd reads a page's order, and says nothing for one it can't read.
func TestDocPageEnd(t *testing.T) {
	at := func(times ...string) []linearprovider.DocRef {
		var out []linearprovider.DocRef
		for _, s := range times {
			out = append(out, linearprovider.DocRef{UpdatedAt: s})
		}
		return out
	}
	const a, b, c = "2026-10-30T01:00:00.000Z", "2026-10-30T01:00:00.001Z", "2026-10-30T01:00:01.000Z"
	for name, tc := range map[string]struct {
		items []linearprovider.DocRef
		want  int
	}{
		"oldest first":            {at(a, b, b, c), 1},
		"newest first":            {at(c, b, b, a), -1},
		"one time":                {at(b, b, b), 0},
		"mixed":                   {at(a, c, b), 0},
		"unparsable":              {at(a, "yesterday", c), 0},
		"one row":                 {at(a), 0},
		"newest first, then rise": {at(c, a, b), 0},
	} {
		if got := docPageEnd(tc.items); got != tc.want {
			t.Errorf("%s: %d, want %d", name, got, tc.want)
		}
	}
}

// A list that answers last as first gives the newest end, cut at the end
// after it: the page is told, but the cursor doesn't move past what it
// didn't read, and the poll is stalled.
func TestDocWatchDistrustsAPageCutAtTheWrongEnd(t *testing.T) {
	e := setup(t)
	e.burstDocs(60, func(i int) time.Duration { return time.Duration(i) * 10 * time.Second })
	e.f.docLastAsFirst = true
	e.dueNow()
	e.tick()
	start := e.docCursor()
	for range 2 {
		e.dueNow()
		if tick := e.tick(); tick.Docs == nil || tick.Docs.Error != "burst_stalled" || e.docCursor() != start {
			t.Fatal(tick.Docs, e.docCursor(), start)
		}
	}
}
