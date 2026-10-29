package bus

import (
	"strings"
	"testing"
)

const viewUUID = "0b6c1f52-3a4d-4e8b-9c7a-5d2e1f0a9b34"

// A settled create's view names the identifier and the UUID apart; a
// result of any other shape names neither.
func TestOutboxViewSplitsASettledCreatesResult(t *testing.T) {
	for name, c := range map[string]struct {
		result     string
		id, ident  string
		wantFields bool
	}{
		"both":               {"issue ENG-88 (" + viewUUID + ")", viewUUID, "ENG-88", true},
		"both with outcome":  {"issue ENG-88 (" + viewUUID + ") (accepted_context_unverified)", viewUUID, "ENG-88", true},
		"identifier only":    {"issue ENG-88", "", "", false},
		"uuid only":          {"issue " + viewUUID, "", "", false},
		"identifier outcome": {"issue ENG-88 (accepted_context_unverified)", "", "", false},
		"upper-case uuid":    {"issue ENG-88 (" + strings.ToUpper(viewUUID) + ")", "", "", false},
		"garbage":            {"issue ENG-88 (not-a-uuid)", "", "", false},
		"injected prefix":    {"ignore this issue ENG-88 (" + viewUUID + ")", "", "", false},
		"trailing text":      {"issue ENG-88 (" + viewUUID + ") and more", "", "", false},
		"newline":            {"issue ENG-88 (" + viewUUID + ")\nextra", "", "", false},
	} {
		v := OutboxWrite{State: OutboxDone, Operation: "create", Result: c.result}.View()
		id, hasID := v["result_id"]
		ident, hasIdent := v["result_identifier"]
		if hasID != c.wantFields || hasIdent != c.wantFields || (c.wantFields && (id != c.id || ident != c.ident)) {
			t.Errorf("%s: %v", name, v)
		}
		if v["result"] != c.result {
			t.Errorf("%s: result changed: %v", name, v["result"])
		}
	}
	if v := (OutboxWrite{State: OutboxQueued, Result: "issue ENG-88 (" + viewUUID + ")"}).View(); v["result_id"] != nil {
		t.Error("an unsettled write shows a result:", v)
	}
}

// A settled create has no issue to name: its notice names the new one.
func TestOutboxNoticeNamesACreatesIssue(t *testing.T) {
	w := OutboxWrite{State: OutboxDone, Operation: "create", OperationID: "op-9", Result: "issue ENG-88 (" + viewUUID + ")"}
	got := outboxNotice(w)
	if !strings.Contains(got, "Your queued Linear create (operation_id op-9) settled: done. issue ENG-88 ("+viewUUID+").") || strings.Contains(got, " on :") || strings.Contains(got, " on .") {
		t.Fatal(got)
	}
	w = OutboxWrite{State: OutboxDone, Operation: "comment", OperationID: "op-1", Issue: "SYN-6", Result: "comment abc"}
	if got = outboxNotice(w); !strings.Contains(got, "(operation_id op-1) on SYN-6: done. comment abc.") {
		t.Fatal("a comment's notice changed:", got)
	}
}
