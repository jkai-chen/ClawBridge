package linearboard

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
)

func textIssueV2(p *providerV2, title, description string) {
	i := p.issues[issueID]
	i.Title = title
	i.Description = description
	p.issues[issueID] = i
}

func TestLinearV2TextGuardsAreScopedToTheReplacedField(t *testing.T) {
	sum := descriptionSHA256("body")
	for name, in := range map[string]WriteInputV2{
		"title guard without title":             {WriteInput: WriteInput{Operation: "update", OperationID: "a", IssueID: issueID, Description: ptrV2("x")}, ExpectedTitle: ptrV2("t")},
		"description guard without description": {WriteInput: WriteInput{Operation: "update", OperationID: "b", IssueID: issueID, Title: ptrV2("x")}, ExpectedDescriptionSHA256: &sum},
		"guard on create":                       {WriteInput: WriteInput{Operation: "create", OperationID: "c", Title: ptrV2("x")}, ExpectedTitle: ptrV2("t")},
		"uppercase digest":                      {WriteInput: WriteInput{Operation: "update", OperationID: "d", IssueID: issueID, Description: ptrV2("x")}, ExpectedDescriptionSHA256: ptrV2(strings.ToUpper(sum))},
		"short digest":                          {WriteInput: WriteInput{Operation: "update", OperationID: "e", IssueID: issueID, Description: ptrV2("x")}, ExpectedDescriptionSHA256: ptrV2(sum[:63])},
	} {
		s, _, p, scope := testV2(t)
		if _, e := s.WriteV2(context.Background(), scope, in); e == nil || p.writes != 0 || p.reads != 0 {
			t.Fatalf("%s: accepted or performed I/O: %v", name, e)
		}
		if _, e := os.Stat(s.cfg.LedgerDirectory); !os.IsNotExist(e) {
			t.Fatalf("%s: invalid intent created ledger state", name)
		}
	}
}

func TestLinearV2TextGuardsRefuseStaleReadsAndPassCurrentOnes(t *testing.T) {
	s, a, p, scope := testV2(t)
	textIssueV2(p, "old title", "old body")
	read, e := s.IssueV2(context.Background(), scope, issueID)
	if e != nil || read.DescriptionSHA256 != descriptionSHA256("old body") {
		t.Fatal("issue read lacks description_sha256", read.DescriptionSHA256, e)
	}
	// Someone edits the issue after the caller's read: refused before the ledger.
	textIssueV2(p, "edited title", "edited body")
	for _, in := range []WriteInputV2{
		{WriteInput: WriteInput{Operation: "update", OperationID: "stale-title", IssueID: issueID, Title: ptrV2("new")}, ExpectedTitle: ptrV2(read.Title)},
		{WriteInput: WriteInput{Operation: "update", OperationID: "stale-body", IssueID: issueID, Description: ptrV2("new")}, ExpectedDescriptionSHA256: &read.DescriptionSHA256},
	} {
		if _, e = s.WriteV2(context.Background(), scope, in); e == nil || !strings.Contains(e.Error(), "differs from expected_") || p.writes != 0 || ledgerStateV2(t, s, a, in.OperationID) != "absent" {
			t.Fatal("stale text guard not refused before the ledger", in.OperationID, e)
		}
	}
	// The current values pass, and the guard is part of the payload digest.
	current := descriptionSHA256("edited body")
	in := WriteInputV2{WriteInput: WriteInput{Operation: "update", OperationID: "current", IssueID: issueID, Title: ptrV2("new"), Description: ptrV2("new body")}, ExpectedTitle: ptrV2("edited title"), ExpectedDescriptionSHA256: &current}
	if _, e = s.WriteV2(context.Background(), scope, in); e != nil || p.writes != 1 {
		t.Fatal("current text guard refused", e)
	}
	in.ExpectedTitle = ptrV2("other")
	if _, e = s.WriteV2(context.Background(), scope, in); e == nil || !strings.Contains(e.Error(), "different scoped payload") || p.writes != 1 {
		t.Fatal("same operation ID accepted a different guard", e)
	}
}

func TestLinearV2TextGuardDriftInsideTheLockIsTerminal(t *testing.T) {
	for _, field := range []string{"title", "description"} {
		t.Run(field, func(t *testing.T) {
			s, a, p, scope := testV2(t)
			textIssueV2(p, "title", "body")
			sum := descriptionSHA256("body")
			in := WriteInputV2{WriteInput: WriteInput{Operation: "update", OperationID: "drift-" + field, IssueID: issueID}}
			if field == "title" {
				in.Title, in.ExpectedTitle = ptrV2("new"), ptrV2("title")
			} else {
				in.Description, in.ExpectedDescriptionSHA256 = ptrV2("new"), &sum
			}
			a.afterAccess = func(n int) {
				if n == 2 {
					textIssueV2(p, "concurrent title", "concurrent body")
				}
			}
			if _, e := s.WriteV2(context.Background(), scope, in); e == nil || p.writes != 0 {
				t.Fatal("concurrent edit overwritten", e)
			}
			if state := ledgerStateV2(t, s, a, in.OperationID); state != "refused" {
				t.Fatal("in-lock text drift was not a terminal refusal", state)
			}
		})
	}
}

func TestLinearV2UnguardedPayloadDigestIsUnchanged(t *testing.T) {
	in := WriteInputV2{WriteInput: WriteInput{Operation: "update", OperationID: "plain", IssueID: issueID, Title: ptrV2("t")}}
	raw, _ := json.Marshal(in)
	if strings.Contains(string(raw), "ExpectedTitle") || strings.Contains(string(raw), "ExpectedDescriptionSHA256") {
		t.Fatal("unguarded payload carries guard keys; v0.2.0 ledger digests would move", string(raw))
	}
	in.ExpectedTitle = ptrV2("t")
	if legacyOnlyV2(in) {
		t.Fatal("a guarded write took the legacy path, whose digest omits the guard")
	}
}

func TestLinearV2TextGuardsThroughAdapter(t *testing.T) {
	s, _, p, scope := testV2(t)
	textIssueV2(p, "title", "body")
	args := map[string]any{"id": issueID, "operation_id": "adapter", "description": "new", "expected_description_sha256": descriptionSHA256("other")}
	if _, e := s.ExecuteV2(context.Background(), scope, "update", args); e == nil || p.writes != 0 {
		t.Fatal("adapter lost the description guard", e)
	}
	args = map[string]any{"id": issueID, "operation_id": "adapter-2", "title": "new", "expected_title": "title"}
	if _, e := s.ExecuteV2(context.Background(), scope, "update", args); e != nil || p.writes != 1 {
		t.Fatal("adapter refused a current title guard", e)
	}
	if _, e := s.ExecuteV2(context.Background(), scope, "update", map[string]any{"id": issueID, "operation_id": "adapter-3", "title": "new", "expected_title": nil}); e == nil {
		t.Fatal("adapter accepted a null title guard")
	}
}
