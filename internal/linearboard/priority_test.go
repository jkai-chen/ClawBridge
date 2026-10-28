package linearboard

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"clawbridge/internal/linearprovider"
)

func TestParsePriority(t *testing.T) {
	for in, want := range map[any]int{0: 0, 4: 4, float64(2): 2, "urgent": 1, " High ": 2, "medium": 3, "normal": 3, "low": 4, "none": 0, "no priority": 0, "3": 3, int64(1): 1} {
		if got, e := ParsePriority(in); e != nil || got != want {
			t.Fatal(in, got, e)
		}
	}
	for _, in := range []any{5, -1, float64(2.5), "critical", "", "12", "5", "-1", true, nil} {
		if _, e := ParsePriority(in); e == nil {
			t.Fatal("accepted", in)
		}
	}
}

// priorityOverride reports the priority Linear's answer carries: report,
// or none at all (silent).
type priorityOverride struct {
	*providerV2
	report *int
}

func (p *priorityOverride) UpdateV2(ctx context.Context, in linearprovider.UpdateInputV2) (linearprovider.IssueV2, error) {
	i, e := p.providerV2.UpdateV2(ctx, in)
	i.Priority, i.PriorityKnown = p.report, p.report != nil
	return i, e
}

// FR2: an update sets Linear's priority and its receipt is checked; a
// provider that can't express it sends nothing.
func TestLinearV2PriorityIsSetAndChecked(t *testing.T) {
	ctx := context.Background()
	s, a, p, scope := testV2(t)
	p.caps.Priority = true
	high, medium := 2, 3
	in := WriteInputV2{WriteInput: WriteInput{Operation: "update", OperationID: "prio", IssueID: issueID}, Priority: &high}
	s.cfg.Open = openV2(&priorityOverride{providerV2: p, report: &high})
	r, e := s.WriteV2(ctx, scope, in)
	if e != nil || r.(ReceiptV2).Unverified != nil {
		t.Fatal(r, e)
	}
	if got := p.updated[len(p.updated)-1].Priority; got == nil || *got != 2 {
		t.Fatal("priority not sent:", got)
	}
	// Linear reports another priority: uncertain.
	in.OperationID = "prio-other"
	s.cfg.Open = openV2(&priorityOverride{providerV2: p, report: &medium})
	if _, e = s.WriteV2(ctx, scope, in); e == nil || !strings.Contains(e.Error(), "receipt_mismatch") || ledgerStateV2(t, s, a, in.OperationID) != "pending" {
		t.Fatal("a contradicting priority accepted:", e)
	}
	// Linear doesn't report it: accepted, named unverified, and valid.
	in.OperationID = "prio-silent"
	s.cfg.Open = openV2(&priorityOverride{providerV2: p})
	r, e = s.WriteV2(ctx, scope, in)
	if e != nil || !reflect.DeepEqual(r.(ReceiptV2).Unverified, []string{"priority"}) {
		t.Fatal(r, e)
	}
	raw, _ := json.Marshal(r)
	if !validReceiptV2(raw) {
		t.Fatal("a receipt with priority unverified doesn't validate:", string(raw))
	}
	// No capability: refused before any write.
	p.caps.Priority = false
	writes := p.writes
	in.OperationID = "prio-nocap"
	if _, e = s.WriteV2(ctx, scope, in); e == nil || !strings.Contains(e.Error(), "cannot set an issue's priority") || p.writes != writes {
		t.Fatal("sent without the capability:", e)
	}
}

// A create carries the priority to the provider.
func TestLinearV2CreateWithPriority(t *testing.T) {
	ctx := context.Background()
	s, _, p, scope := testV2(t)
	p.caps.Priority = true
	low := 4
	title := "Fix the thing"
	in := WriteInputV2{WriteInput: WriteInput{Operation: "create", OperationID: "create-prio", Title: &title}, Project: linearprovider.Field[*string]{Present: true, Value: ptrV2(projectA)}, Priority: &low}
	r, e := s.WriteV2(ctx, scope, in)
	// The fake's created issue reports no priority: named unverified.
	if e != nil || !reflect.DeepEqual(r.(ReceiptV2).Unverified, []string{"priority"}) {
		t.Fatal(r, e)
	}
	if len(p.created) == 0 || p.created[len(p.created)-1].Priority == nil || *p.created[len(p.created)-1].Priority != 4 {
		t.Fatal("priority not on the create:", p.created)
	}
}

// The app route carries priority across: the server reads back what the
// client sent, and the board's argument check takes a name or a number.
func TestPriorityRoundTripsAndIsChecked(t *testing.T) {
	two := 2
	in := WriteInputV2{WriteInput: WriteInput{Operation: "update", OperationID: "x", IssueID: issueID}, Priority: &two}
	back, e := parseWriteArgsV2("update", writeArgsV2(in))
	if e != nil || back.Priority == nil || *back.Priority != 2 {
		t.Fatal(back.Priority, e)
	}
	if back, e = parseWriteArgsV2("update", map[string]any{"id": issueID, "operation_id": "x", "priority": "urgent"}); e != nil || *back.Priority != 1 {
		t.Fatal(e)
	}
	s, _, _, scope := testV2(t)
	if _, e = s.ExecuteV2(context.Background(), scope, "update", map[string]any{"id": issueID, "operation_id": "x", "priority": "critical"}); e == nil || !strings.Contains(e.Error(), "priority is 0-4") {
		t.Fatal(e)
	}
	if _, e = s.ExecuteV2(context.Background(), scope, "comment", map[string]any{"id": issueID, "operation_id": "x", "body": "b", "priority": 1}); e == nil {
		t.Fatal("a comment took a priority")
	}
}
