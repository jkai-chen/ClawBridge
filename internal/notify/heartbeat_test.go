package notify

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"clawbridge/internal/bus"
)

// beats records heartbeats and fails the first.
type beats struct {
	mu   sync.Mutex
	seen []bus.Request
}

func (b *beats) Execute(_ context.Context, r bus.Request) (bus.Result, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.seen = append(b.seen, r)
	if len(b.seen) == 1 {
		return bus.Result{}, errors.New("unavailable")
	}
	return bus.Result{}, nil
}

func (b *beats) count() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.seen)
}

// KeepAlive renews the bound instance on every beat, skips while unbound,
// carries on after a failure, and stops with its context.
func TestKeepAlive(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	var mu sync.Mutex
	instance := ""
	e := &beats{}
	done := make(chan struct{})
	go func() {
		KeepAlive(ctx, e, 5*time.Millisecond, func() (bus.Request, bool) {
			mu.Lock()
			defer mu.Unlock()
			return bus.Request{Workspace: "w", Project: "p", Session: "s", Args: map[string]any{"instance": instance}}, instance != ""
		})
		close(done)
	}()
	time.Sleep(30 * time.Millisecond)
	if n := e.count(); n != 0 {
		t.Fatal("beat while unbound:", n)
	}
	mu.Lock()
	instance = "i-1"
	mu.Unlock()
	deadline := time.Now().Add(2 * time.Second)
	for e.count() < 3 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	<-done
	n := e.count()
	if n < 3 {
		t.Fatal("no beats after a failure:", n)
	}
	time.Sleep(20 * time.Millisecond)
	if e.count() != n {
		t.Fatal("beat after its context ended")
	}
	for _, r := range e.seen {
		if r.Operation != "heartbeat" || r.Session != "s" || r.Args["instance"] != "i-1" {
			t.Fatal(r)
		}
	}
}
