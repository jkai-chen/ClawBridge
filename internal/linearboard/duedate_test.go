package linearboard

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"clawbridge/internal/linearprovider"
)

func TestParseDueDate(t *testing.T) {
	for _, in := range []string{"2026-11-14", "2000-01-01", "2100-12-31", "2028-02-29"} {
		if got, e := ParseDueDate(in); e != nil || got != in {
			t.Fatal(in, got, e)
		}
	}
	for _, in := range []any{"1999-12-31", "2101-01-01", "2026-02-30", "2027-02-29", "2026-13-01", "2026-1-5", "26-11-14", "2026-11-14T00:00:00Z", "", " 2026-11-14", "tomorrow", 20261114, nil, true} {
		if _, e := ParseDueDate(in); e == nil || !strings.Contains(e.Error(), "calendar date YYYY-MM-DD") {
			t.Fatal("accepted", in, e)
		}
	}
}

// dueOverride reports the due date Linear's answer carries: report when
// known, or none at all (silent).
type dueOverride struct {
	*providerV2
	report *string
	known  bool
}

func (p *dueOverride) UpdateV2(ctx context.Context, in linearprovider.UpdateInputV2) (linearprovider.IssueV2, error) {
	i, e := p.providerV2.UpdateV2(ctx, in)
	i.DueDate, i.DueDateKnown = p.report, p.known
	return i, e
}

func (p *dueOverride) CreateV2(ctx context.Context, in linearprovider.CreateInputV2) (linearprovider.IssueV2, error) {
	i, e := p.providerV2.CreateV2(ctx, in)
	i.DueDate, i.DueDateKnown = p.report, p.known
	return i, e
}

func dueField(d *string) linearprovider.Field[*string] {
	return linearprovider.Field[*string]{Present: true, Value: d}
}

func TestLinearV2DueDateIsSetAndChecked(t *testing.T) {
	ctx := context.Background()
	s, a, p, scope := testV2(t)
	p.caps.DueDate = true
	day, other := "2026-11-14", "2026-11-15"
	in := WriteInputV2{WriteInput: WriteInput{Operation: "update", OperationID: "due", IssueID: issueID}, DueDate: dueField(&day)}
	s.cfg.Open = openV2(&dueOverride{providerV2: p, report: &day, known: true})
	r, e := s.WriteV2(ctx, scope, in)
	if e != nil || r.(ReceiptV2).Unverified != nil {
		t.Fatal(r, e)
	}
	if got := p.updated[len(p.updated)-1].DueDate; !got.Present || got.Value == nil || *got.Value != day {
		t.Fatal("due date not sent:", got)
	}
	// Linear reports another date, or none, or doesn't say: uncertain.
	for name, o := range map[string]*dueOverride{"other": {report: &other, known: true}, "none": {known: true}, "unknown": {}} {
		o.providerV2 = p
		in.OperationID = "due-" + name
		s.cfg.Open = openV2(o)
		if _, e = s.WriteV2(ctx, scope, in); e == nil || !strings.Contains(e.Error(), "receipt_mismatch") || ledgerStateV2(t, s, a, in.OperationID) != "pending" {
			t.Fatal(name, "accepted:", e)
		}
	}
	if got := p.validatedDue[len(p.validatedDue)-1]; !got.Present || got.Value == nil || *got.Value != day {
		t.Fatal("the update preflight did not see the due date:", p.validatedDue)
	}
	// A clear: sent as a present nil, and Linear reports none.
	in.OperationID = "due-clear"
	in.DueDate = dueField(nil)
	s.cfg.Open = openV2(&dueOverride{providerV2: p, known: true})
	if r, e = s.WriteV2(ctx, scope, in); e != nil || r.(ReceiptV2).Unverified != nil {
		t.Fatal(r, e)
	}
	if got := p.updated[len(p.updated)-1].DueDate; !got.Present || got.Value != nil {
		t.Fatal("clear not sent as null:", got)
	}
	// A clear Linear does not confirm is uncertain too.
	in.OperationID = "due-clear-unknown"
	s.cfg.Open = openV2(&dueOverride{providerV2: p})
	if _, e = s.WriteV2(ctx, scope, in); e == nil || !strings.Contains(e.Error(), "receipt_mismatch") {
		t.Fatal("an unconfirmed clear accepted:", e)
	}
	// A clear Linear answers with a date is uncertain.
	in.OperationID = "due-clear-kept"
	s.cfg.Open = openV2(&dueOverride{providerV2: p, report: &day, known: true})
	if _, e = s.WriteV2(ctx, scope, in); e == nil || !strings.Contains(e.Error(), "receipt_mismatch") {
		t.Fatal("a kept date accepted as cleared:", e)
	}
}

func TestLinearV2DueDateRefusals(t *testing.T) {
	ctx := context.Background()
	s, _, p, scope := testV2(t)
	p.caps.DueDate = true
	for _, bad := range []string{"2026-02-30", "1999-01-01", "soon", "2026-11-14T10:00:00Z", ""} {
		writes, tools := p.writes, p.tools
		in := WriteInputV2{WriteInput: WriteInput{Operation: "update", OperationID: "bad", IssueID: issueID}, DueDate: dueField(&bad)}
		if _, e := s.WriteV2(ctx, scope, in); e == nil || !strings.Contains(e.Error(), "calendar date YYYY-MM-DD") || p.writes != writes || p.tools != tools {
			t.Fatal("bad date", bad, e)
		}
	}
	// A value without presence is not an intent.
	day := "2026-11-14"
	in := WriteInputV2{WriteInput: WriteInput{Operation: "update", OperationID: "nopresent", IssueID: issueID}, Priority: ptrInt(1)}
	in.DueDate.Value = &day
	if e := ValidateWriteV2(in); e == nil {
		t.Fatal("a value without Present passed")
	}
	// A new issue has nothing to clear; a comment takes no due date.
	title := "t"
	create := WriteInputV2{WriteInput: WriteInput{Operation: "create", OperationID: "c", Title: &title}, DueDate: dueField(nil)}
	if e := ValidateWriteV2(create); e == nil {
		t.Fatal("create clear passed")
	}
	comment := WriteInputV2{WriteInput: WriteInput{Operation: "comment", OperationID: "k", IssueID: issueID, Body: "b"}, DueDate: dueField(&day)}
	if e := ValidateWriteV2(comment); e == nil {
		t.Fatal("a comment took a due date")
	}
	// No capability: refused before any write.
	p.caps.DueDate = false
	writes := p.writes
	in = WriteInputV2{WriteInput: WriteInput{Operation: "update", OperationID: "nocap", IssueID: issueID}, DueDate: dueField(&day)}
	if _, e := s.WriteV2(ctx, scope, in); e == nil || !strings.Contains(e.Error(), "can't set a due date; nothing was sent") || p.writes != writes {
		t.Fatal("sent without the capability:", e)
	}
	title = "Fix"
	create = WriteInputV2{WriteInput: WriteInput{Operation: "create", OperationID: "nocap-create", Title: &title}, Project: dueField(ptrV2(projectA)), DueDate: dueField(&day)}
	if _, e := s.WriteV2(ctx, scope, create); e == nil || !strings.Contains(e.Error(), "can't set a due date") || p.writes != writes {
		t.Fatal("create sent without the capability:", e)
	}
}

func ptrInt(n int) *int { return &n }

func TestLinearV2CreateWithDueDate(t *testing.T) {
	ctx := context.Background()
	s, _, p, scope := testV2(t)
	p.caps.DueDate = true
	day := "2026-11-14"
	title := "Fix the thing"
	in := WriteInputV2{WriteInput: WriteInput{Operation: "create", OperationID: "create-due", Title: &title}, Project: dueField(ptrV2(projectA)), DueDate: dueField(&day)}
	s.cfg.Open = openV2(&dueOverride{providerV2: p, report: &day, known: true})
	r, e := s.WriteV2(ctx, scope, in)
	if e != nil || r.(ReceiptV2).Unverified != nil {
		t.Fatal(r, e)
	}
	if got := p.created[len(p.created)-1].DueDate; !got.Present || got.Value == nil || *got.Value != day {
		t.Fatal("due date not on the create:", got)
	}
	if got := p.validatedDue[len(p.validatedDue)-1]; !got.Present || got.Value == nil || *got.Value != day {
		t.Fatal("the preflight did not see the due date:", p.validatedDue)
	}
	// The plain fake reports no date: the create is uncertain.
	in.OperationID = "create-due-silent"
	s.cfg.Open = openV2(p)
	if _, e = s.WriteV2(ctx, scope, in); e == nil || !strings.Contains(e.Error(), "receipt_mismatch") {
		t.Fatal("an unreported date accepted:", e)
	}
}

// The ledger digest of a write without a due date is unchanged, and one
// with it differs.
func TestDueDateLeavesDigestAlone(t *testing.T) {
	in := WriteInputV2{WriteInput: WriteInput{Operation: "update", OperationID: "x", IssueID: issueID}}
	before := digest(in)
	if raw, _ := json.Marshal(in); strings.Contains(string(raw), "DueDate") {
		t.Fatal("an absent due date is in the digest input")
	}
	day := "2026-11-14"
	in.DueDate = dueField(&day)
	if digest(in) == before {
		t.Fatal("a due date does not change the digest")
	}
}

func TestDueDateRoundTripsAndIsChecked(t *testing.T) {
	day := "2026-11-14"
	for name, f := range map[string]linearprovider.Field[*string]{"set": dueField(&day), "clear": dueField(nil)} {
		in := WriteInputV2{WriteInput: WriteInput{Operation: "update", OperationID: "x", IssueID: issueID}, DueDate: f}
		args := writeArgsV2(in)
		v, ok := args["due_date"]
		if !ok || (name == "clear") != (v == nil) {
			t.Fatal(name, args)
		}
		back, e := parseWriteArgsV2("update", args)
		if e != nil || !reflect.DeepEqual(back.DueDate, f) {
			t.Fatal(name, back.DueDate, e)
		}
	}
	if _, ok := writeArgsV2(WriteInputV2{WriteInput: WriteInput{Operation: "update", OperationID: "x", IssueID: issueID}})["due_date"]; ok {
		t.Fatal("an absent due date was sent")
	}
	s, _, p, scope := testV2(t)
	p.caps.DueDate = true
	for _, bad := range []any{"2026-02-30", "tomorrow", 5, true} {
		if _, e := s.ExecuteV2(context.Background(), scope, "update", map[string]any{"id": issueID, "operation_id": "x", "due_date": bad}); e == nil || !strings.Contains(e.Error(), "calendar date YYYY-MM-DD") {
			t.Fatal(bad, e)
		}
	}
	if _, e := s.ExecuteV2(context.Background(), scope, "comment", map[string]any{"id": issueID, "operation_id": "x", "body": "b", "due_date": "2026-11-14"}); e == nil {
		t.Fatal("a comment took a due date")
	}
}
