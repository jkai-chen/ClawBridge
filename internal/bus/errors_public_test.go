package bus

import (
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
