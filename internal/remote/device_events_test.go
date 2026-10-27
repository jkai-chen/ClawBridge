package remote

import (
	"context"
	"strings"
	"sync"
	"testing"

	"clawbridge/internal/bus"
	"clawbridge/internal/devicesign"
	"clawbridge/internal/server"
)

// eventsExecutor is a hosted authority with pending device events.
type eventsExecutor struct {
	mu      sync.Mutex
	pending int
	ops     []string
}

func (e *eventsExecutor) ExecuteAs(context.Context, string, bus.Request) (bus.Result, error) {
	return bus.Result{}, nil
}

func (e *eventsExecutor) ExecuteAsIdentity(_ context.Context, _ bus.Identity, r bus.Request) (bus.Result, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.ops = append(e.ops, r.Operation)
	if r.Operation == "device_events" {
		return bus.Result{Source: "server", Data: map[string]any{"events": []any{map[string]any{"event_id": "dve_1", "kind": "added", "label": "x"}}}}, nil
	}
	return bus.Result{Source: "server", Data: map[string]any{}}, nil
}

func (e *eventsExecutor) PendingDeviceEvents(context.Context, string) (int, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.pending, nil
}

func (e *eventsExecutor) calls() string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return strings.Join(e.ops, ",")
}

// §17.3, R7: a signed answer carries the device's pending events; the
// client fetches them at most once a minute, only when some are pending and
// a receiver is set, and never from its own fetch.
func TestDeviceEventsReachTheReceiver(t *testing.T) {
	d := newDeviceServer(t)
	exec := &eventsExecutor{pending: 2}
	d.ts.Config.Handler = server.NewHandlerWithOptions(exec, nil, server.HandlerOptions{Auth: server.AuthOptions{Methods: []string{server.AuthDevice}, PublicOrigin: d.ts.URL, Devices: d.store}})
	key, _, _ := devicesign.NewKey()
	d.join(t, key)
	c := d.client(t, key, nil)
	var got [][]map[string]any
	SetDeviceEventsNotice(func(events []map[string]any) { got = append(got, events) })
	t.Cleanup(func() { SetDeviceEventsNotice(nil) })
	run := func() bus.Result {
		t.Helper()
		out, err := c.Execute(context.Background(), bus.Request{Operation: "list_projects", Workspace: "acme"})
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	out := run()
	if out.PendingDeviceEvents == nil || *out.PendingDeviceEvents != 2 {
		t.Fatal("no pending count:", out)
	}
	run()
	if exec.calls() != "list_projects,device_events,list_projects" || len(got) != 1 || got[0][0]["event_id"] != "dve_1" {
		t.Fatal(exec.calls(), got)
	}
	// Nothing pending, or no receiver: no fetch.
	SetDeviceEventsNotice(func(events []map[string]any) { got = append(got, events) })
	exec.mu.Lock()
	exec.pending, exec.ops = 0, nil
	exec.mu.Unlock()
	run()
	SetDeviceEventsNotice(nil)
	exec.mu.Lock()
	exec.pending = 3
	exec.mu.Unlock()
	run()
	if exec.calls() != "list_projects,list_projects" || len(got) != 1 {
		t.Fatal(exec.calls(), got)
	}
}
