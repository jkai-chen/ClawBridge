package linearboard

import (
	"context"
	"os"
	"reflect"
	"strings"
	"testing"

	"clawbridge/internal/linearprovider"
)

func labelledIssueV2(p *providerV2, ids ...string) {
	i := p.issues[issueID]
	i.LabelsKnown = true
	i.LabelIDs = ids
	p.issues[issueID] = i
}
func replaceLabelsV2(op string, labels, expected []string) WriteInputV2 {
	in := WriteInputV2{WriteInput: WriteInput{Operation: "update", OperationID: op, IssueID: issueID}, Labels: linearprovider.Field[[]string]{Present: true, Value: labels}}
	if expected != nil {
		in.ExpectedLabels = linearprovider.Field[[]string]{Present: true, Value: expected}
	}
	return in
}

func TestLinearV2ExpectedLabelsAreMandatoryAndScoped(t *testing.T) {
	title := "task"
	for name, in := range map[string]WriteInputV2{
		"replace without expectation": replaceLabelsV2("missing", []string{labelA}, nil),
		"clear without expectation":   replaceLabelsV2("missing-clear", []string{}, nil),
		"expectation without labels":  {WriteInput: WriteInput{Operation: "update", OperationID: "orphan", IssueID: issueID, Title: &title}, ExpectedLabels: linearprovider.Field[[]string]{Present: true, Value: []string{}}},
		"expectation on create":       {WriteInput: WriteInput{Operation: "create", OperationID: "create", Title: &title}, Labels: linearprovider.Field[[]string]{Present: true, Value: []string{}}, ExpectedLabels: linearprovider.Field[[]string]{Present: true, Value: []string{}}},
		"null expectation":            {WriteInput: WriteInput{Operation: "update", OperationID: "null", IssueID: issueID}, Labels: linearprovider.Field[[]string]{Present: true, Value: []string{}}, ExpectedLabels: linearprovider.Field[[]string]{Present: true}},
		"value without presence":      {WriteInput: WriteInput{Operation: "update", OperationID: "absent", IssueID: issueID}, Labels: linearprovider.Field[[]string]{Present: true, Value: []string{}}, ExpectedLabels: linearprovider.Field[[]string]{Value: []string{}}},
		"duplicate expectation":       replaceLabelsV2("dup", []string{}, []string{labelA, labelA}),
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

func TestLinearV2ExpectedLabelsGuardFullReplacement(t *testing.T) {
	// A stale expectation is refused before any ledger record exists.
	s, a, p, scope := testV2(t)
	labelledIssueV2(p, labelA)
	in := replaceLabelsV2("stale", []string{}, []string{})
	if _, e := s.WriteV2(context.Background(), scope, in); e == nil || !strings.Contains(e.Error(), "expected_label_ids") || p.writes != 0 || ledgerStateV2(t, s, a, in.OperationID) != "absent" {
		t.Fatal("stale expectation was not refused before the ledger", e)
	}
	// The exact set the caller read, in any order, authorizes the full
	// replacement; [] still clears (D1).
	in = replaceLabelsV2("current", []string{}, []string{labelA})
	if _, e := s.WriteV2(context.Background(), scope, in); e != nil || p.writes != 1 || !p.updated[0].Labels.Present || p.updated[0].Labels.Value == nil || len(p.updated[0].Labels.Value) != 0 {
		t.Fatal("matching expectation did not clear labels", e, p.updated)
	}
	// The expectation is part of the payload digest: the same ID cannot carry
	// a different expectation.
	in.ExpectedLabels.Value = []string{}
	if _, e := s.WriteV2(context.Background(), scope, in); e == nil || !strings.Contains(e.Error(), "different scoped payload") || p.writes != 1 {
		t.Fatal("same operation ID accepted a different expectation", e)
	}
}

func TestLinearV2ExpectedLabelsThroughAdapter(t *testing.T) {
	s, _, p, scope := testV2(t)
	labelledIssueV2(p, labelA)
	base := map[string]any{"id": issueID, "operation_id": "adapter", "label_ids": []any{}}
	if _, e := s.ExecuteV2(context.Background(), scope, "update", base); e == nil || p.writes != 0 {
		t.Fatal("adapter replaced labels without an expectation", e)
	}
	base["expected_label_ids"] = nil
	if _, e := s.ExecuteV2(context.Background(), scope, "update", base); e == nil || p.writes != 0 {
		t.Fatal("adapter accepted a null expectation", e)
	}
	base["expected_label_ids"] = []any{labelA}
	if _, e := s.ExecuteV2(context.Background(), scope, "update", base); e != nil || p.writes != 1 || !reflect.DeepEqual(p.updated[0].Labels.Value, []string{}) {
		t.Fatal("adapter expectation lost", e, p.updated)
	}
	if _, e := s.ExecuteV2(context.Background(), scope, "create", map[string]any{"title": "t", "operation_id": "c", "label_ids": []any{}, "expected_label_ids": []any{}}); e == nil {
		t.Fatal("create accepted an expectation")
	}
}

func TestLinearV2LegacyDigestExcludesExpectedLabels(t *testing.T) {
	// Expected labels can never reach the byte-exact historical L1 payload:
	// they require label replacement, which is never legacy-only.
	in := replaceLabelsV2("legacy", []string{}, []string{})
	in.Labels = linearprovider.Field[[]string]{}
	if legacyOnlyV2(in) {
		t.Fatal("an expectation was treated as legacy-only intent")
	}
	if reflect.TypeOf(WriteInput{}).NumField() != 7 {
		t.Fatal("historical L1 WriteInput shape changed; its ledger digest would move")
	}
}
