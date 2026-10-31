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

// IntakeCursorSoon brings a far next poll to two minutes from now and never
// pushes a nearer one out; the next IntakeCursorSet sets the usual wait.
func TestIntakeCursorSoon(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	var pid string
	if err := h.s.db.QueryRow(`SELECT id FROM projects`).Scan(&pid); err != nil {
		t.Fatal(err)
	}
	polled := layout(time.Now().UTC())
	next := func() time.Duration {
		var raw string
		if err := h.s.db.QueryRow(`SELECT next_poll_at FROM watch_cursors WHERE source='intake-s1' AND subject='a'`).Scan(&raw); err != nil {
			t.Fatal(err)
		}
		return time.Until(at(raw))
	}
	if err := h.s.IntakeCursorSet(ctx, pid, "intake-s1", "a", polled, false); err != nil {
		t.Fatal(err)
	}
	if err := h.s.IntakeCursorSoon(ctx, pid, "intake-s1", "a"); err != nil {
		t.Fatal(err)
	}
	if d := next(); d > 2*time.Minute+5*time.Second || d < time.Minute {
		t.Fatal("not two minutes:", d)
	}
	if _, err := h.s.db.Exec(`UPDATE watch_cursors SET next_poll_at=?`, layout(time.Now().UTC().Add(20*time.Second))); err != nil {
		t.Fatal(err)
	}
	if err := h.s.IntakeCursorSoon(ctx, pid, "intake-s1", "a"); err != nil {
		t.Fatal(err)
	}
	if d := next(); d > 25*time.Second {
		t.Fatal("a nearer poll was pushed out:", d)
	}
	if err := h.s.IntakeCursorSet(ctx, pid, "intake-s1", "a", layout(time.Now().UTC()), false); err != nil {
		t.Fatal(err)
	}
	if d := next(); d < 14*time.Minute {
		t.Fatal("the usual wait did not return:", d)
	}
	if h.s.IntakeCursorSoon(ctx, pid, "comments", "a") == nil {
		t.Fatal("a non-intake source was accepted")
	}
}

// IntakeDueAt lists, per project, the earliest active intake source due
// after now and inside the window; it skips other sources, closed cursors,
// sources already due and sources past the window.
func TestIntakeDueAt(t *testing.T) {
	h := newHarness(t)
	var pid string
	if err := h.s.db.QueryRow(`SELECT id FROM projects`).Scan(&pid); err != nil {
		t.Fatal(err)
	}
	checkIntakeDueAt(t, h.s, pid)
}

// checkIntakeDueAt is TestIntakeDueAt's body, shared with the Postgres run.
func checkIntakeDueAt(t *testing.T, s *Store, pid string) {
	t.Helper()
	ctx := context.Background()
	now := time.Now().UTC()
	put := func(source, subject string, due time.Time, closed bool) {
		t.Helper()
		until := any(nil)
		if closed {
			until = layout(now.Add(time.Hour))
		}
		tx, err := s.beginIntake(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback()
		if _, err := tx.ExecContext(ctx, `INSERT INTO watch_cursors(project_id,source,subject,cursor_time,watch_started_at,active_until,next_poll_at,updated_at) VALUES(?,?,?,?,?,?,?,?)`, pid, source, subject, layout(now), layout(now), until, layout(due), layout(now)); err != nil {
			t.Fatal(err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatal(err)
		}
	}
	put("intake-s1", "late", now.Add(8*time.Minute), false)
	put("intake-s1", "early", now.Add(3*time.Minute), false)
	put("intake-s2", "same-project", now.Add(5*time.Minute), false)
	put("intake-s1", "closed", now.Add(time.Minute), true)
	put("intake-s1", "overdue", now.Add(-time.Minute), false)
	put("intake-s1", "far", now.Add(20*time.Minute), false)
	put("comments", "issue", now.Add(time.Minute), false)
	got, err := s.IntakeDueAt(ctx, 15*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || layout(got[pid]) != layout(now.Add(3*time.Minute)) {
		t.Fatal("not the earliest of the project:", got, now.Add(3*time.Minute))
	}
	if got, err = s.IntakeDueAt(ctx, time.Minute); err != nil || len(got) != 0 {
		t.Fatal("a source past the window:", got, err)
	}
}
