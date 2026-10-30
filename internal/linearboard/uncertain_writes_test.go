package linearboard

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"clawbridge/internal/bus"
	"clawbridge/internal/linearprovider"
)

const uncertainProject = "p-1"

// uncertainCreate leaves an uncertain create in l: Linear's answer failed
// after the send.
func uncertainCreate(t *testing.T, l ledger, ns, op string) {
	t.Helper()
	payload := map[string]any{"title": "SYNTHETIC-TITLE-zq81", "body": "SYNTHETIC-BODY-zq81"}
	if _, err := l.write(ns, op, ledgerMeta{Operation: "create", ProjectID: uncertainProject}, payload, func() (any, error) {
		return nil, &linearprovider.Error{Kind: "scope_metadata_missing"}
	}); !IsUncertain(err) {
		t.Fatal("not uncertain:", err)
	}
}

// C1: a record names its operation ID, its operation and its project, and
// nothing else of the write: no title, body or name.
func TestLedgerRecordKeysPinned(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "ledger")
	l := ledger{dir, time.Now}
	uncertainCreate(t, l, "ns", "op-1")
	raw, err := os.ReadFile(filepath.Join(dir, digest("ns"), digest("op-1")+".json"))
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err = json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	var keys []string
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	if want := []string{"digest", "operation", "operation_id", "project_id", "reason", "state", "updated_at", "version"}; !reflect.DeepEqual(keys, want) {
		t.Fatal("record keys:", keys)
	}
	if m["operation_id"] != "op-1" || m["operation"] != "create" || m["project_id"] != uncertainProject || m["reason"] != "scope_metadata_missing" {
		t.Fatal(m)
	}
	if strings.Contains(string(raw), "SYNTHETIC") {
		t.Fatal("the record holds the write's text")
	}
}

// An uncertain write is listed for its project; another project's isn't;
// one still being sent is in_progress; an older record, which names no
// operation, is only counted, with how to clear it (B3). An ack hides it,
// and a retry of the same operation ID still sends nothing (B2).
func TestLocalUncertainWritesListAndAck(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "ledger")
	now := time.Date(2026, 10, 29, 6, 0, 0, 0, time.UTC)
	l := ledger{dir, func() time.Time { return now }}
	uncertainCreate(t, l, "ns-a", "op-1")
	uncertainCreate(t, l, "ns-b", "op-2") // another session on this machine
	if _, err := l.write("ns-a", "op-3", ledgerMeta{Operation: "update", ProjectID: "p-other"}, "x", func() (any, error) { return nil, errors.New("reset") }); !IsUncertain(err) {
		t.Fatal(err)
	}
	// A crashed send: pending with no reason, young, then old.
	young := ledgerRecord{OperationID: "op-4", Operation: "comment", ProjectID: uncertainProject, Version: 1, Digest: digest("y"), State: "pending", UpdatedAt: now.Add(-10 * time.Second).Format(time.RFC3339Nano)}
	writeRecord(t, dir, "ns-a", "op-4", young)
	// A record from before v0.3.9: no operation, no project.
	writeRecord(t, dir, "ns-a", "op-old", ledgerRecord{Version: 1, Digest: digest("o"), State: "pending", Reason: "timeout", UpdatedAt: now.Add(-48 * time.Hour).Format(time.RFC3339Nano)})
	got := LocalUncertainWrites(dir, uncertainProject, now)
	states := map[string]string{}
	for _, w := range got.Writes {
		states[w.OperationID] = w.State
		if w.Where != "this_machine" {
			t.Fatal(w)
		}
	}
	if !reflect.DeepEqual(states, map[string]string{"op-1": "uncertain", "op-2": "uncertain", "op-4": "in_progress"}) || got.OlderLocal != 1 || !strings.Contains(got.OlderLocalHow, dir) || got.LocalNote != "" || got.LocalChecked != 5 {
		t.Fatalf("%+v", got)
	}
	if n, err := ackLocal(dir, uncertainProject, "op-1", now); err != nil || n != 1 {
		t.Fatal(n, err)
	}
	if n, err := ackLocal(dir, uncertainProject, "op-4", now); n != 0 || err == nil || !strings.Contains(err.Error(), "still being sent") {
		t.Fatal("a write still being sent was acknowledged:", n, err)
	}
	if n, _ := ackLocal(dir, uncertainProject, "op-3", now); n != 0 {
		t.Fatal("another project's record acknowledged")
	}
	after := LocalUncertainWrites(dir, uncertainProject, now)
	for _, w := range after.Writes {
		if w.OperationID == "op-1" {
			t.Fatal("an acknowledged write is still listed")
		}
	}
	if _, err := l.write("ns-a", "op-1", ledgerMeta{Operation: "create", ProjectID: uncertainProject}, map[string]any{"title": "SYNTHETIC-TITLE-zq81", "body": "SYNTHETIC-BODY-zq81"}, func() (any, error) {
		t.Fatal("an acknowledged write was sent again")
		return nil, nil
	}); !IsUncertain(err) {
		t.Fatal("the retry of an acknowledged write isn't refused:", err)
	}
}

// C2: the walk reads at most localWalkLimit files and says so.
func TestLocalUncertainWritesStopsAtTheLimit(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "ledger")
	now := time.Now().UTC()
	for i := 0; i <= localWalkLimit; i++ {
		op := fmt.Sprintf("op-%d", i)
		writeRecord(t, dir, "ns", op, ledgerRecord{OperationID: op, Operation: "create", ProjectID: uncertainProject, Version: 1, Digest: digest(op), State: "pending", Reason: "timeout", UpdatedAt: now.Format(time.RFC3339Nano)})
	}
	got := LocalUncertainWrites(dir, uncertainProject, now)
	if got.LocalChecked != localWalkLimit || len(got.Writes) != localWalkLimit || !strings.Contains(got.LocalNote, fmt.Sprintf("first %d checked", localWalkLimit)) {
		t.Fatal(got.LocalChecked, len(got.Writes), got.LocalNote)
	}
}

func writeRecord(t *testing.T, dir, ns, op string, r ledgerRecord) {
	t.Helper()
	path := filepath.Join(dir, digest(ns), digest(op)+".json")
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(r)
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
}

// writesExecutor answers the server's side of `linear writes`: recorded
// Linear writes (project_create_*) and the shared ledger's uncertain
// issue writes, and records what it was asked.
type writesExecutor struct {
	*authorityV2
	recorded []string
	server   []string
	failList bool
	asked    []string
}

func (w *writesExecutor) Execute(ctx context.Context, r bus.Request) (bus.Result, error) {
	switch r.Operation {
	case "project_create_list", "project_create_ack", "issue_writes_ack":
		w.asked = append(w.asked, r.Operation+" "+fmt.Sprint(r.Args["operation_id"]))
		var rows []any
		for _, op := range w.recorded {
			rows = append(rows, map[string]any{"operation_id": op})
		}
		return bus.Result{Data: map[string]any{"unconfirmed": rows, "acknowledged": 1}}, nil
	case "issue_writes_uncertain":
		if w.failList {
			return bus.Result{}, errors.New("unsupported operation")
		}
		var rows []any
		for _, op := range w.server {
			if want, _ := r.Args["operation_id"].(string); want == "" || want == op {
				rows = append(rows, map[string]any{"operation_id": op, "operation": "comment", "reason": "timeout", "updated_at": "2026-10-29T05:00:00Z"})
			}
		}
		return bus.Result{Data: map[string]any{"writes": rows}}, nil
	}
	return w.authorityV2.Execute(ctx, r)
}

// `linear writes` lists this machine's and the server's uncertain issue
// writes; --ack goes to the kind the ID names, and an ID naming both a
// recorded Linear write and an issue write is refused.
func TestLinearWritesListAndAckIssueWrites(t *testing.T) {
	s, a, _, scope := testV2(t)
	w := &writesExecutor{authorityV2: a, recorded: []string{"op-r", "op-both"}, server: []string{"op-s"}}
	s.cfg.Executor = w
	l := ledger{s.cfg.LedgerDirectory, time.Now}
	for _, op := range []string{"op-1", "op-both"} {
		if _, err := l.write("ns", op, ledgerMeta{Operation: "create", ProjectID: "prj"}, "x", func() (any, error) { return nil, errors.New("reset") }); !IsUncertain(err) {
			t.Fatal(err)
		}
	}
	got, err := s.UncertainIssueWrites(context.Background(), scope)
	where := map[string]string{}
	for _, x := range got.Writes {
		where[x.OperationID] = x.Where
		// A plain error keeps the reason "uncertain", so the write is
		// uncertain at once, never taken for one still being sent.
		if x.State != "uncertain" || x.Reason == "" {
			t.Fatalf("%+v", x)
		}
	}
	if err != nil || !reflect.DeepEqual(where, map[string]string{"op-1": "this_machine", "op-both": "this_machine", "op-s": "server"}) || got.ServerNote != "" {
		t.Fatalf("%+v %v", got, err)
	}
	if _, err = s.AckLinearWrite(context.Background(), scope, "op-1"); err == nil || !strings.Contains(err.Error(), "a session can't") {
		t.Fatal("a session acked:", err)
	}
	scope.Session = ""
	if _, err = s.AckLinearWrite(context.Background(), scope, "op-both"); err == nil || !strings.Contains(err.Error(), "names both") {
		t.Fatal("an ID naming both kinds:", err)
	}
	w.asked = nil
	if out, err := s.AckLinearWrite(context.Background(), scope, "op-1"); err != nil || out.(map[string]any)["acknowledged_this_machine"] != 1 {
		t.Fatal(out, err)
	}
	if _, err = s.AckLinearWrite(context.Background(), scope, "op-s"); err != nil {
		t.Fatal(err)
	}
	if _, err = s.AckLinearWrite(context.Background(), scope, "op-r"); err != nil {
		t.Fatal(err)
	}
	if want := []string{"project_create_list <nil>", "project_create_list <nil>", "issue_writes_ack op-s", "project_create_list <nil>", "project_create_ack op-r"}; !reflect.DeepEqual(w.asked, want) {
		t.Fatal(w.asked)
	}
	w.failList = true
	if got, err = s.UncertainIssueWrites(context.Background(), scope); err != nil || got.ServerNote == "" || len(got.Writes) != 1 {
		t.Fatalf("an older server: %+v %v", got, err)
	}
}
