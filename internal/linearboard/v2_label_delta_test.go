package linearboard

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"reflect"
	"strings"
	"testing"

	"clawbridge/internal/bus"
	"clawbridge/internal/linearprovider"
)

func deltaV2(op string, add, remove []string) WriteInputV2 {
	return WriteInputV2{WriteInput: WriteInput{Operation: "update", OperationID: op, IssueID: issueID}, AddLabels: add, RemoveLabels: remove}
}

func deltaTestV2(t *testing.T, present ...string) (*Service, *authorityV2, *providerV2, bus.Request) {
	t.Helper()
	s, a, p, scope := testV2(t)
	p.caps.LabelDeltas = true
	p.labels = []linearprovider.Label{{ID: labelA, TeamID: teamID, EligibilityKnown: true}, {ID: memberA, TeamID: teamID, EligibilityKnown: true}}
	labelledIssueV2(p, present...)
	return s, a, p, scope
}

func TestLinearV2LabelDeltasAreValidatedBeforeAnyIO(t *testing.T) {
	many := make([]string, 21)
	for i := range many {
		many[i] = fmt.Sprintf("%08x-0000-4000-8000-000000000000", i+1)
	}
	title := "t"
	for name, in := range map[string]WriteInputV2{
		"on create":         {WriteInput: WriteInput{Operation: "create", OperationID: "c", Title: &title}, AddLabels: []string{labelA}},
		"with replacement":  {WriteInput: WriteInput{Operation: "update", OperationID: "r", IssueID: issueID}, AddLabels: []string{labelA}, Labels: linearprovider.Field[[]string]{Present: true, Value: []string{}}, ExpectedLabels: linearprovider.Field[[]string]{Present: true, Value: []string{}}},
		"with expectation":  {WriteInput: WriteInput{Operation: "update", OperationID: "x", IssueID: issueID}, RemoveLabels: []string{labelA}, ExpectedLabels: linearprovider.Field[[]string]{Present: true, Value: []string{}}},
		"empty add":         deltaV2("e", []string{}, nil),
		"too many":          deltaV2("m", many, nil),
		"added and removed": deltaV2("b", []string{labelA}, []string{labelA}),
		"duplicate":         deltaV2("d", nil, []string{labelA, labelA}),
		"non-canonical":     deltaV2("n", []string{"Bug"}, nil),
	} {
		s, _, p, scope := deltaTestV2(t)
		if _, e := s.WriteV2(context.Background(), scope, in); e == nil || p.writes != 0 || p.reads != 0 {
			t.Fatalf("%s: accepted or performed I/O: %v", name, e)
		}
		if _, e := os.Stat(s.cfg.LedgerDirectory); !os.IsNotExist(e) {
			t.Fatalf("%s: invalid intent created ledger state", name)
		}
	}
}

func TestLinearV2LabelDeltasSendOnlyRealChanges(t *testing.T) {
	ctx := context.Background()
	// Known label sets decide membership directly.
	s, a, p, scope := deltaTestV2(t, labelA)
	r, e := s.WriteV2(ctx, scope, deltaV2("noop", []string{labelA}, []string{memberA}))
	if e != nil || p.writes != 0 || r.(ReceiptV2).Outcome != "observed_unchanged" {
		t.Fatal("deltas already in effect were sent", r, e, p.writes)
	}
	again, e := s.WriteV2(ctx, scope, deltaV2("noop", []string{labelA}, []string{memberA}))
	var replayed ReceiptV2
	raw, _ := json.Marshal(again)
	if e != nil || p.writes != 0 || json.Unmarshal(raw, &replayed) != nil || replayed.Outcome != "observed_unchanged" {
		t.Fatal("observed_unchanged not replayed from the ledger", again, e)
	}
	r, e = s.WriteV2(ctx, scope, deltaV2("swap", []string{memberA}, []string{labelA}))
	if e != nil || p.writes != 1 || !reflect.DeepEqual(p.updated[0].AddLabels, []string{memberA}) || !reflect.DeepEqual(p.updated[0].RemoveLabels, []string{labelA}) || p.updated[0].Labels.Present {
		t.Fatal("real deltas not sent as deltas", r, e, p.updated)
	}
	if receipt := r.(ReceiptV2); !reflect.DeepEqual(receipt.LabelsAdded, []string{memberA}) || !reflect.DeepEqual(receipt.LabelsRemoved, []string{labelA}) {
		t.Fatal("receipt does not name the sent deltas", receipt)
	}
	// A no-op delta beside a real change sends only the real change.
	in := deltaV2("with-title", []string{labelA}, nil)
	in.Title = ptrV2("new")
	if _, e = s.WriteV2(ctx, scope, in); e != nil || p.writes != 2 || p.updated[1].AddLabels != nil || p.updated[1].Title == nil {
		t.Fatal("no-op delta sent beside a title change", e, p.updated)
	}
	// Beside a priority, likewise: the priority alone is a change.
	in = deltaV2("with-priority", []string{labelA}, nil)
	urgent := 1
	in.Priority = &urgent
	p.caps.Priority = true
	if _, e = s.WriteV2(ctx, scope, in); e != nil || p.writes != 3 || p.updated[2].AddLabels != nil || p.updated[2].Priority == nil || *p.updated[2].Priority != 1 {
		t.Fatal("no-op delta beside a priority sent nothing", e, p.updated)
	}
	// An ineligible add and a missing capability are refused before the ledger.
	if _, e = s.WriteV2(ctx, scope, deltaV2("foreign", []string{parentA}, nil)); e == nil || p.writes != 3 || ledgerStateV2(t, s, a, "foreign") != "absent" {
		t.Fatal("ineligible label added", e)
	}
	p.caps.LabelDeltas = false
	if _, e = s.WriteV2(ctx, scope, deltaV2("no-cap", []string{memberA}, nil)); e == nil || p.writes != 3 || ledgerStateV2(t, s, a, "no-cap") != "absent" {
		t.Fatal("deltas sent without the capability", e)
	}
}

func TestLinearV2NamesOnlyDeltasUseServerMembership(t *testing.T) {
	ctx := context.Background()
	catalog := []linearprovider.LabelName{{ID: labelA, Name: "needs-agent"}}
	s, p, m, _ := resolvingV2(t, []string{"needs-agent"}, catalog, map[string]bool{labelA: true})
	_, a, _, scope := testV2(t)
	p.caps.LabelDeltas = true
	p.labels = []linearprovider.Label{{ID: labelA, TeamID: teamID, EligibilityKnown: true}, {ID: memberA, TeamID: teamID, EligibilityKnown: true}}
	// Proven present: the removal is sent, with one membership check before,
	// one inside the lock and one confirming it afterwards, and no catalog walk.
	p.after = func() { m.member[labelA] = false }
	r, e := s.WriteV2(ctx, scope, deltaV2("clear-needs-agent", nil, []string{labelA}))
	if e != nil || p.writes != 1 || !reflect.DeepEqual(p.updated[0].RemoveLabels, []string{labelA}) || *m.checks != 3 || *m.catalogs != 0 {
		t.Fatalf("proven removal not sent cheaply: %v writes=%d checks=%d catalogs=%d", e, p.writes, *m.checks, *m.catalogs)
	}
	if r.(ReceiptV2).Unverified != nil {
		t.Fatal("removal confirmed by the label filter still reported unverified", r)
	}
	p.after = nil
	// Proven absent: nothing to remove.
	if r, e := s.WriteV2(ctx, scope, deltaV2("absent", nil, []string{memberA})); e != nil || p.writes != 1 || r.(ReceiptV2).Outcome != "observed_unchanged" {
		t.Fatal("proven-absent removal not observed_unchanged", r, e)
	}
	// Unknown membership: an add is still safe to send, a removal is refused.
	m.bounded = true
	if _, e := s.WriteV2(ctx, scope, deltaV2("unknown-add", []string{memberA}, nil)); e != nil || p.writes != 2 || !reflect.DeepEqual(p.updated[1].AddLabels, []string{memberA}) {
		t.Fatal("append-only add with unknown membership refused", e)
	}
	if _, e := s.WriteV2(ctx, scope, deltaV2("unknown-remove", nil, []string{labelA})); e == nil || !strings.Contains(e.Error(), "membership is unknown") || p.writes != 2 || ledgerStateV2(t, s, a, "unknown-remove") != "absent" {
		t.Fatal("removal with unknown membership not refused before the ledger", e)
	}
	// Without the membership proof capability, membership is unknown too.
	m.bounded = false
	p.caps.LabelMembershipProof = false
	if _, e := s.WriteV2(ctx, scope, deltaV2("no-proof-remove", nil, []string{labelA})); e == nil || p.writes != 2 {
		t.Fatal("removal without membership proof sent", e)
	}
}

func TestLinearV2DeltaDriftInsideTheLockIsTerminal(t *testing.T) {
	for _, mode := range []string{"label added meanwhile", "label removed meanwhile", "add no longer eligible"} {
		t.Run(mode, func(t *testing.T) {
			s, a, p, scope := deltaTestV2(t, labelA)
			in := deltaV2("drift", []string{memberA}, []string{labelA})
			a.afterAccess = func(n int) {
				if n != 2 {
					return
				}
				switch mode {
				case "label added meanwhile":
					labelledIssueV2(p, labelA, memberA)
				case "label removed meanwhile":
					labelledIssueV2(p)
				case "add no longer eligible":
					p.labels = p.labels[:1]
				}
			}
			if _, e := s.WriteV2(context.Background(), scope, in); e == nil || p.writes != 0 {
				t.Fatal("drifted delta dispatched", e)
			}
			if state := ledgerStateV2(t, s, a, in.OperationID); state != "refused" {
				t.Fatal("in-lock delta drift was not a terminal refusal", state)
			}
		})
	}
}

func TestLinearV2DeltaDigestAndAdapter(t *testing.T) {
	raw, _ := json.Marshal(WriteInputV2{WriteInput: WriteInput{Operation: "update", OperationID: "plain", IssueID: issueID, Title: ptrV2("t")}})
	if strings.Contains(string(raw), "AddLabels") || strings.Contains(string(raw), "RemoveLabels") {
		t.Fatal("payload without deltas carries delta keys; earlier ledger digests would move", string(raw))
	}
	if legacyOnlyV2(deltaV2("x", nil, []string{labelA})) {
		t.Fatal("a delta took the legacy path, whose digest omits it")
	}
	s, _, p, scope := deltaTestV2(t)
	if _, e := s.ExecuteV2(context.Background(), scope, "update", map[string]any{"id": issueID, "operation_id": "a", "add_label_ids": []any{labelA}}); e != nil || p.writes != 1 || !reflect.DeepEqual(p.updated[0].AddLabels, []string{labelA}) {
		t.Fatal("adapter add lost", e, p.updated)
	}
	if _, e := s.ExecuteV2(context.Background(), scope, "update", map[string]any{"id": issueID, "operation_id": "a", "add_label_ids": []any{memberA}}); e == nil || !strings.Contains(e.Error(), "different scoped payload") {
		t.Fatal("same operation ID accepted a different delta", e)
	}
	for _, args := range []map[string]any{
		{"id": issueID, "operation_id": "n", "remove_label_ids": nil},
		{"id": issueID, "operation_id": "r", "remove_label_ids": []any{labelA}, "label_ids": []any{}, "expected_label_ids": []any{}},
	} {
		if _, e := s.ExecuteV2(context.Background(), scope, "update", args); e == nil || p.writes != 1 {
			t.Fatal("adapter accepted an invalid delta", args, e)
		}
	}
}

func TestLinearV2RecordedDeltaReceiptsAreValidated(t *testing.T) {
	issue := func(outcome string, added []string) []byte {
		raw, _ := json.Marshal(ReceiptV2{Kind: "issue", Outcome: outcome, ID: issueID, TeamID: teamID, LabelsAdded: added})
		return raw
	}
	comment, _ := json.Marshal(ReceiptV2{Kind: "comment", Outcome: "accepted", ID: issueID, IssueID: issueID, LabelsRemoved: []string{labelA}})
	for name, c := range map[string]struct {
		raw []byte
		ok  bool
	}{
		"accepted with deltas":      {issue("accepted", []string{labelA}), true},
		"observed unchanged":        {issue("observed_unchanged", nil), true},
		"unchanged claiming a send": {issue("observed_unchanged", []string{labelA}), false},
		"non-canonical delta":       {issue("accepted", []string{"Bug"}), false},
		"empty delta list":          {[]byte(`{"kind":"issue","outcome":"accepted","id":"` + issueID + `","team_id":"` + teamID + `","labels_added":[]}`), false},
		"deltas on a comment":       {comment, false},
	} {
		if validReceiptV2(c.raw) != c.ok {
			t.Fatal(name, string(c.raw))
		}
	}
}

// labelReceiptOverride answers updates with a fixed label report, or none.
type labelReceiptOverride struct {
	*providerV2
	labels []string
	silent bool
}

func (p *labelReceiptOverride) UpdateV2(ctx context.Context, in linearprovider.UpdateInputV2) (linearprovider.IssueV2, error) {
	i, e := p.providerV2.UpdateV2(ctx, in)
	i.LabelIDs, i.LabelsKnown = p.labels, !p.silent
	return i, e
}

func TestLinearV2DeltaReceiptsCheckReportedLabels(t *testing.T) {
	ctx := context.Background()
	for name, c := range map[string]struct {
		report  labelReceiptOverride
		outcome string
	}{
		"add not reported present": {labelReceiptOverride{labels: []string{}}, "receipt_mismatch"},
		"remove still reported":    {labelReceiptOverride{labels: []string{labelA, memberA}}, "receipt_mismatch"},
		"labels not reported":      {labelReceiptOverride{silent: true}, "unverified"},
		"both as asked":            {labelReceiptOverride{labels: []string{memberA}}, "accepted"},
	} {
		t.Run(name, func(t *testing.T) {
			s, a, p, scope := deltaTestV2(t, labelA)
			c.report.providerV2 = p
			s.cfg.Open = openV2(&c.report)
			in := deltaV2("receipt", []string{memberA}, []string{labelA})
			r, e := s.WriteV2(ctx, scope, in)
			switch c.outcome {
			case "receipt_mismatch":
				if e == nil || !strings.Contains(e.Error(), "receipt_mismatch") || ledgerStateV2(t, s, a, in.OperationID) != "pending" {
					t.Fatal("contradicting label receipt accepted", r, e)
				}
			case "unverified":
				if e != nil || !reflect.DeepEqual(r.(ReceiptV2).Unverified, []string{"labels"}) {
					t.Fatal("unreported labels not named unverified", r, e)
				}
			default:
				if e != nil || r.(ReceiptV2).Unverified != nil {
					t.Fatal("matching label receipt not verified", r, e)
				}
			}
		})
	}
}

// Native receipts name labels only, so a delta is confirmed afterwards with
// Linear's list filtered by each label's UUID.
func TestLinearV2NamesOnlyDeltaReceiptsAreConfirmedByTheLabelFilter(t *testing.T) {
	ctx := context.Background()
	catalog := []linearprovider.LabelName{{ID: labelA, Name: "needs-agent"}}
	for name, c := range map[string]struct {
		after   func(m *membershipV2)
		outcome string
	}{
		"took effect":       {func(m *membershipV2) { m.member[labelA] = false; m.member[memberA] = true }, "verified"},
		"removal not taken": {func(m *membershipV2) { m.member[memberA] = true }, "receipt_mismatch"},
		"add not taken":     {func(m *membershipV2) { m.member[labelA] = false }, "receipt_mismatch"},
		"filter cannot say": {func(m *membershipV2) { m.providerV2.caps.LabelMembershipProof = false }, "unverified"},
	} {
		t.Run(name, func(t *testing.T) {
			s, p, m, _ := resolvingV2(t, []string{"needs-agent"}, catalog, map[string]bool{labelA: true})
			_, a, _, scope := testV2(t)
			p.caps.LabelDeltas = true
			p.labels = []linearprovider.Label{{ID: labelA, TeamID: teamID, EligibilityKnown: true}, {ID: memberA, TeamID: teamID, EligibilityKnown: true}}
			p.after = func() { c.after(m) }
			in := deltaV2("swap", []string{memberA}, []string{labelA})
			r, e := s.WriteV2(ctx, scope, in)
			switch c.outcome {
			case "verified":
				if e != nil || r.(ReceiptV2).Unverified != nil {
					t.Fatal("confirmed deltas reported unverified", r, e)
				}
			case "unverified":
				if e != nil || !reflect.DeepEqual(r.(ReceiptV2).Unverified, []string{"labels"}) {
					t.Fatal("inconclusive check not reported unverified", r, e)
				}
			default:
				if e == nil || !strings.Contains(e.Error(), "receipt_mismatch") || ledgerStateV2(t, s, a, in.OperationID) != "pending" {
					t.Fatal("contradicting label filter accepted", r, e)
				}
			}
		})
	}
}

// When the label filter reaches no answer, the delta stays unverified.
func TestLinearV2InconclusiveLabelFilterLeavesDeltaUnverified(t *testing.T) {
	catalog := []linearprovider.LabelName{{ID: labelA, Name: "needs-agent"}}
	s, p, m, _ := resolvingV2(t, []string{"needs-agent"}, catalog, map[string]bool{labelA: true})
	_, _, _, scope := testV2(t)
	p.caps.LabelDeltas = true
	p.labels = []linearprovider.Label{{ID: labelA, TeamID: teamID, EligibilityKnown: true}, {ID: memberA, TeamID: teamID, EligibilityKnown: true}}
	m.bounded = true // an append-only add is still sent; the check after it is inconclusive
	r, e := s.WriteV2(context.Background(), scope, deltaV2("bounded-add", []string{memberA}, nil))
	if e != nil || p.writes != 1 || !reflect.DeepEqual(r.(ReceiptV2).Unverified, []string{"labels"}) {
		t.Fatal("inconclusive label filter treated as confirmation", r, e)
	}
}

// F5: a removal answered with an empty, "known" label list is checked
// afterwards with the label filter, as an add is: the empty list alone does
// not prove the label gone.
func TestLinearV2RemovalWithAnEmptyReceiptIsCheckedAfterwards(t *testing.T) {
	ctx := context.Background()
	for name, c := range map[string]struct {
		stillThere bool
		mismatch   bool
	}{"gone": {false, false}, "still there": {true, true}} {
		t.Run(name, func(t *testing.T) {
			s, p, m, _ := resolvingV2(t, nil, nil, map[string]bool{labelA: true})
			_, a, _, scope := testV2(t)
			p.caps.LabelDeltas = true
			issue := p.issues[issueID]
			issue.LabelsKnown, issue.LabelIDs, issue.LabelNames = true, []string{labelA}, nil
			p.issues[issueID] = issue
			checksBefore := 0
			p.after = func() {
				checksBefore = *m.checks
				m.member[labelA] = c.stillThere
			}
			in := deltaV2("remove-empty", nil, []string{labelA})
			r, e := s.WriteV2(ctx, scope, in)
			if *m.checks <= checksBefore {
				t.Fatal("no label check after the removal was sent", *m.checks, checksBefore)
			}
			if c.mismatch {
				if e == nil || !strings.Contains(e.Error(), "receipt_mismatch") || ledgerStateV2(t, s, a, in.OperationID) != "pending" {
					t.Fatal("a removal that did not take was accepted:", r, e)
				}
				return
			}
			if e != nil || r.(ReceiptV2).Unverified != nil {
				t.Fatal("a confirmed removal was not verified:", r, e)
			}
		})
	}
}
