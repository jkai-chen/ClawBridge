package server

import (
	"context"
	"strings"
	"testing"

	"clawbridge/internal/bus"
	"clawbridge/internal/linearapp"
	"clawbridge/internal/linearboard"
)

// linear_write refuses, before any Linear or bus access, every operation
// but the agent writes, any argument outside instance/operation/args, and
// any argument that would choose an identity.
func TestLinearWriteRefusesBeforeAnyAccess(t *testing.T) {
	w := &AppWriter{App: &linearapp.App{}}
	id := bus.Identity{Principal: "a@example.com"}
	for name, args := range map[string]map[string]any{
		"link":         {"operation": "link", "args": map[string]any{}},
		"unlink":       {"operation": "unlink", "args": map[string]any{}},
		"repair":       {"operation": "repair", "args": map[string]any{}},
		"label admin":  {"operation": "label_create", "args": map[string]any{}},
		"a read":       {"operation": "issue", "args": map[string]any{}},
		"extra key":    {"operation": "comment", "args": map[string]any{}, "profile": "p"},
		"no args":      {"operation": "comment"},
		"profile arg":  {"operation": "comment", "args": map[string]any{"profile": "personal"}},
		"token arg":    {"operation": "comment", "args": map[string]any{"token": "x"}},
		"identity arg": {"operation": "update", "args": map[string]any{"write_identity": "personal"}},
		"as arg":       {"operation": "relation_add", "args": map[string]any{"as": "me"}},
	} {
		_, err := w.Execute(context.Background(), id, bus.Request{Operation: "linear_write", Args: args})
		if err == nil {
			t.Fatal(name, "accepted")
		}
		if code, status := serviceError(err); code != "invalid" || status != 400 {
			t.Fatal(name, code, status, err)
		}
	}
	var none *AppWriter
	if _, err := none.Execute(context.Background(), id, bus.Request{Operation: "linear_write"}); err == nil || !strings.Contains(err.Error(), "app_unavailable") {
		t.Fatal("no app configured", err)
	}
}

// The board's own refusal text reaches the caller; an uncertain outcome is
// a conflict.
func TestLinearWriteErrorsKeepTheBoardMessage(t *testing.T) {
	refused := writeError(errorString("workspace_unpinned: this binding records no Linear workspace"))
	if code, _ := serviceError(refused); code != "invalid" || responseMessage(code, refused) != "workspace_unpinned: this binding records no Linear workspace" {
		t.Fatal(responseMessage(code, refused))
	}
	uncertain := writeError(linearboard.UncertainError{Message: "Linear operation may have been dispatched; outcome uncertain (timeout), inspect Linear before retrying"})
	if code, status := serviceError(uncertain); code != "conflict" || status != 409 || !strings.Contains(responseMessage(code, uncertain), "outcome uncertain") {
		t.Fatal(code, status)
	}
	busy := writeError(linearboard.InProgressError{Message: "in_progress: retry the same operation_id shortly"})
	if code, status := serviceError(busy); code != "in_progress" || status != 409 || !strings.HasPrefix(responseMessage(code, busy), "in_progress") {
		t.Fatal(code, status)
	}
	// The text alone decides nothing: an untyped error saying "uncertain" is a refusal.
	if code, _ := serviceError(writeError(errorString("outcome uncertain"))); code != "invalid" {
		t.Fatal(code)
	}
	if m := responseMessage("invalid", writeError(errorString("line\\nbreak\n"))); m != "the Linear write was refused" {
		t.Fatal("unsafe text shown", m)
	}
}

type errorString string

func (e errorString) Error() string { return string(e) }
