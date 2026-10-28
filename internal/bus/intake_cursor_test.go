package bus

import (
	"context"
	"testing"
	"time"
)

// An intake source's next poll counts from its read's start; an unreadable
// start counts from now, never from the zero time.
func TestIntakeCursorNextPoll(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	var pid string
	if err := h.s.db.QueryRow(`SELECT id FROM projects`).Scan(&pid); err != nil {
		t.Fatal(err)
	}
	read := time.Now().UTC().Add(-20 * time.Second)
	for subject, polled := range map[string]string{"a": layout(read), "b": "unreadable"} {
		if err := h.s.IntakeCursorSet(ctx, pid, "intake-s1", subject, polled, false); err != nil {
			t.Fatal(err)
		}
	}
	next := func(subject string) time.Time {
		var raw string
		if err := h.s.db.QueryRow(`SELECT next_poll_at FROM watch_cursors WHERE source='intake-s1' AND subject=?`, subject).Scan(&raw); err != nil {
			t.Fatal(err)
		}
		return at(raw)
	}
	if got := next("a"); !got.Equal(read.Add(15 * time.Minute)) {
		t.Fatal("from the read's start:", got, read)
	}
	if got := next("b"); time.Until(got) < 14*time.Minute {
		t.Fatal("an unreadable start:", got)
	}
}
