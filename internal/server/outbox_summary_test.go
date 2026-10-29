package server

import (
	"testing"

	"clawbridge/internal/linearboard"
)

const summaryUUID = "0b6c1f52-3a4d-4e8b-9c7a-5d2e1f0a9b34"

func TestReceiptSummaryNamesTheIdentifierAndTheUUID(t *testing.T) {
	for name, c := range map[string]struct {
		op      string
		receipt linearboard.ReceiptV2
		outcome string
		want    string
	}{
		"both":                 {"create", linearboard.ReceiptV2{Kind: "issue", ID: summaryUUID, Identifier: "ENG-88"}, "", "issue ENG-88 (" + summaryUUID + ")"},
		"both with an outcome": {"create", linearboard.ReceiptV2{Kind: "issue", ID: summaryUUID, Identifier: "ENG-88"}, "accepted_context_unverified", "issue ENG-88 (" + summaryUUID + ") (accepted_context_unverified)"},
		"identifier only":      {"create", linearboard.ReceiptV2{Kind: "issue", Identifier: "ENG-88"}, "", "issue ENG-88"},
		"uuid only":            {"comment", linearboard.ReceiptV2{Kind: "comment", ID: summaryUUID}, "", "comment " + summaryUUID},
		"unsafe identifier":    {"create", linearboard.ReceiptV2{Kind: "issue", ID: summaryUUID, Identifier: "ENG 88)"}, "", "issue " + summaryUUID},
		"id that is no uuid":   {"create", linearboard.ReceiptV2{Kind: "issue", ID: "abc123", Identifier: "ENG-88"}, "", "issue ENG-88"},
		"neither":              {"update", linearboard.ReceiptV2{Kind: "issue"}, "", "issue"},
	} {
		if got := receiptSummary(c.op, c.receipt, c.outcome); got != c.want {
			t.Errorf("%s: %q, want %q", name, got, c.want)
		}
	}
}
