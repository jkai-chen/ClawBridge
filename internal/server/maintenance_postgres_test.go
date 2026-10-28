package server

import (
	"context"
	"database/sql"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"clawbridge/internal/bus"
)

// drainSchemaStore is a hosted store in a schema of its own, since the
// maintenance row is one per database, and that schema's connection URL.
func drainSchemaStore(t *testing.T) (*bus.Store, string) {
	t.Helper()
	dsn := os.Getenv("CLAWBRIDGE_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("reviewer supplies disposable Postgres DSN")
	}
	ctx := context.Background()
	admin, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { admin.Close() })
	schema := strings.ReplaceAll(awUnique("drain"), "-", "_")
	if _, err = admin.ExecContext(ctx, `CREATE SCHEMA `+schema); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { admin.ExecContext(context.Background(), `DROP SCHEMA `+schema+` CASCADE`) })
	parsed, err := url.Parse(dsn)
	if err != nil {
		t.Fatal(err)
	}
	q := parsed.Query()
	q.Set("search_path", schema)
	parsed.RawQuery = q.Encode()
	s, err := bus.OpenPostgres(ctx, parsed.String(), awUnique("owner")+"@drain-test.example")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s, parsed.String()
}

// servingDrain is one serving process's drain, with its hub listening.
func servingDrain(t *testing.T, ctx context.Context, store *bus.Store, dsn, name string, listen bool) (*Drain, *StreamHub) {
	t.Helper()
	d := NewDrain(store, name, time.Now)
	hub := NewStreamHub()
	hub.maintenance = d.Invalidate
	d.onStart, d.onEnd = hub.StartDrain, hub.EndDrain
	if listen {
		go hub.Listen(ctx, dsn)
	}
	go d.Run(ctx)
	return d, hub
}

func within(t *testing.T, wait time.Duration, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(wait)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatal(what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// A maintenance started by the operator's process reaches a listening
// server within a second (the notify), and one whose listener is down
// within its 5 s poll; its end does too.
func TestPostgresMaintenanceReachesServers(t *testing.T) {
	store, dsn := drainSchemaStore(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	listening, _ := servingDrain(t, ctx, store, dsn, "clawbridge-server-listening", true)
	polling, _ := servingDrain(t, ctx, store, dsn, "clawbridge-server-polling", false)
	// Let both settle on "none" and the listener connect.
	time.Sleep(300 * time.Millisecond)
	if _, err := store.StartMaintenance(ctx, "deploy", "operator", time.Minute); err != nil {
		t.Fatal(err)
	}
	within(t, time.Second, "the notify didn't reach the listening server", listening.Quiet)
	within(t, maintenancePoll+time.Second, "the poll didn't reach the other server", polling.Quiet)
	if _, err := store.EndMaintenance(ctx); err != nil {
		t.Fatal(err)
	}
	within(t, time.Second, "the end didn't reach the listening server", func() bool { return !listening.Quiet() })
	if _, paused := listening.Paused(); !paused {
		t.Fatal("no resume grace after the end")
	}
}

// --wait returns drained when both servers have drained, and names the one
// still holding a request while it does.
func TestPostgresMaintenanceWaitTwoServers(t *testing.T) {
	store, dsn := drainSchemaStore(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	a, _ := servingDrain(t, ctx, store, dsn, "clawbridge-server-a", true)
	_, _ = servingDrain(t, ctx, store, dsn, "clawbridge-server-b", true)
	time.Sleep(300 * time.Millisecond)
	held := a.Begin()
	if _, err := store.StartMaintenance(ctx, "deploy", "operator", time.Minute); err != nil {
		t.Fatal(err)
	}
	var out strings.Builder
	err := waitDrained(ctx, store, 2*time.Second, &out)
	if err == nil || !strings.Contains(err.Error(), "clawbridge-server-a") || strings.Contains(err.Error(), "clawbridge-server-b") {
		t.Fatal("the wait didn't name the undrained server:", err)
	}
	held()
	out.Reset()
	if err = waitDrained(ctx, store, 3*time.Second, &out); err != nil || out.String() != "drained: 2 of 2 live servers\n" {
		t.Fatalf("%v %q", err, out.String())
	}
}

// Through the handler: in maintenance an open consume stream is told
// maintenance, then bye; a new stream and a stream ticket get 503
// maintenance; a pure read still answers; and the server drains with the
// stream having been open.
func TestPostgresDrainClosesStreams(t *testing.T) {
	f := newSTFixture(t, StreamLimits{}, bus.QueueLimits{})
	drainStore, dsn := drainSchemaStore(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	d := NewDrain(drainStore, "clawbridge-server-stream", time.Now)
	d.onStart, d.onEnd = f.hub.StartDrain, f.hub.EndDrain
	listener := NewStreamHub()
	listener.maintenance = d.Invalidate
	go listener.Listen(ctx, dsn)
	go d.Run(ctx)
	verifier := VerifierFunc(func(_ context.Context, token string) (Identity, error) {
		return Identity{Principal: strings.TrimPrefix(token, "tok-"), Subject: "sub-" + strings.TrimPrefix(token, "tok-")}, nil
	})
	host := httptest.NewServer(NewHandlerWithOptions(f.store, verifier, HandlerOptions{Streams: f.hub, Drain: d}))
	defer host.Close()
	f.host = host
	f.send("b", "before the drain")
	stream := f.open("b", bus.StreamConsume)
	stream.must(5*time.Second, "message")
	ticket := f.ticket("b", bus.StreamConsume)
	time.Sleep(300 * time.Millisecond)
	m, err := drainStore.StartMaintenance(ctx, "deploy", "operator", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	e := stream.must(2*time.Second, "maintenance")
	if retry, _ := e.data["retry_after"].(float64); retry != 30 {
		t.Fatal(e.data)
	}
	stream.must(time.Second, "bye")
	if resp := f.get(f.url("b", bus.StreamConsume), f.principalOf("b"), ticket); resp.StatusCode != 503 || resp.Header.Get("Retry-After") == "" {
		t.Fatal("a new stream in maintenance:", resp.Status)
	}
	if resp := f.post(f.principalOf("b"), bus.Request{Operation: "stream_ticket", Workspace: f.workspace, Project: "p", Session: "b", Args: map[string]any{"mode": "consume", "instance": "b-inst"}}); resp.StatusCode != 503 {
		t.Fatal("stream_ticket in maintenance:", resp.Status)
	}
	if resp := f.post(f.principalOf("b"), bus.Request{Operation: "list_sessions", Workspace: f.workspace, Project: "p"}); resp.StatusCode != 200 {
		t.Fatal("a pure read in maintenance:", resp.Status)
	}
	within(t, 7*time.Second, "never drained", func() bool {
		list, err := drainStore.MaintenanceInstances(ctx)
		return err == nil && len(list) == 1 && list[0].Drained
	})
	_ = m
}
