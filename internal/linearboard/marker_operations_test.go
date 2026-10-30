package linearboard

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

var markerOp = regexp.MustCompile(`\Amarker-[0-9a-f]{16}\z`)

// sharedRecord is the shared ledger's record for op.
func sharedRecord(t *testing.T, store *memLedgerStore, op string) ledgerRecord {
	t.Helper()
	for key, raw := range store.records {
		if strings.HasSuffix(key, "/"+op) {
			var r ledgerRecord
			if err := json.Unmarshal([]byte(raw), &r); err != nil {
				t.Fatal(err)
			}
			return r
		}
	}
	t.Fatal("no shared ledger record for", op, store.records)
	return ledgerRecord{}
}

// A state move or label write that may have reached Linear settles with
// its attempt's operation ID, and the shared ledger's record of that
// attempt names the issue and the marker part, so the bus can bind it.
func TestMarkerAttemptSettlesWithItsOperationID(t *testing.T) {
	s, a, p, scope := stateTest(t, "claim", sfBacklog, sfPolicy(), false)
	store := newMemLedgerStore()
	s.cfg.Ledger = SharedLedger(store, nil)
	p.writeErr = errors.New("connection reset")
	s.ApplyMarker(context.Background(), scope, "asg_1", "claim", nil)
	got := settled(a, "state")
	op, _ := got["operation_id"].(string)
	if got["outcome"] != "uncertain" || !markerOp.MatchString(op) {
		t.Fatal(got)
	}
	if r := sharedRecord(t, store, op); r.IssueID != issueID || r.MarkerPart != "state" || r.Operation != "update" || r.ProjectID != "prj" || r.State != "pending" {
		t.Fatalf("%+v", r)
	}
	// The attempt's own reread proves the move landed: done carries the ID.
	s, a, p, scope = stateTest(t, "claim", sfBacklog, sfPolicy(), false)
	s.cfg.Ledger = SharedLedger(newMemLedgerStore(), nil)
	p.writeErr = errors.New("connection reset")
	p.after = func() {
		issue := p.issues[issueID]
		issue.StateID = sfProgress
		p.issues[issueID] = issue
	}
	s.ApplyMarker(context.Background(), scope, "asg_1", "claim", nil)
	if got = settled(a, "state"); got["outcome"] != "done" || !markerOp.MatchString(got["operation_id"].(string)) {
		t.Fatal(got)
	}
	// A move that went through carries none.
	s, a, _, scope = stateTest(t, "claim", sfBacklog, sfPolicy(), false)
	s.ApplyMarker(context.Background(), scope, "asg_1", "claim", nil)
	if got = settled(a, "state"); got["outcome"] != "done" || got["operation_id"] != nil {
		t.Fatal(got)
	}
	// The label part the same.
	s, a, p, scope = markerTest(t)
	store = newMemLedgerStore()
	s.cfg.Ledger = SharedLedger(store, nil)
	a.comments.gos["comment"] = map[string]any{"action": "none"}
	p.writeErr = errors.New("connection reset")
	s.ApplyMarker(context.Background(), scope, "asg_1", "claim", nil)
	got = settled(a, "label")
	op, _ = got["operation_id"].(string)
	if got["outcome"] != "uncertain" || !markerOp.MatchString(op) {
		t.Fatal(got)
	}
	if r := sharedRecord(t, store, op); r.IssueID != issueID || r.MarkerPart != "label" || r.Operation != "update" {
		t.Fatalf("%+v", r)
	}
	s, a, _, scope = markerTest(t)
	s.ApplyMarker(context.Background(), scope, "asg_1", "claim", nil)
	if got = settled(a, "label"); got["outcome"] != "done" || got["operation_id"] != nil {
		t.Fatal(got)
	}
}

// The private ledger leaves the issue and the marker part out: an older
// client's strict decoder would refuse its record.
func TestPrivateLedgerRecordNamesNoIssue(t *testing.T) {
	s, _, p, scope := stateTest(t, "claim", sfBacklog, sfPolicy(), false)
	p.writeErr = errors.New("connection reset")
	s.ApplyMarker(context.Background(), scope, "asg_1", "claim", nil)
	records, _ := filepath.Glob(filepath.Join(s.cfg.LedgerDirectory, "*", "*.json"))
	if len(records) == 0 {
		t.Fatal("no private record")
	}
	for _, path := range records {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(raw), "issue_id") || strings.Contains(string(raw), "marker_part") {
			t.Fatal(string(raw))
		}
	}
}

// An issue write's shared record names its issue, and no marker part.
func TestSharedLedgerRecordNamesTheIssue(t *testing.T) {
	s, _, _, scope := testV2(t)
	store := newMemLedgerStore()
	s.cfg.Ledger = SharedLedger(store, nil)
	title := "SYNTHETIC-zq81"
	if _, err := s.WriteV2(context.Background(), scope, WriteInputV2{WriteInput: WriteInput{Operation: "update", OperationID: "op-issue", IssueID: issueID, Title: &title}}); err != nil {
		t.Fatal(err)
	}
	if r := sharedRecord(t, store, "op-issue"); r.IssueID != issueID || r.MarkerPart != "" || r.Operation != "update" {
		t.Fatalf("%+v", r)
	}
	for _, raw := range store.records {
		if strings.Contains(raw, "SYNTHETIC") {
			t.Fatal("Linear text in the ledger:", raw)
		}
	}
}

// A server older than schema 53 refuses operation_id: the part settles
// without it, as before.
func TestMarkerSettlesWithoutTheIDOnAnOlderServer(t *testing.T) {
	s, a, p, scope := stateTest(t, "claim", sfBacklog, sfPolicy(), false)
	a.comments.refuseOperationID = true
	p.writeErr = errors.New("connection reset")
	out := s.ApplyMarker(context.Background(), scope, "asg_1", "claim", nil)
	got := settled(a, "state")
	if got["outcome"] != "uncertain" || got["operation_id"] != nil || len(a.comments.markerSettles) != 1 || strings.Contains(out.Error, "unsupported argument") {
		t.Fatal(out, a.comments.markerSettles)
	}
}
