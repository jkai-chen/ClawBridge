package cli

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"clawbridge/internal/bus"
	"clawbridge/internal/linearboard"
)

const (
	statusProject = "55555555-5555-4555-8555-555555555555"
	statusPlanned = "66666666-6666-4666-8666-666666666666"
)

// statusExecutor records what the CLI sends and answers with data or err.
type statusExecutor struct {
	sent []bus.Request
	data map[string]any
	err  error
}

func (e *statusExecutor) Execute(_ context.Context, r bus.Request) (bus.Result, error) {
	e.sent = append(e.sent, r)
	return bus.Result{Data: e.data}, e.err
}

func statusRun(t *testing.T, e *statusExecutor, session string, args ...string) (map[string]any, error) {
	t.Helper()
	v, pos, err := parseLinear("project-status", args, &bytes.Buffer{})
	if err != nil {
		return nil, err
	}
	if len(args) > 0 && args[0] == "set" {
		t.Fatal("pass set as statusSet")
	}
	return statusCall(t, e, session, v, pos)
}

func statusSetRun(t *testing.T, e *statusExecutor, session string, args ...string) (map[string]any, error) {
	t.Helper()
	v, pos, err := parseLinear("project-status", args, &bytes.Buffer{})
	if err != nil {
		return nil, err
	}
	v.statusSet = true
	return statusCall(t, e, session, v, pos)
}

func statusCall(t *testing.T, e *statusExecutor, session string, v linearFlags, pos []string) (map[string]any, error) {
	service := linearboard.New(linearboard.Config{Executor: e})
	scope := bus.Request{Workspace: "w", Project: "p", Session: session, Args: map[string]any{}}
	data, err := linearProjectStatus(context.Background(), service, scope, Config{}, v, pos[0])
	out, _ := data.(map[string]any)
	return out, err
}

// The read needs no profile and sends the project and suggest; set's
// flags are refused on it.
func TestLinearProjectStatusRead(t *testing.T) {
	typing(t, false, "")
	e := &statusExecutor{data: map[string]any{"status": map[string]any{"name": "Doing\u202e\x1b[2J", "type": "started"}}}
	out, err := statusRun(t, e, "s1", statusProject, "--suggest")
	if err != nil || out["status"].(map[string]any)["name"] != "Doing[2J" {
		t.Fatal(out, err)
	}
	if r := e.sent[0]; r.Operation != "linear_app_project_status" || r.Args["linear_project"] != statusProject || r.Args["suggest"] != true {
		t.Fatal(r)
	}
	for _, args := range [][]string{{statusProject, "--type", "started"}, {statusProject, "--confirm"}, {"FIX-1"}, {}} {
		if _, err = statusRun(t, e, "s1", args...); err == nil {
			t.Fatal("accepted", args)
		}
	}
}

// A preview prints the confirm to run; the confirm needs the status the
// preview saw; --allow-open goes only with it.
func TestLinearProjectStatusSet(t *testing.T) {
	typing(t, false, "")
	e := &statusExecutor{data: map[string]any{"preview": true, "expected_status_id": statusPlanned, "needs_allow_open": true}}
	out, err := statusSetRun(t, e, "s1", statusProject, "--type", "completed", "--operation-id", "op-1")
	if err != nil || out["next"] != "clawbridge linear project status set "+statusProject+" --type completed --operation-id op-1 --confirm --expected-status "+statusPlanned+" --allow-open" {
		t.Fatal(out, err)
	}
	if r := e.sent[0]; r.Operation != "linear_app_project_status_set" || r.Args["confirm"] != nil || r.Args["type"] != "completed" || r.Args["operation_id"] != "op-1" {
		t.Fatal(r)
	}
	e.data = map[string]any{"state": "accepted"}
	if out, err = statusSetRun(t, e, "s1", statusProject, "--type", "completed", "--operation-id", "op-1", "--confirm", "--expected-status", statusPlanned, "--allow-open"); err != nil || out["state"] != "accepted" {
		t.Fatal(out, err)
	}
	if r := e.sent[1]; r.Args["confirm"] != true || r.Args["expected_status_id"] != statusPlanned || r.Args["allow_open"] != true {
		t.Fatal(r)
	}
	sent := len(e.sent)
	for _, args := range [][]string{
		{statusProject, "--type", "done", "--operation-id", "op-1"},
		{statusProject, "--type", "started"},
		{statusProject, "--type", "started", "--operation-id", "op-1", "--confirm"},
		{statusProject, "--type", "started", "--operation-id", "op-1", "--expected-status", statusPlanned},
		{statusProject, "--type", "started", "--operation-id", "op-1", "--allow-open"},
		{statusProject, "--type", "started", "--operation-id", "op-1", "--status-id", "x"},
		{statusProject, "--type", "started", "--operation-id", "op-1", "--suggest"},
	} {
		if _, err = statusSetRun(t, e, "s1", args...); err == nil {
			t.Fatal("accepted", args)
		}
	}
	if len(e.sent) != sent {
		t.Fatal("a refused set was sent")
	}
}

// Without a session a set is the person's own: under an agent's
// environment it is refused before anything is sent. With a session it
// goes, and the server decides.
func TestLinearProjectStatusSetWithoutASessionIsAPersons(t *testing.T) {
	e := &statusExecutor{data: map[string]any{"preview": true}}
	typing(t, false, "", "CLAUDECODE=1")
	if _, err := statusSetRun(t, e, "", statusProject, "--type", "started", "--operation-id", "op-1"); err == nil || !strings.Contains(err.Error(), "CLAUDECODE") || len(e.sent) != 0 {
		t.Fatal(err, e.sent)
	}
	if _, err := statusSetRun(t, e, "coordinator", statusProject, "--type", "started", "--operation-id", "op-1"); err != nil || len(e.sent) != 1 {
		t.Fatal(err)
	}
	if _, err := statusRun(t, e, "", statusProject); err != nil {
		t.Fatal("a read under an agent's environment:", err)
	}
}

// A local authority has no app: the refusal says so.
func TestLinearProjectStatusNeedsAHostedServer(t *testing.T) {
	typing(t, false, "")
	e := &statusExecutor{err: errors.New(`unsupported operation "linear_app_project_status"`)}
	if _, err := statusRun(t, e, "s1", statusProject); err == nil || !strings.Contains(err.Error(), "capability_unavailable") || !strings.Contains(err.Error(), "hosted server") {
		t.Fatal(err)
	}
}

// The dispatch: status takes its own flags and no --profile.
func TestLinearProjectStatusDispatch(t *testing.T) {
	for _, args := range [][]string{{"project", "rename", statusProject}, {"project", "status"}} {
		if err := runLinear(context.Background(), Config{}, args, &bytes.Buffer{}, &bytes.Buffer{}); err == nil {
			t.Fatal("accepted", args)
		}
	}
}

// Every documented form reaches the project-status route (it stops only
// at the missing project, past parsing); a parse name typed as a command
// (linear project-status) is refused with the form to type, never sent on
// as an issue write ("write requires operation_id").
func TestLinearProjectStatusDocumentedForms(t *testing.T) {
	// No config, no profile, no agent marker: nothing reaches a server.
	isolated(t)
	typing(t, false, "")
	routed := "explicit existing ClawBridge workspace/project"
	for _, args := range [][]string{
		{"project", "status", statusProject},
		{"project", "status", statusProject, "--suggest"},
		{"project", "status", "set", statusProject, "--type", "started", "--operation-id", "op-1"},
		{"project", "status", "set", statusProject, "--type", "completed", "--operation-id", "op-1", "--confirm", "--expected-status", statusProject, "--allow-open"},
	} {
		if err := runLinear(t.Context(), Config{}, args, &bytes.Buffer{}, &bytes.Buffer{}); err == nil || !strings.Contains(err.Error(), routed) {
			t.Errorf("%v: %v", args, err)
		}
	}
	for _, args := range [][]string{
		{"project-status", statusProject},
		{"project-status", statusProject, "--suggest", "--operation-id", "op-1"},
		{"project-status", "status", statusProject},
	} {
		err := runLinear(t.Context(), Config{}, args, &bytes.Buffer{}, &bytes.Buffer{})
		if err == nil || !strings.Contains(err.Error(), "isn't a command; use linear project status LINEAR_PROJECT") || strings.Contains(err.Error(), "operation_id") {
			t.Errorf("%v: %v", args, err)
		}
	}
	for name := range linearInternalCommands {
		if err := runLinear(t.Context(), Config{}, []string{name}, &bytes.Buffer{}, &bytes.Buffer{}); err == nil || !strings.Contains(err.Error(), "isn't a command") {
			t.Errorf("%s: %v", name, err)
		}
	}
}
