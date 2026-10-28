package server

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"clawbridge/internal/bus"
)

// fakeMaintenance is the operator's database.
type fakeMaintenance struct {
	state     bus.Maintenance
	started   []string
	ttl       time.Duration
	ended     int
	instances [][]bus.MaintenanceInstance
	lists     int
}

func (f *fakeMaintenance) MaintenanceState(context.Context) (bus.Maintenance, error) {
	return f.state, nil
}

func (f *fakeMaintenance) StartMaintenance(_ context.Context, reason, by string, ttl time.Duration) (bus.Maintenance, error) {
	f.started, f.ttl = append(f.started, reason+"|"+by), ttl
	f.state = bus.Maintenance{Active: true, StartedAt: drainStart, EndsAt: drainStart.Add(ttl), Reason: reason, StartedBy: by}
	return f.state, nil
}

func (f *fakeMaintenance) EndMaintenance(context.Context) (bool, error) {
	f.ended++
	was := f.state.Active
	f.state = bus.Maintenance{}
	return was, nil
}

func (f *fakeMaintenance) MaintenanceInstances(context.Context) ([]bus.MaintenanceInstance, error) {
	i := min(f.lists, len(f.instances)-1)
	f.lists++
	return f.instances[i], nil
}

func useFakeMaintenance(t *testing.T, f *fakeMaintenance) {
	t.Helper()
	t.Setenv("CLAWBRIDGE_DATABASE_URL", "postgres://example.invalid/db")
	saved, savedPoll := openMaintenanceStore, drainPoll
	openMaintenanceStore = func(context.Context, string) (maintenanceStore, func(), error) { return f, func() {}, nil }
	drainPoll = time.Millisecond
	t.Cleanup(func() { openMaintenanceStore, drainPoll = saved, savedPoll })
}

func operatorRun(args ...string) (string, error) {
	var out bytes.Buffer
	err := RunOperator(context.Background(), args, &out)
	return out.String(), err
}

// Each refusal comes before the database is opened.
func TestMaintenanceOperatorRefusals(t *testing.T) {
	f := &fakeMaintenance{instances: [][]bus.MaintenanceInstance{{}}}
	useFakeMaintenance(t, f)
	for _, c := range []struct {
		args []string
		want string
	}{
		{[]string{"maintenance"}, "usage:"},
		{[]string{"maintenance", "pause"}, "usage:"},
		{[]string{"maintenance", "start", "--wait", "90s", "--confirm"}, "needs --reason"},
		{[]string{"maintenance", "start", "--reason", "deploy", "--confirm"}, "needs --wait DURATION, or --no-wait"},
		{[]string{"maintenance", "start", "--reason", "deploy", "--wait", "90s", "--no-wait", "--confirm"}, "needs --wait DURATION, or --no-wait"},
		{[]string{"maintenance", "start", "--reason", "deploy", "--ttl", "61m", "--wait", "90s", "--confirm"}, "at most 60m"},
		{[]string{"maintenance", "start", "--reason", "deploy", "--ttl", "0s", "--wait", "90s", "--confirm"}, "at most 60m"},
		{[]string{"maintenance", "start", "--reason", "deploy", "--ttl", "1m", "--wait", "2m", "--confirm"}, "at most --ttl"},
		{[]string{"maintenance", "start", "--reason", "deploy", "--wait", "90s"}, "repeat with --confirm"},
		{[]string{"maintenance", "start", "--reason", "deploy", "--wait", "90s", "extra", "--confirm"}, "usage:"},
		{[]string{"maintenance", "end"}, "repeat with --confirm"},
		{[]string{"maintenance", "end", "--reason", "x", "--confirm"}, "takes only --confirm"},
		{[]string{"maintenance", "show", "--confirm"}, "takes no flags"},
	} {
		if _, err := operatorRun(c.args...); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Fatalf("%v: %v", c.args, err)
		}
	}
	if len(f.started) != 0 || f.ended != 0 || f.lists != 0 {
		t.Fatal("a refusal touched the database:", f)
	}
}

// start --wait returns drained once every live server has drained;
// instances that aren't live don't hold it.
func TestMaintenanceOperatorWaitDrained(t *testing.T) {
	f := &fakeMaintenance{instances: [][]bus.MaintenanceInstance{
		{{Instance: "clawbridge-server-a", Live: true}, {Instance: "clawbridge-server-b", Live: true, Drained: true}, {Instance: "clawbridge-server-old"}},
		{{Instance: "clawbridge-server-a", Live: true, Drained: true}, {Instance: "clawbridge-server-b", Live: true, Drained: true}, {Instance: "clawbridge-server-old"}},
	}}
	useFakeMaintenance(t, f)
	out, err := operatorRun("maintenance", "start", "--reason", "deploy v0.3.9", "--wait", "5s", "--confirm")
	if err != nil {
		t.Fatal(err)
	}
	if f.started[0] != "deploy v0.3.9|operator" || f.ttl != bus.MaintenanceDefaultTTL {
		t.Fatal(f.started, f.ttl)
	}
	if out != "maintenance on until 22:15:00Z\ndrained: 2 of 2 live servers\n" {
		t.Fatalf("%q", out)
	}
}

// A wait that runs out names the servers that haven't drained and leaves
// maintenance on (D7).
func TestMaintenanceOperatorWaitNamesUndrained(t *testing.T) {
	f := &fakeMaintenance{instances: [][]bus.MaintenanceInstance{{{Instance: "clawbridge-server-a", Live: true, Drained: true}, {Instance: "clawbridge-server-b", Live: true}}}}
	useFakeMaintenance(t, f)
	_, err := operatorRun("maintenance", "start", "--reason", "deploy", "--ttl", "20m", "--wait", "20ms", "--confirm")
	if err == nil || !strings.Contains(err.Error(), "not drained after 20ms: clawbridge-server-b;") || strings.Contains(err.Error(), "server-a") {
		t.Fatal(err)
	}
	if f.ttl != 20*time.Minute || f.ended != 0 || !f.state.Active {
		t.Fatal("a partial drain must leave maintenance on")
	}
}

// --no-wait starts at once and says what that risks; end and show.
func TestMaintenanceOperatorNoWaitEndShow(t *testing.T) {
	f := &fakeMaintenance{instances: [][]bus.MaintenanceInstance{{{Instance: "clawbridge-server-a", Live: true}}}}
	useFakeMaintenance(t, f)
	out, err := operatorRun("maintenance", "start", "--reason", "emergency", "--no-wait", "--confirm")
	if err != nil || !strings.Contains(out, "may be cut by a swap now") || f.lists != 0 {
		t.Fatal(out, err, f.lists)
	}
	out, err = operatorRun("maintenance", "show")
	if err != nil || !strings.Contains(out, `"reason":"emergency"`) || !strings.Contains(out, `"instance":"clawbridge-server-a"`) {
		t.Fatal(out, err)
	}
	if out, err = operatorRun("maintenance", "end", "--confirm"); err != nil || !strings.HasPrefix(out, "maintenance ended") {
		t.Fatal(out, err)
	}
	if out, err = operatorRun("maintenance", "end", "--confirm"); err != nil || out != "no maintenance was on\n" {
		t.Fatal(out, err)
	}
}
