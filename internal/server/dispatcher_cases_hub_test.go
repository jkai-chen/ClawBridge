package server

import (
	"context"
	"encoding/json"
	"os"
	"sync"
	"testing"
	"time"

	"clawbridge/internal/bus"
)

// nextCases answers cases/next's checks: no case until open is set.
type nextCases struct {
	ModelStore
	mu     sync.Mutex
	checks int
	open   bool
}

func (n *nextCases) ModelNextCase(context.Context, string, int) (bus.ModelNext, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.checks++
	if n.open {
		return bus.ModelNext{Case: map[string]any{"id": "case_1"}}, nil
	}
	return bus.ModelNext{}, nil
}

func (n *nextCases) count() int { n.mu.Lock(); defer n.mu.Unlock(); return n.checks }

// With the hub, cases/next waits for a new case's notification instead of
// re-reading every second, and answers as soon as one comes; without it,
// it re-reads every second as before.
func TestCasesNextWaitsOnTheHub(t *testing.T) {
	hub := NewStreamHub()
	store := &nextCases{}
	body := map[string]json.RawMessage{"wait_seconds": json.RawMessage(`20`)}
	polls := &casePolls{held: map[string]bool{}, limit: 10}
	type answer struct {
		out any
		err error
	}
	done := make(chan answer, 1)
	go func() {
		out, err := serveCasesNext(context.Background(), store, hub, "tok", body, 3, polls, nil, nil)
		done <- answer{out, err}
	}()
	time.Sleep(1500 * time.Millisecond)
	if n := store.count(); n != 1 {
		t.Fatalf("checked %d times without a new case", n)
	}
	// A listener reconnect wakes every poll to re-read.
	hub.WakeAll()
	deadline := time.Now().Add(time.Second)
	for store.count() != 2 {
		if time.Now().After(deadline) {
			t.Fatal("a reconnect's wake didn't re-read")
		}
		time.Sleep(10 * time.Millisecond)
	}
	store.mu.Lock()
	store.open = true
	store.mu.Unlock()
	woken := time.Now()
	hub.notified(bus.CaseChannel, "prj_any")
	select {
	case a := <-done:
		if a.err != nil || a.out.(map[string]any)["case"] == nil || time.Since(woken) > time.Second {
			t.Fatal(a, time.Since(woken))
		}
	case <-time.After(3 * time.Second):
		t.Fatal("a new case didn't wake cases/next")
	}
	hub.mu.Lock()
	left := len(hub.cases)
	hub.mu.Unlock()
	if left != 0 {
		t.Fatal("a finished poll stayed subscribed")
	}

	// A missed notification costs at most caseSafety: it still re-reads.
	quiet := &nextCases{}
	body = map[string]json.RawMessage{"wait_seconds": json.RawMessage(`7`)}
	if out, err := serveCasesNext(context.Background(), quiet, hub, "tok", body, 3, polls, nil, nil); err != nil || out.(map[string]any)["case"] != nil {
		t.Fatal(out, err)
	}
	if n := quiet.count(); n != 3 {
		t.Fatalf("checked %d times in 7 s with the hub, want now, at 5 s and at the end", n)
	}

	// Without a hub: a re-read every second.
	plain := &nextCases{}
	body = map[string]json.RawMessage{"wait_seconds": json.RawMessage(`2`)}
	if out, err := serveCasesNext(context.Background(), plain, nil, "tok", body, 3, polls, nil, nil); err != nil || out.(map[string]any)["case"] != nil {
		t.Fatal(out, err)
	}
	if n := plain.count(); n < 3 {
		t.Fatalf("checked %d times in 2 s without a hub", n)
	}
}

// The hub LISTENs on cb_case: a notification on Postgres wakes a waiting
// cases/next.
func TestPostgresCasesNextWokenByNotify(t *testing.T) {
	dsn := os.Getenv("CLAWBRIDGE_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("reviewer supplies disposable Postgres DSN")
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	hub := NewStreamHub()
	go hub.Listen(ctx, dsn)
	store := &nextCases{}
	polls := &casePolls{held: map[string]bool{}, limit: 10}
	done := make(chan any, 1)
	go func() {
		out, _ := serveCasesNext(ctx, store, hub, "tok", map[string]json.RawMessage{"wait_seconds": json.RawMessage(`20`)}, 3, polls, nil, nil)
		done <- out
	}()
	// Past the listener's connect, whose wake re-reads once.
	time.Sleep(1500 * time.Millisecond)
	store.mu.Lock()
	store.open = true
	store.mu.Unlock()
	db, err := sqlOpen(dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err = db.Exec(`SELECT pg_notify($1,'prj_any')`, bus.CaseChannel); err != nil {
		t.Fatal(err)
	}
	select {
	case out := <-done:
		if out.(map[string]any)["case"] == nil {
			t.Fatal(out)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("cb_case didn't wake cases/next")
	}
}
