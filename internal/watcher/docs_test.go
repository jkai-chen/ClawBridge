package watcher

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"testing"

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
