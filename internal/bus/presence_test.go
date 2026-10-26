package bus

import (
	"context"
	"strings"
	"testing"
	"time"
)

// Derived presence (§1): a live lease is working or idle, an expired one
// asleep or gone; an old client with no lease keeps today's active/stale.
func TestLiveness(t *testing.T) {
	now := time.Date(2026, 1, 2, 12, 0, 0, 0, time.UTC)
	at := func(d time.Duration) string { return now.Add(d).Format(timestampLayout) }
	for name, c := range map[string]struct {
		status, seen, until, state, want string
	}{
		"live lease, recent call":          {"active", at(-time.Minute), at(time.Minute), "", "working"},
		"live lease, quiet, says working":  {"active", at(-time.Hour), at(time.Minute), "working", "working"},
		"live lease, quiet, says idle":     {"active", at(-time.Hour), at(time.Minute), "idle", "idle"},
		"live lease, quiet, says nothing":  {"active", at(-time.Hour), at(time.Minute), "", "idle"},
		"expired lease, says working":      {"active", at(-time.Minute), at(-time.Second), "working", "asleep"},
		"expired lease, quiet":             {"active", at(-time.Hour), at(-time.Hour), "", "asleep"},
		"expired lease, nothing for a day": {"active", at(-25 * time.Hour), at(-25 * time.Hour), "", "gone"},
		"old client, recent call":          {"active", at(-time.Minute), "", "", "working"},
		"old client, quiet":                {"active", at(-time.Hour), "", "", "asleep"},
		"old client, nothing for a day":    {"active", at(-25 * time.Hour), "", "", "asleep"},
		"retired, live lease":              {"retired", at(-time.Minute), at(time.Minute), "working", "gone"},
		"moved":                            {"moved", at(-time.Minute), "", "", "gone"},
	} {
		m := map[string]any{"status": c.status, "last_seen": c.seen, "alive_state": c.state}
		if c.until != "" {
			m["alive_until"] = c.until
		}
		if got := liveness(m, now, 24); got != c.want {
			t.Errorf("%s: %s, want %s", name, got, c.want)
		}
	}
}

// heartbeat renews only the holding instance's lease, at most once per
// 20 s, and never touches last_seen; a retired session holds no lease.
func TestHeartbeatLease(t *testing.T) {
	h := newHarness(t)
	row := func() map[string]any {
		t.Helper()
		r, err := h.s.Execute(context.Background(), h.request("whoami", "a", nil))
		if err != nil {
			t.Fatal(err)
		}
		var until, state any
		if err = h.s.db.QueryRow(`SELECT alive_until,alive_state FROM sessions WHERE name='a'`).Scan(&until, &state); err != nil {
			t.Fatal(err)
		}
		session := r.Data.(map[string]any)["session"].(map[string]any)
		return map[string]any{"until": until, "state": state, "last_seen": session["last_seen"], "liveness": session["liveness"]}
	}
	before := row()
	if before["until"] != nil || before["liveness"] != "working" {
		t.Fatalf("before any heartbeat: %v", before)
	}
	var seen string
	if err := h.s.db.QueryRow(`SELECT last_seen FROM sessions WHERE name='a'`).Scan(&seen); err != nil {
		t.Fatal(err)
	}
	out := h.call("heartbeat", "a", map[string]any{"state": "working"}).(map[string]any)
	if out["renewed"] != true || out["lease_seconds"] != 90 {
		t.Fatal(out)
	}
	var after string
	if err := h.s.db.QueryRow(`SELECT last_seen FROM sessions WHERE name='a'`).Scan(&after); err != nil || after != seen {
		t.Fatalf("heartbeat moved last_seen from %s to %s (%v)", seen, after, err)
	}
	got := row()
	if got["state"] != "working" || got["until"] == nil {
		t.Fatalf("lease not written: %v", got)
	}
	// Within 20 s of the last write, nothing is written again.
	if out := h.call("heartbeat", "a", map[string]any{"state": "idle"}).(map[string]any); out["renewed"] != false {
		t.Fatal("renewed twice within 20 s:", out)
	}
	if row()["state"] != "working" {
		t.Fatal("a skipped heartbeat changed the state")
	}
	// Past it, the lease moves on.
	if _, err := h.s.db.Exec(`UPDATE sessions SET alive_until=? WHERE name='a'`, time.Now().UTC().Add(60*time.Second).Format(timestampLayout)); err != nil {
		t.Fatal(err)
	}
	if out := h.call("heartbeat", "a", map[string]any{"state": "idle"}).(map[string]any); out["renewed"] != true {
		t.Fatal("not renewed with 60 s left:", out)
	}
	if row()["state"] != "idle" {
		t.Fatal("state not renewed")
	}
	// A heartbeat with no state clears the claim.
	if _, err := h.s.db.Exec(`UPDATE sessions SET alive_until=NULL WHERE name='a'`); err != nil {
		t.Fatal(err)
	}
	h.call("heartbeat", "a", nil)
	if row()["state"] != nil {
		t.Fatal("state kept without a claim")
	}
	for name, a := range map[string]map[string]any{
		"another instance": {"instance": "other-instance"},
		"a bad state":      {"state": "busy"},
	} {
		_, err := h.s.Execute(context.Background(), h.request("heartbeat", "a", a))
		if err == nil {
			t.Fatal(name, "accepted")
		}
	}
	if _, err := h.s.Execute(context.Background(), Request{Operation: "heartbeat", Workspace: "w", Project: "p", Args: map[string]any{"instance": "a-instance"}}); err == nil {
		t.Fatal("a heartbeat without a session was accepted")
	}
	if _, err := h.s.db.Exec(`UPDATE sessions SET status='retired' WHERE name='b'`); err != nil {
		t.Fatal(err)
	}
	if _, err := h.s.Execute(context.Background(), h.request("heartbeat", "b", nil)); err == nil || !strings.Contains(err.Error(), "holds no lease") {
		t.Fatal("a retired session renewed:", err)
	}
}
