package server

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

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

// Without the write scope in the token's grant, the app refuses relation
// writes before anything is sent: Linear would answer success and drop the
// edge. With it, the write goes on. A grant that cannot be read refuses the
// write with that error. Other writes never meet this guard.
func TestRelationWritesNeedTheWriteScope(t *testing.T) {
	ctx := context.Background()
	granted := func(ok bool, err error) func(context.Context, string) (bool, error) {
		return func(_ context.Context, scope string) (bool, error) {
			if scope != "write" {
				t.Fatalf("asked for scope %q", scope)
			}
			return ok, err
		}
	}
	for _, operation := range []string{"relation_add", "relation_remove"} {
		err := relationScope(ctx, operation, granted(false, nil))
		if code, status := serviceError(err); code != "invalid" || status != 400 || !strings.HasPrefix(responseMessage(code, err), "capability_unavailable:") {
			t.Fatal(operation, code, status, err)
		}
		if err := relationScope(ctx, operation, granted(true, nil)); err != nil {
			t.Fatal(operation, "refused with the write scope:", err)
		}
		unread := errorString("token store unavailable")
		if err := relationScope(ctx, operation, granted(true, unread)); err != unread {
			t.Fatal(operation, "an unread grant let the write through:", err)
		}
	}
	never := func(context.Context, string) (bool, error) {
		t.Fatal("another write met the relation guard")
		return false, nil
	}
	for _, operation := range []string{"comment", "create", "update"} {
		if err := relationScope(ctx, operation, never); err != nil {
			t.Fatal(operation, err)
		}
	}
}

// Through linear_write, an app that cannot get a token refuses a relation
// write with that error, and nothing reaches Linear.
func TestRelationWriteRefusedWhenTheTokenFails(t *testing.T) {
	app, err := linearapp.New(linearapp.Config{
		ClientID: "client", Secret: "secret", Key: make([]byte, 32),
		Store:        failingTokenStore{},
		HTTP:         &http.Client{Transport: noLinear{t}},
		MCPTransport: noLinear{t},
		Now:          time.Now,
	})
	if err != nil {
		t.Fatal(err)
	}
	w := &AppWriter{App: app, Member: func(context.Context, bus.Identity, string) error { return nil }}
	_, err = w.Execute(context.Background(), bus.Identity{Principal: "a@example.com"}, bus.Request{Operation: "linear_write", Args: map[string]any{"operation": "relation_add", "args": map[string]any{}}})
	if err == nil || !strings.Contains(err.Error(), "token store unavailable") {
		t.Fatal(err)
	}
}

// failingTokenStore cannot read the app's tokens.
type failingTokenStore struct{}

func (failingTokenStore) RecordMintedAppToken(context.Context, string, string, time.Time, time.Time) error {
	return errorString("token store unavailable")
}

func (failingTokenStore) LiveAppToken(context.Context, string, string) (bus.AppToken, bool, error) {
	return bus.AppToken{}, false, errorString("token store unavailable")
}

func (failingTokenStore) PublishAppToken(context.Context, bus.AppToken, string) (bus.AppToken, bool, error) {
	return bus.AppToken{}, false, errorString("token store unavailable")
}

func (failingTokenStore) AppTokensMinted(context.Context, string, time.Time) (int, error) {
	return 0, errorString("token store unavailable")
}

// noLinear fails the test on any request to Linear.
type noLinear struct{ t *testing.T }

func (n noLinear) RoundTrip(r *http.Request) (*http.Response, error) {
	n.t.Fatalf("a request reached Linear: %s %s", r.Method, r.URL)
	return nil, errorString("unreachable")
}

// G9 F1: with no authority to prove membership, the app is never reached:
// the check fails closed.
func TestAppWriterRefusesWithoutAMembershipCheck(t *testing.T) {
	app, err := linearapp.New(linearapp.Config{
		ClientID: "client", Secret: "secret", Key: make([]byte, 32),
		Store:        failingTokenStore{},
		HTTP:         &http.Client{Transport: noLinear{t}},
		MCPTransport: noLinear{t},
		Now:          time.Now,
	})
	if err != nil {
		t.Fatal(err)
	}
	w := &AppWriter{App: app}
	for _, r := range []bus.Request{
		{Operation: "linear_app_status", Args: map[string]any{}},
		{Operation: "linear_write", Args: map[string]any{"operation": "relation_add", "args": map[string]any{}}},
	} {
		out, err := w.Execute(context.Background(), bus.Identity{Principal: "a@example.com"}, r)
		if code, _ := serviceError(err); code != "unavailable" || out.Data != nil {
			t.Fatal(r.Operation, out.Data, err)
		}
	}
}
