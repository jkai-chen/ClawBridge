package bus

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

// markerLedger inserts a shared ledger row for op, as linearboard's shared
// ledger writes it: project is the row's and the record's, issue and part
// the record's ("" leaves them out, as an older record does).
func (w *watchHarness) markerLedger(op, project, issue, operation, part, state string) {
	w.t.Helper()
	record := map[string]any{"operation_id": op, "operation": operation, "project_id": project, "version": 1, "digest": strings.Repeat("a", 64), "state": state, "updated_at": "2026-10-29T06:00:00Z"}
	if state == "pending" {
		record["reason"] = "timeout"
	}
	if issue != "" {
		record["issue_id"] = issue
	}
	if part != "" {
		record["marker_part"] = part
	}
	pending := 0
	if state == "pending" {
		pending = 1
	}
	if _, err := w.s.db.Exec(`INSERT INTO linear_write_ledger(namespace,operation_id,record,updated_at,project_id,principal,pending) VALUES(?,?,?,?,?,'',?)`, strings.Repeat("0", 64), op, asJSON(record), "2026-10-29T06:00:00Z", project, pending); err != nil {
		w.t.Fatal(err)
	}
}

// ledgerAck is op's row: who acknowledged it, when, and whether it is
// still pending.
func (w *watchHarness) ledgerAck(op string) (by, at string, pending int) {
	w.t.Helper()
	if err := w.s.db.QueryRow(`SELECT acknowledged_by,acknowledged_at,pending FROM linear_write_ledger WHERE operation_id=?`, op).Scan(&by, &at, &pending); err != nil {
		w.t.Fatal(op, err)
	}
	return
}

// markerOps is the part's kept list.
func (w *watchHarness) markerOps(id, phase, part string) []string {
	w.t.Helper()
	var raw string
	if err := w.s.db.QueryRow(`SELECT `+part+`_operations FROM assignment_markers WHERE assignment_id=? AND phase=?`, id, phase).Scan(&raw); err != nil {
		w.t.Fatal(err)
	}
	var ops []string
	if err := json.Unmarshal([]byte(raw), &ops); err != nil {
		w.t.Fatal(raw, err)
	}
	return ops
}

// uncertainWrites lists the project's uncertain issue writes, as the
// project owner reads them.
func (w *watchHarness) uncertainWrites() []string {
	w.t.Helper()
	out := []string{}
	for _, row := range w.call("issue_writes_uncertain", "", map[string]any{}).(map[string]any)["writes"].([]map[string]any) {
		out = append(out, row["operation_id"].(string))
	}
	return out
}

// rereadAudits is op's reread acknowledgements in history.
func (w *watchHarness) rereadAudits(op string) []string {
	w.t.Helper()
	rows, err := w.s.db.Query(`SELECT details FROM history WHERE entity='linear_write_ledger' AND entity_key=? AND action='acknowledged'`, op)
	if err != nil {
		w.t.Fatal(err)
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var d string
		if err = rows.Scan(&d); err != nil {
			w.t.Fatal(err)
		}
		out = append(out, d)
	}
	return out
}

// stateMarker is a claim whose state part is handed out.
func stateMarker(t *testing.T) (*watchHarness, string) {
	w := newWatchHarness(t)
	w.setStatusPolicy(0, sfPolicy(true))
	w.assign("b", sfIssueNext)
	id := w.assignmentID("b")
	w.markersDue()
	if g := w.markerGo("c", id, "claim", "state"); g["action"] != "move" {
		t.Fatal(g)
	}
	return w, id
}

// stateAgain hands the state part out again after a retried settle.
func (w *watchHarness) stateAgain(id string) {
	w.t.Helper()
	w.markersDue()
	if g := w.markerGo("c", id, "claim", "state"); g["action"] != "move" {
		w.t.Fatal(g)
	}
}

func opN(n int) string { return fmt.Sprintf("marker-%016x", n) }

// An uncertain state move kept its operation ID; the retry's reread finds
// the move landed and settles done, which acknowledges the first attempt's
// record by reread: it is no longer listed, and one audit row says why.
func TestMarkerRereadAcksAnUncertainStateMove(t *testing.T) {
	w, id := stateMarker(t)
	pid, x := w.projectID(), opN(1)
	w.markerLedger(x, pid, sfIssueNext, "update", "state", "pending")
	if r := w.markerSettle("c", id, "claim", "state", map[string]any{"outcome": "uncertain", "operation_id": x}); r["state"] != "in_flight" || r["operations_dropped"] != nil {
		t.Fatal(r)
	}
	if ops := w.markerOps(id, "claim", "state"); len(ops) != 1 || ops[0] != x {
		t.Fatal(ops)
	}
	if got := w.uncertainWrites(); len(got) != 1 || got[0] != x {
		t.Fatal("not listed before:", got)
	}
	w.stateAgain(id)
	r := w.markerSettle("c", id, "claim", "state", map[string]any{"outcome": "done", "state_target": sfProgress, "state_landed": sfProgress})
	if r["state"] != "done" || r["operations_acknowledged"] != 1 {
		t.Fatal(r)
	}
	if by, at, pending := w.ledgerAck(x); by != "reread" || at == "" || pending != 1 {
		t.Fatal(by, at, pending)
	}
	if got := w.uncertainWrites(); len(got) != 0 {
		t.Fatal("still listed:", got)
	}
	audits := w.rereadAudits(x)
	if len(audits) != 1 || !strings.Contains(audits[0], `"reason":"accepted_by_reread"`) || !strings.Contains(audits[0], `"by":"reread"`) || !strings.Contains(audits[0], `"assignment_id":"`+id+`"`) || !strings.Contains(audits[0], `"part":"state"`) || !strings.Contains(audits[0], `"phase":"claim"`) {
		t.Fatal(audits)
	}
	if ops := w.markerOps(id, "claim", "state"); len(ops) != 0 {
		t.Fatal("the list was not cleared:", ops)
	}
}

// The attempt whose own reread found the move landed settles done with
// its operation ID: that record is acknowledged too.
func TestMarkerRereadAcksTheSettlingAttempt(t *testing.T) {
	w, id := stateMarker(t)
	x := opN(1)
	w.markerLedger(x, w.projectID(), sfIssueNext, "update", "state", "pending")
	w.markerSettle("c", id, "claim", "state", map[string]any{"outcome": "done", "state_target": sfProgress, "state_landed": sfProgress, "operation_id": x})
	if by, _, _ := w.ledgerAck(x); by != "reread" {
		t.Fatal(by)
	}
}

// Skipped (someone else moved the issue) closes the earlier attempts as
// reread_skipped.
func TestMarkerRereadSkippedAcksAsRereadSkipped(t *testing.T) {
	w, id := stateMarker(t)
	x := opN(1)
	w.markerLedger(x, w.projectID(), sfIssueNext, "update", "state", "pending")
	w.markerSettle("c", id, "claim", "state", map[string]any{"outcome": "uncertain", "operation_id": x})
	w.stateAgain(id)
	if r := w.markerSettle("c", id, "claim", "state", map[string]any{"outcome": "skipped", "skip_reason": "from_state"}); r["state"] != "skipped" || r["operations_acknowledged"] != 1 {
		t.Fatal(r)
	}
	if by, at, _ := w.ledgerAck(x); by != "reread_skipped" || at == "" {
		t.Fatal(by, at)
	}
	if audits := w.rereadAudits(x); len(audits) != 1 || !strings.Contains(audits[0], `"by":"reread_skipped"`) {
		t.Fatal(audits)
	}
	if got := w.uncertainWrites(); len(got) != 0 {
		t.Fatal(got)
	}
}

// Two failed attempts, then done, acknowledges both.
func TestMarkerRereadAcksTwoFailures(t *testing.T) {
	w, id := stateMarker(t)
	pid, x, y := w.projectID(), opN(1), opN(2)
	w.markerLedger(x, pid, sfIssueNext, "update", "state", "pending")
	w.markerLedger(y, pid, sfIssueNext, "update", "state", "pending")
	w.markerSettle("c", id, "claim", "state", map[string]any{"outcome": "failed", "operation_id": x})
	w.stateAgain(id)
	w.markerSettle("c", id, "claim", "state", map[string]any{"outcome": "failed", "operation_id": y})
	if ops := w.markerOps(id, "claim", "state"); len(ops) != 2 || ops[0] != x || ops[1] != y {
		t.Fatal(ops)
	}
	w.stateAgain(id)
	if r := w.markerSettle("c", id, "claim", "state", map[string]any{"outcome": "done", "state_target": sfProgress, "state_landed": sfProgress}); r["operations_acknowledged"] != 2 {
		t.Fatal(r)
	}
	for _, op := range []string{x, y} {
		if by, _, _ := w.ledgerAck(op); by != "reread" {
			t.Fatal(op, by)
		}
	}
}

// ask 1: an operation ID that is not this marker part's attempt is never
// kept and never acknowledged, and the settle still succeeds: another
// project's, another issue's, another kind's, an unknown one, an older
// record without an issue, a malformed one. One slipped into the list
// is bound again at close.
func TestMarkerRereadIgnoresForeignOperations(t *testing.T) {
	w, id := stateMarker(t)
	pid := w.projectID()
	foreign := map[string]func(op string){
		"another project": func(op string) { w.markerLedger(op, "prj_other", sfIssueNext, "update", "state", "pending") },
		"another issue":   func(op string) { w.markerLedger(op, pid, sfIssueOther, "update", "state", "pending") },
		"a label write":   func(op string) { w.markerLedger(op, pid, sfIssueNext, "update", "label", "pending") },
		"a comment":       func(op string) { w.markerLedger(op, pid, sfIssueNext, "comment", "comment", "pending") },
		"an agent update": func(op string) { w.markerLedger(op, pid, sfIssueNext, "update", "", "pending") },
		"an older record": func(op string) { w.markerLedger(op, pid, "", "update", "", "pending") },
		"unknown":         func(string) {},
	}
	var ops []string
	n := 0
	for name, insert := range foreign {
		n++
		op := opN(n)
		insert(op)
		ops = append(ops, op)
		if r := w.markerSettle("c", id, "claim", "state", map[string]any{"outcome": "uncertain", "operation_id": op}); r["state"] != "in_flight" {
			t.Fatal(name, r)
		}
		if kept := w.markerOps(id, "claim", "state"); len(kept) != 0 {
			t.Fatal(name, "kept:", kept)
		}
		w.stateAgain(id)
	}
	if r := w.markerSettle("c", id, "claim", "state", map[string]any{"outcome": "uncertain", "operation_id": "not-a-marker"}); r["state"] != "in_flight" {
		t.Fatal(r)
	}
	w.stateAgain(id)
	// A list written by hand is bound again: none of it is acknowledged.
	if _, err := w.s.db.Exec(`UPDATE assignment_markers SET state_operations=? WHERE assignment_id=? AND phase='claim'`, asJSON(ops), id); err != nil {
		t.Fatal(err)
	}
	r := w.markerSettle("c", id, "claim", "state", map[string]any{"outcome": "done", "state_target": sfProgress, "state_landed": sfProgress, "operation_id": ops[0]})
	if r["state"] != "done" || r["operations_acknowledged"] != nil {
		t.Fatal(r)
	}
	for _, op := range ops {
		var by, at string
		err := w.s.db.QueryRow(`SELECT acknowledged_by,acknowledged_at FROM linear_write_ledger WHERE operation_id=?`, op).Scan(&by, &at)
		if err == nil && (by != "" || at != "") {
			t.Fatal(op, "acknowledged:", by, at)
		}
	}
	var audits int
	if err := w.s.db.QueryRow(`SELECT COUNT(*) FROM history WHERE entity='linear_write_ledger'`).Scan(&audits); err != nil || audits != 0 {
		t.Fatal(audits, err)
	}
}

// ask 2: a record whose outcome is in by the time of the reread (applied),
// or that a person already acknowledged, is left as it is.
func TestMarkerRereadLeavesARecordedOutcome(t *testing.T) {
	w, id := stateMarker(t)
	pid, x, y := w.projectID(), opN(1), opN(2)
	w.markerLedger(x, pid, sfIssueNext, "update", "state", "pending")
	w.markerLedger(y, pid, sfIssueNext, "update", "state", "pending")
	w.markerSettle("c", id, "claim", "state", map[string]any{"outcome": "uncertain", "operation_id": x})
	w.stateAgain(id)
	w.markerSettle("c", id, "claim", "state", map[string]any{"outcome": "uncertain", "operation_id": y})
	// X's outcome lands; a person acknowledges Y.
	var record string
	if err := w.s.db.QueryRow(`SELECT record FROM linear_write_ledger WHERE operation_id=?`, x).Scan(&record); err != nil {
		t.Fatal(err)
	}
	applied := strings.Replace(strings.Replace(record, `"state":"pending"`, `"state":"accepted"`, 1), `"reason":"timeout",`, "", 1)
	if _, err := w.s.db.Exec(ledgerReplace, applied, stamp(), recordPending(applied), strings.Repeat("0", 64), x, record); err != nil {
		t.Fatal(err)
	}
	if _, err := w.s.db.Exec(`UPDATE linear_write_ledger SET acknowledged_at='2026-10-29T07:00:00Z' WHERE operation_id=?`, y); err != nil {
		t.Fatal(err)
	}
	w.stateAgain(id)
	if r := w.markerSettle("c", id, "claim", "state", map[string]any{"outcome": "done", "state_target": sfProgress, "state_landed": sfProgress}); r["operations_acknowledged"] != nil {
		t.Fatal(r)
	}
	if by, at, pending := w.ledgerAck(x); by != "" || at != "" || pending != 0 {
		t.Fatal("applied touched:", by, at, pending)
	}
	if by, at, _ := w.ledgerAck(y); by != "" || at != "2026-10-29T07:00:00Z" {
		t.Fatal("a person's ack touched:", by, at)
	}
}

// ask 2, the race: the reread acknowledges X first, then the server that
// sent X records its late outcome; the compare-and-swap still takes it,
// so the row says applied.
func TestMarkerRereadAckTakesALateOutcome(t *testing.T) {
	w, id := stateMarker(t)
	x := opN(1)
	w.markerLedger(x, w.projectID(), sfIssueNext, "update", "state", "pending")
	w.markerSettle("c", id, "claim", "state", map[string]any{"outcome": "uncertain", "operation_id": x})
	w.stateAgain(id)
	w.markerSettle("c", id, "claim", "state", map[string]any{"outcome": "done", "state_target": sfProgress, "state_landed": sfProgress})
	var record string
	if err := w.s.db.QueryRow(`SELECT record FROM linear_write_ledger WHERE operation_id=?`, x).Scan(&record); err != nil {
		t.Fatal(err)
	}
	applied := strings.Replace(strings.Replace(record, `"state":"pending"`, `"state":"accepted"`, 1), `"reason":"timeout",`, "", 1)
	r, err := w.s.db.Exec(ledgerReplace, applied, stamp(), recordPending(applied), strings.Repeat("0", 64), x, record)
	if err != nil {
		t.Fatal(err)
	}
	if n, _ := r.RowsAffected(); n != 1 {
		t.Fatal("the late outcome was refused")
	}
	var now string
	if err = w.s.db.QueryRow(`SELECT record FROM linear_write_ledger WHERE operation_id=?`, x).Scan(&now); err != nil || now != applied {
		t.Fatal(now, err)
	}
	if by, _, pending := w.ledgerAck(x); by != "reread" || pending != 0 {
		t.Fatal(by, pending)
	}
	if got := w.uncertainWrites(); len(got) != 0 {
		t.Fatal(got)
	}
}

// ask 3: the part keeps the newest five attempts; a settle that drops an
// older one says so, and the dropped ones stay listed for a person.
func TestMarkerOperationsKeepTheNewestFive(t *testing.T) {
	w, id := stateMarker(t)
	pid := w.projectID()
	var ops []string
	dropped := 0
	for i := 1; i <= 7; i++ {
		op := opN(i)
		ops = append(ops, op)
		w.markerLedger(op, pid, sfIssueNext, "update", "state", "pending")
		r := w.markerSettle("c", id, "claim", "state", map[string]any{"outcome": "uncertain", "operation_id": op})
		if d, ok := r["operations_dropped"].(int); ok {
			dropped += d
		}
		w.stateAgain(id)
	}
	if dropped != 2 {
		t.Fatal("dropped:", dropped)
	}
	if kept := w.markerOps(id, "claim", "state"); strings.Join(kept, ",") != strings.Join(ops[2:], ",") {
		t.Fatal(kept)
	}
	if r := w.markerSettle("c", id, "claim", "state", map[string]any{"outcome": "done", "state_target": sfProgress, "state_landed": sfProgress}); r["operations_acknowledged"] != 5 {
		t.Fatal(r)
	}
	if got := w.uncertainWrites(); strings.Join(got, ",") != strings.Join(ops[:2], ",") {
		t.Fatal(got)
	}
}

// The label part the same: an uncertain add kept, done acknowledges it; a
// state part's attempt is not the label's.
func TestMarkerRereadAcksAnUncertainLabelWrite(t *testing.T) {
	w := newWatchHarness(t)
	w.assign("b", sfIssueNext)
	id := w.assignmentID("b")
	pid, x, s := w.projectID(), opN(1), opN(2)
	w.markerLedger(x, pid, sfIssueNext, "update", "label", "pending")
	w.markerLedger(s, pid, sfIssueNext, "update", "state", "pending")
	w.markersDue()
	if g := w.markerGo("c", id, "claim", "label"); g["action"] != "add" {
		t.Fatal(g)
	}
	w.markerSettle("c", id, "claim", "label", map[string]any{"outcome": "uncertain", "operation_id": x})
	w.markersDue()
	w.markerGo("c", id, "claim", "label")
	w.markerSettle("c", id, "claim", "label", map[string]any{"outcome": "uncertain", "operation_id": s})
	if ops := w.markerOps(id, "claim", "label"); len(ops) != 1 || ops[0] != x {
		t.Fatal(ops)
	}
	w.markersDue()
	w.markerGo("c", id, "claim", "label")
	if r := w.markerSettle("c", id, "claim", "label", map[string]any{"outcome": "done"}); r["state"] != "done" || r["operations_acknowledged"] != 1 {
		t.Fatal(r)
	}
	if by, at, _ := w.ledgerAck(x); by != "reread" || at == "" {
		t.Fatal(by, at)
	}
	if by, _, _ := w.ledgerAck(s); by != "" {
		t.Fatal("the state attempt acknowledged by the label:", by)
	}
	if ops := w.markerOps(id, "claim", "label"); len(ops) != 0 {
		t.Fatal(ops)
	}
	if len(w.rereadAudits(x)) != 1 {
		t.Fatal(w.rereadAudits(x))
	}
}

// An older watcher sends no operation_id: the settles behave as before
// and no ledger row changes.
func TestMarkerSettleWithoutOperationIDAsBefore(t *testing.T) {
	w, id := stateMarker(t)
	x := opN(1)
	w.markerLedger(x, w.projectID(), sfIssueNext, "update", "state", "pending")
	if r := w.markerSettle("c", id, "claim", "state", map[string]any{"outcome": "uncertain"}); r["state"] != "in_flight" {
		t.Fatal(r)
	}
	w.stateAgain(id)
	if r := w.markerSettle("c", id, "claim", "state", map[string]any{"outcome": "done", "state_target": sfProgress, "state_landed": sfProgress}); r["state"] != "done" || r["operations_acknowledged"] != nil {
		t.Fatal(r)
	}
	if by, at, _ := w.ledgerAck(x); by != "" || at != "" {
		t.Fatal(by, at)
	}
	if got := w.uncertainWrites(); len(got) != 1 {
		t.Fatal(got)
	}
}
