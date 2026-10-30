package linearboard

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// memLedgerStore is the bus's shared ledger in memory.
type memLedgerStore struct {
	mu        sync.Mutex
	records   map[string]string
	unsettled map[string]bool
	failNext  string
	// scopes keeps each row's project and principal.
	scopes map[string]string
}

func newMemLedgerStore() *memLedgerStore {
	return &memLedgerStore{records: map[string]string{}, unsettled: map[string]bool{}}
}
func (m *memLedgerStore) LedgerInsert(_ context.Context, ns, op, record, project, who string) (string, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if r, ok := m.records[ns+"/"+op]; ok {
		return r, false, nil
	}
	m.records[ns+"/"+op] = record
	if m.scopes == nil {
		m.scopes = map[string]string{}
	}
	m.scopes[ns+"/"+op] = project + " " + who
	return "", true, nil
}
func (m *memLedgerStore) LedgerGet(_ context.Context, ns, op string) (string, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.records[ns+"/"+op]
	return r, ok, nil
}
func (m *memLedgerStore) LedgerReplace(_ context.Context, ns, op, from, to string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.failNext == "replace" {
		m.failNext = ""
		return errors.New("store down")
	}
	if m.records[ns+"/"+op] != from {
		return errors.New("conflict")
	}
	m.records[ns+"/"+op] = to
	return nil
}
func (m *memLedgerStore) LedgerDelete(_ context.Context, ns, op, record string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.records[ns+"/"+op] != record {
		return errors.New("conflict")
	}
	delete(m.records, ns+"/"+op)
	return nil
}
func (m *memLedgerStore) LedgerMarkUnsettled(_ context.Context, ns, op string, on bool) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if on {
		m.unsettled[ns+"/"+op] = true
	} else {
		delete(m.unsettled, ns+"/"+op)
	}
	return nil
}
func (m *memLedgerStore) LedgerUnsettled(_ context.Context, ns string, limit int) ([]string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := []string{}
	for k := range m.unsettled {
		if strings.HasPrefix(k, ns+"/") && len(out) < limit {
			out = append(out, strings.TrimPrefix(k, ns+"/"))
		}
	}
	return out, nil
}

// Both ledgers answer every case the same way.
func TestSharedLedgerAnswersLikeThePrivateLedger(t *testing.T) {
	receipt := map[string]any{"kind": "comment", "outcome": "accepted", "id": "c1000000-0000-4000-8000-000000000001", "issue_id": issueID}
	for name, open := range map[string]func(t *testing.T) OperationLedger{
		"private": func(t *testing.T) OperationLedger { return ledger{filepath.Join(t.TempDir(), "ledger"), time.Now} },
		"shared":  func(*testing.T) OperationLedger { return SharedLedger(newMemLedgerStore(), nil)(context.Background()) },
	} {
		l := open(t)
		sent := 0
		send := func(r any, err error) func() (any, error) {
			return func() (any, error) { sent++; return r, err }
		}
		// Accepted, then answered from the record without a second send.
		if got, err := l.write("ns", "op-1", ledgerMeta{}, "p", send(receipt, nil)); err != nil || got == nil {
			t.Fatal(name, got, err)
		}
		if got, err := l.write("ns", "op-1", ledgerMeta{}, "p", send(nil, errors.New("must not run"))); err != nil || got.(map[string]any)["id"] != receipt["id"] || sent != 1 {
			t.Fatal(name, "accepted retry", got, err, sent)
		}
		if got, found, err := l.lookup("ns", "op-1", "p"); err != nil || !found || got.(map[string]any)["id"] != receipt["id"] {
			t.Fatal(name, "lookup", got, found, err)
		}
		if _, err := l.write("ns", "op-1", ledgerMeta{}, "other", send(receipt, nil)); err == nil || !strings.Contains(err.Error(), "different scoped payload") {
			t.Fatal(name, "another payload under the same ID", err)
		}
		// A replay finds the record whatever its payload; a lookup does not.
		if _, _, err := l.lookup("ns", "op-1", "other"); err == nil || !strings.Contains(err.Error(), "different scoped payload") {
			t.Fatal(name, "lookup with another payload", err)
		}
		if got, found, err := l.lookupAny("ns", "op-1"); err != nil || !found || got.(map[string]any)["id"] != receipt["id"] {
			t.Fatal(name, "lookupAny", got, found, err)
		}
		if _, found, err := l.lookupAny("ns", "op-missing"); err != nil || found {
			t.Fatal(name, "lookupAny of a missing operation", found, err)
		}
		// Not dispatched: the ID stays free, and the error keeps its type.
		var never notDispatchedError
		if _, err := l.write("ns", "op-2", ledgerMeta{}, "p", send(nil, notDispatched(errors.New("timeout")))); !errors.As(err, &never) {
			t.Fatal(name, "not dispatched", err)
		}
		if _, found, _ := l.lookup("ns", "op-2", "p"); found {
			t.Fatal(name, "a never-dispatched operation kept its record")
		}
		// Refused: recorded, and a retry is refused without sending.
		var refused refusedBeforeDispatchError
		if _, err := l.write("ns", "op-3", ledgerMeta{}, "p", send(nil, refusedBeforeDispatch(errors.New("binding changed")))); !errors.As(err, &refused) {
			t.Fatal(name, "refusal type", err)
		}
		before := sent
		if _, err := l.write("ns", "op-3", ledgerMeta{}, "p", send(receipt, nil)); err == nil || !strings.Contains(err.Error(), "binding changed") || sent != before {
			t.Fatal(name, "refusal not kept", err)
		}
		// Uncertain: stays pending, and a retry sends nothing.
		if _, err := l.write("ns", "op-4", ledgerMeta{}, "p", send(nil, errors.New("connection reset"))); !IsUncertain(err) {
			t.Fatal(name, "uncertain type", err)
		}
		before = sent
		if _, err := l.write("ns", "op-4", ledgerMeta{}, "p", send(receipt, nil)); !IsUncertain(err) || IsInProgress(err) || sent != before {
			t.Fatal(name, "uncertain operation re-sent or taken for in progress", err)
		}
		// Unsettled comments.
		if err := l.markUnsettled("ns", "op-1"); err != nil {
			t.Fatal(name, err)
		}
		if got := l.unsettled("ns", 10); len(got) != 1 || got[0] != "op-1" {
			t.Fatal(name, "unsettled", got)
		}
		if state, id, ok := l.peek("ns", "op-1"); !ok || state != "accepted" || id != receipt["id"] {
			t.Fatal(name, "peek", state, id, ok)
		}
		l.dropUnsettled("ns", "op-1")
		if got := l.unsettled("ns", 10); len(got) != 0 {
			t.Fatal(name, "dropped", got)
		}
		if state, _, ok := l.peek("ns", "op-9"); !ok || state != "absent" {
			t.Fatal(name, "absent peek", state)
		}
	}
}

// R2-M1: a second caller with the same ID while the first dispatches finds
// the pending record and sends nothing.
func TestSharedLedgerNeverDispatchesTwice(t *testing.T) {
	l := SharedLedger(newMemLedgerStore(), nil)(context.Background())
	inside, release := make(chan struct{}), make(chan struct{})
	done := make(chan error, 1)
	go func() {
		_, err := l.write("ns", "op", ledgerMeta{}, "p", func() (any, error) {
			close(inside)
			<-release
			return map[string]any{"kind": "comment", "outcome": "accepted", "id": "c1000000-0000-4000-8000-000000000001", "issue_id": issueID}, nil
		})
		done <- err
	}()
	<-inside
	second := 0
	if _, err := l.write("ns", "op", ledgerMeta{}, "p", func() (any, error) { second++; return nil, nil }); !IsInProgress(err) || IsUncertain(err) || second != 0 {
		t.Fatal("a concurrent retry dispatched, or was not told it is in progress", err, second)
	}
	if _, _, err := l.lookup("ns", "op", "p"); !IsInProgress(err) {
		t.Fatal("lookup of a dispatching operation", err)
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

// A settle the store cannot record leaves the outcome uncertain, never lost.
func TestSharedLedgerUnrecordedSettle(t *testing.T) {
	store := newMemLedgerStore()
	l := SharedLedger(store, nil)(context.Background())
	store.failNext = "replace"
	r, err := l.write("ns", "op", ledgerMeta{}, "p", func() (any, error) {
		return map[string]any{"kind": "comment", "outcome": "accepted", "id": "c1000000-0000-4000-8000-000000000001", "issue_id": issueID}, nil
	})
	var re *ReceiptError
	if r == nil || !errors.As(err, &re) || re.Outcome != "accepted_ledger_unrecorded" {
		t.Fatal(r, err)
	}
}

// A pending record older than a write can take is uncertain, not in progress.
func TestSharedLedgerStalePendingIsUncertain(t *testing.T) {
	now := time.Date(2026, 10, 23, 12, 0, 0, 0, time.UTC)
	store := newMemLedgerStore()
	l := SharedLedger(store, func() time.Time { return now })(context.Background())
	store.LedgerInsert(context.Background(), digest("ns"), "op", encodeRecord(ledgerRecord{Version: 1, Digest: digest("p"), State: "pending", UpdatedAt: now.Format(time.RFC3339Nano)}), "", "")
	if _, _, err := l.lookup("ns", "op", "p"); !IsInProgress(err) {
		t.Fatal("young pending", err)
	}
	now = now.Add(inProgressFor + time.Second)
	if _, _, err := l.lookup("ns", "op", "p"); !IsUncertain(err) || IsInProgress(err) {
		t.Fatal("stale pending", err)
	}
	sent := 0
	if _, err := l.write("ns", "op", ledgerMeta{}, "p", func() (any, error) { sent++; return nil, nil }); !IsUncertain(err) || sent != 0 {
		t.Fatal("stale pending re-sent", err)
	}
}

// PR-B: a shared ledger row names its ClawBridge project and the person
// whose write it is, and its record its operation, never the payload.
func TestSharedLedgerRowNamesItsScope(t *testing.T) {
	store := newMemLedgerStore()
	l := SharedLedgerAs(store, nil, "person@example.test")(context.Background())
	if _, err := l.write("ns", "op-1", ledgerMeta{Operation: "update", ProjectID: "prj"}, map[string]any{"title": "SYNTHETIC-zq81"}, func() (any, error) { return nil, errors.New("reset") }); !IsUncertain(err) {
		t.Fatal(err)
	}
	key := digest("ns") + "/op-1"
	if store.scopes[key] != "prj person@example.test" {
		t.Fatal(store.scopes)
	}
	record := store.records[key]
	if !strings.Contains(record, `"operation_id":"op-1"`) || !strings.Contains(record, `"operation":"update"`) || !strings.Contains(record, `"project_id":"prj"`) || strings.Contains(record, "SYNTHETIC") {
		t.Fatal(record)
	}
}
