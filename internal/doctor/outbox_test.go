package doctor

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"clawbridge/internal/bus"
	"clawbridge/internal/outbox"
)

type maintenanceExecutor struct {
	until time.Time
}

func (maintenanceExecutor) Execute(context.Context, bus.Request) (bus.Result, error) {
	return bus.Result{}, nil
}
func (maintenanceExecutor) ResolveProject(context.Context, string, string, string) (string, string, error) {
	return "", "", nil
}
func (m maintenanceExecutor) MaintenanceUntil() (time.Time, bool) { return m.until, !m.until.IsZero() }

func TestServerMaintenanceCheck(t *testing.T) {
	var got []Check
	add := func(c Check) { got = append(got, c) }
	checkServerMaintenance(maintenanceExecutor{}, add)
	if len(got) != 0 {
		t.Fatal("shown outside maintenance", got)
	}
	checkServerMaintenance(maintenanceExecutor{until: time.Date(2026, 10, 28, 22, 10, 0, 0, time.UTC)}, add)
	if len(got) != 1 || got[0].ID != "server.maintenance" || got[0].Status != Warn || !strings.Contains(got[0].Message, "until 22:10 UTC") {
		t.Fatal(got)
	}
}

func TestClientOutboxCheck(t *testing.T) {
	dir := t.TempDir()
	config := Config{Mode: "cloud", Cache: filepath.Join(dir, "mirror.db")}
	check := func() Check {
		t.Helper()
		var got []Check
		checkClientOutbox(context.Background(), config, func(c Check) { got = append(got, c) })
		if len(got) != 1 || got[0].ID != "client.outbox" {
			t.Fatal(got)
		}
		return got[0]
	}
	if c := check(); c.Status != Pass {
		t.Fatal(c)
	}
	if _, err := os.Stat(filepath.Join(dir, "outbox.db")); !os.IsNotExist(err) {
		t.Fatal("doctor created the outbox", err)
	}
	ob, err := outbox.Open(outbox.PathFor(config.Cache))
	if err != nil {
		t.Fatal(err)
	}
	defer ob.Close()
	now := time.Now()
	ob.SetNow(func() time.Time { return now })
	send := bus.Request{Operation: "send", Workspace: "w", Project: "p", Session: "s", Args: map[string]any{"instance": "i", "to": "x", "body": "b", "operation_id": "op-1"}}
	if _, err = ob.Enqueue(context.Background(), "https://bus.example.test", outbox.Scope{}, send); err != nil {
		t.Fatal(err)
	}
	if c := check(); c.Status != Pass || !strings.Contains(c.Message, "1 write(s) wait") {
		t.Fatal(c)
	}
	// Over 10 minutes: a warning that names the commands.
	ob.SetNow(func() time.Time { return now.Add(-outbox.WarnAge - time.Second) })
	send.Args["operation_id"] = "op-2"
	ob.Enqueue(context.Background(), "https://bus.example.test", outbox.Scope{}, send)
	if c := check(); c.Status != Warn || !strings.Contains(c.Action, "clawbridge outbox list") {
		t.Fatal(c)
	}
	// A refused write is a dead letter: a failure.
	ob.SetNow(time.Now)
	ob.Drain(context.Background(), "https://bus.example.test", outbox.Scope{}, nil, func(context.Context, bus.Request) outbox.Answer {
		return outbox.Answer{Kind: outbox.Refused, Class: "forbidden"}
	})
	if c := check(); c.Status != Fail || !strings.Contains(c.Message, "dead letters") {
		t.Fatal(c)
	}
	t.Setenv("CLAWBRIDGE_CLIENT_OUTBOX", "off")
	var none []Check
	checkClientOutbox(context.Background(), config, func(c Check) { none = append(none, c) })
	checkClientOutbox(context.Background(), Config{Mode: "local"}, func(c Check) { none = append(none, c) })
	if len(none) != 0 {
		t.Fatal(none)
	}
}
