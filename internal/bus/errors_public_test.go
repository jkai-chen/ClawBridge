package bus

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
)

type constraintError struct{}

func (constraintError) Error() string {
	return `ERROR: new row for relation "sessions" violates check constraint "sessions_status_check"`
}
func (constraintError) SQLState() string { return "23514" }

// Only messages written for the caller are public; database, transport and
// unknown causes never are, even when they classify as invalid.
func TestOnlyCallerFacingMessagesArePublic(t *testing.T) {
	for name, c := range map[string]struct {
		err    error
		public bool
	}{
		"invalid":                {invalid("unsupported agent_kind"), true},
		"wrapped invalid":        {fmt.Errorf("register: %w", invalid("unsupported agent_kind")), true},
		"scoped refusal":         {scopedError(errNotFound, "target session must exist in the same project", "invalid"), true},
		"classified constraint":  {classify(constraintError{}), false},
		"classified unknown":     {classify(errors.New("sql: Scan error on column index 3")), false},
		"unscoped database fail": {scopedError(constraintError{}, "never shown", "invalid"), false},
	} {
		message, ok := PublicMessage(c.err)
		if ok != c.public || (ok && strings.Contains(message, "constraint")) {
			t.Errorf("%s: public=%v %q", name, ok, message)
		}
	}
}

func TestSafePublicTextIsOnePrintableBoundedLine(t *testing.T) {
	long := strings.Repeat("é", 200) // 400 bytes
	for name, c := range map[string]struct {
		in, want string
		ok       bool
	}{
		"plain":       {"successor must be active", "successor must be active", true},
		"newline":     {"first\nsecond", "", false},
		"control":     {"bell\a", "", false},
		"escape":      {"\x1b[31mred", "", false},
		"invalid utf": {"bad \xff", "", false},
		"empty":       {"", "", false},
		"long":        {long, strings.Repeat("é", 150), true},
	} {
		got, ok := SafePublicText(c.in)
		if ok != c.ok || got != c.want || len(got) > 300 {
			t.Errorf("%s: %q %v", name, got, ok)
		}
	}
}

type stateError string

func (e stateError) Error() string    { return "ERROR: internal detail (SQLSTATE " + string(e) + ")" }
func (e stateError) SQLState() string { return string(e) }

// A data exception is the caller's input, not an outage, and its database
// text stays private.
func TestDataExceptionsAreInvalidAndPrivate(t *testing.T) {
	for state, want := range map[string]string{"22021": "invalid", "22001": "invalid", "22P02": "invalid", "23514": "invalid", "23505": "conflict", "40001": "unavailable", "57014": "unavailable"} {
		err := classify(stateError(state))
		var coded interface{ Code() string }
		if !errors.As(err, &coded) || coded.Code() != want {
			t.Errorf("%s: %v, want %s", state, err, want)
		}
		if _, public := PublicMessage(err); public {
			t.Errorf("%s: database text marked public", state)
		}
	}
}

func TestNULBytesAreRefusedBeforeTheDatabase(t *testing.T) {
	store, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	for name, r := range map[string]Request{
		"argument":    {Operation: "workspace_init", Workspace: "w", Args: map[string]any{"mode": "local", "name": "a\x00b"}},
		"scope":       {Operation: "workspace_init", Workspace: "w\x00", Args: map[string]any{"mode": "local"}},
		"nested":      {Operation: "send", Workspace: "w", Project: "p", Session: "s", Args: map[string]any{"coordination": map[string]any{"k": []any{"x\x00"}}}},
		"string list": {Operation: "mark_read", Workspace: "w", Project: "p", Session: "s", Args: map[string]any{"ids": []string{"m\x00"}}},
	} {
		_, err := store.Execute(context.Background(), r)
		message, public := PublicMessage(err)
		if !public || message != "text arguments must not contain NUL bytes" {
			t.Errorf("%s: %v", name, err)
		}
	}
}
