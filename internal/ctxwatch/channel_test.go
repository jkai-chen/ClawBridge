package ctxwatch

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"
	"time"
)

// The channel record round-trips as a private file and names its session's
// state only.
func TestChannelStateRoundTrip(t *testing.T) {
	dir := t.TempDir()
	if _, ok, err := LoadChannelState(dir, "agent"); ok || err != nil {
		t.Fatal("a missing record", ok, err)
	}
	st := ChannelState{PID: 10, ParentPID: 9, State: ChannelUnconfirmed, Reason: "waiting for a confirmed push", LastPushSeq: 4, LastPushAt: "2026-10-30T10:00:00Z", UpdatedAt: "2026-10-30T10:00:01Z"}
	if err := SaveChannelState(dir, "agent", st); err != nil {
		t.Fatal(err)
	}
	got, ok, err := LoadChannelState(dir, "agent")
	st.Version = 1
	if err != nil || !ok || got != st {
		t.Fatal(got, ok, err)
	}
	path := filepath.Join(dir, "agent.channel.json")
	if info, _ := os.Stat(path); runtime.GOOS != "windows" && info.Mode().Perm() != 0o600 {
		t.Fatal(info.Mode())
	}
	data, _ := os.ReadFile(path)
	for _, key := range []string{`"version"`, `"pid"`, `"parent_pid"`, `"state"`, `"reason"`, `"last_push_seq"`, `"last_push_at"`, `"updated_at"`} {
		if !strings.Contains(string(data), key) {
			t.Fatal("missing", key, string(data))
		}
	}
	for _, bad := range []string{`{"version":1,"state":"asleep","updated_at":"2026-10-30T10:00:00Z"}`, `{"version":2,"state":"active"}`, `not json`} {
		if err := os.WriteFile(path, []byte(bad), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, ok, err := LoadChannelState(dir, "agent"); ok || err == nil {
			t.Fatal("an invalid record was read:", bad)
		}
	}
	if err := SaveChannelState(dir, "../x", st); err == nil {
		t.Fatal("a path was taken for a session")
	}
}

// The notify adapter stands down only for a channel proven to deliver,
// whose server is alive and has written lately.
func TestChannelSkipsTyping(t *testing.T) {
	now := time.Date(2026, 10, 30, 10, 0, 0, 0, time.UTC)
	at := func(d time.Duration) string { return now.Add(d).Format(time.RFC3339) }
	alive := os.Getpid()
	good := ChannelState{Version: 1, PID: alive, State: ChannelActive, ConfirmedAt: at(-time.Hour), UpdatedAt: at(-30 * time.Second)}
	if !ChannelSkipsTyping(good, now) {
		t.Fatal("a fresh, live, active channel doesn't skip")
	}
	edge := good
	edge.UpdatedAt = at(-90 * time.Second)
	if !ChannelSkipsTyping(edge, now) {
		t.Fatal("90 s old is still fresh")
	}
	for name, mutate := range map[string]func(*ChannelState){
		"stale":           func(s *ChannelState) { s.UpdatedAt = at(-91 * time.Second) },
		"from the future": func(s *ChannelState) { s.UpdatedAt = at(2 * time.Minute) },
		"no time":         func(s *ChannelState) { s.UpdatedAt = "" },
		"dead pid":        func(s *ChannelState) { s.PID = deadPID(t) },
		"no pid":          func(s *ChannelState) { s.PID = 0 },
		"unconfirmed":     func(s *ChannelState) { s.State = ChannelUnconfirmed },
		"fallback":        func(s *ChannelState) { s.State = ChannelFallback },
		"inactive":        func(s *ChannelState) { s.State = ChannelInactive },
		"never confirmed": func(s *ChannelState) { s.ConfirmedAt = "" },
	} {
		s := good
		mutate(&s)
		if ChannelSkipsTyping(s, now) {
			t.Fatal(name, "skips typing")
		}
	}
}

// deadPID is a process that has exited.
func deadPID(t *testing.T) int {
	t.Helper()
	for pid := 4_000_000; pid > 3_000_000; pid -= 7919 {
		if !ProcessAlive(pid) {
			return pid
		}
	}
	t.Skip("no free process id found")
	return 0
}

// A turn runs from its start until its end: no file, or an end at or after
// the start, is idle. A turn with no end for staleTurn is idle too: Esc
// skips the Stop hook.
func TestTurnIdle(t *testing.T) {
	t0 := "2026-10-30T10:00:00.5Z"
	t1 := "2026-10-30T10:00:01Z"
	start, _ := time.Parse(time.RFC3339Nano, t1)
	now := start.Add(time.Minute)
	for name, c := range map[string]struct {
		rec  TurnRecord
		ok   bool
		now  time.Time
		idle bool
	}{
		"no file":             {TurnRecord{}, false, now, true},
		"running":             {TurnRecord{StartedAt: t1, EndedAt: t0}, true, now, false},
		"never ended":         {TurnRecord{StartedAt: t0}, true, now, false},
		"ended":               {TurnRecord{StartedAt: t0, EndedAt: t1}, true, now, true},
		"same instant":        {TurnRecord{StartedAt: t0, EndedAt: t0}, true, now, true},
		"only an end":         {TurnRecord{EndedAt: t0}, true, now, true},
		"unreadable end":      {TurnRecord{StartedAt: t0, EndedAt: "soon"}, true, now, false},
		"running 10 minutes":  {TurnRecord{StartedAt: t1, EndedAt: t0}, true, start.Add(10 * time.Minute), false},
		"stale, interrupted":  {TurnRecord{StartedAt: t1, EndedAt: t0}, true, start.Add(10*time.Minute + time.Second), true},
		"stale, never ended":  {TurnRecord{StartedAt: t1}, true, start.Add(time.Hour), true},
		"start in the future": {TurnRecord{StartedAt: t1}, true, start.Add(-time.Hour), false},
	} {
		if got := Idle(c.rec, c.ok, c.now); got != c.idle {
			t.Fatal(name, got)
		}
	}
}

// The turn hooks mark a turn's start and end; each keeps the other's mark.
func TestTurnMarks(t *testing.T) {
	dir := t.TempDir()
	if _, ok := LoadTurn(dir, "agent"); ok {
		t.Fatal("a missing turn file was read")
	}
	start := time.Date(2026, 10, 30, 10, 0, 0, 100, time.UTC)
	if err := MarkTurnStarted(dir, "agent", start); err != nil {
		t.Fatal(err)
	}
	rec, ok := LoadTurn(dir, "agent")
	if !ok || Idle(rec, ok, start) || rec.Version != 1 {
		t.Fatal("a started turn is idle", rec, ok)
	}
	// The end comes within the same second: nanoseconds are kept.
	if err := MarkTurnEnded(dir, "agent", start.Add(time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	rec, ok = LoadTurn(dir, "agent")
	if !ok || !Idle(rec, ok, start) || rec.StartedAt == "" {
		t.Fatal("an ended turn isn't idle", rec, ok)
	}
	if err := MarkTurnStarted(dir, "agent", start.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if rec, ok = LoadTurn(dir, "agent"); Idle(rec, ok, start.Add(time.Second)) || rec.EndedAt == "" {
		t.Fatal("a new turn is idle", rec)
	}
	if info, _ := os.Stat(filepath.Join(dir, "agent.turn.json")); runtime.GOOS != "windows" && info.Mode().Perm() != 0o600 {
		t.Fatal(info.Mode())
	}
}

// The queue record keeps main's fields exactly, so an older binary's
// strict decoder still reads a record a newer MCP server wrote.
func TestQueueRecordFieldsUnchanged(t *testing.T) {
	data, err := json.Marshal(QueueRecord{Server: "s", DB: "d", Leases: []QueueLease{}})
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		t.Fatal(err)
	}
	var keys []string
	for k := range fields {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	if got := strings.Join(keys, ","); got != "agent_pid,db,instance,leases,mcp_pid,project,server,session,stream,updated_at,version,workspace" {
		t.Fatal(got)
	}
}

// A channel turn is known only from our server's fixed tag, and only its
// integer seq is taken from the prompt.
func TestChannelTurnSeq(t *testing.T) {
	good := map[string]int64{
		`<channel source="plugin:clawbridge:wake" kind="wake" count="2" seq="17">2 ClawBridge messages are waiting; call inbox</channel>`: 17,
		"\n\t  <channel source=\"plugin:clawbridge:wake\" kind=\"wake\" count=\"1\" seq=\"1\">x</channel>":                                1,
		`<channel source="plugin:clawbridge:wake" seq="9007199254740991" count="1" kind="wake">`:                                          9007199254740991,
		`<channel source="plugin:clawbridge:wake"  kind="wake"  count="3"  seq="44" >`:                                                    44,
	}
	for prompt, want := range good {
		if got, ok := ChannelTurnSeq(prompt); !ok || got != want {
			t.Fatalf("%q: %d %v", prompt, got, ok)
		}
	}
	bad := []string{
		``,
		`hello`,
		`please read <channel source="plugin:clawbridge:wake" kind="wake" count="1" seq="5">`,
		`x<channel source="plugin:clawbridge:wake" kind="wake" count="1" seq="5">`,
		`<channel source="plugin:clawbridge:wake2" kind="wake" count="1" seq="5">`,
		`<channel source="plugin:clawbridge:wake " kind="wake" count="1" seq="5">`,
		`<channel source="plugin:evil:wake" kind="wake" count="1" seq="5">`,
		`<channel source="server:clawbridge" kind="wake" count="1" seq="5">`,
		`<channel  source="plugin:clawbridge:wake" kind="wake" count="1" seq="5">`,
		`<CHANNEL source="plugin:clawbridge:wake" kind="wake" count="1" seq="5">`,
		`<channel source='plugin:clawbridge:wake' kind="wake" count="1" seq="5">`,
		`<channel source="plugin:clawbridge:wake" kind="wake" count="1" seq="5" from="peer">`,
		`<channel source="plugin:clawbridge:wake" kind="wake" count="1" seq="5" seq="6">`,
		`<channel source="plugin:clawbridge:wake" kind="wake" count="1">`,
		`<channel source="plugin:clawbridge:wake" kind="wake" seq="5">`,
		`<channel source="plugin:clawbridge:wake" count="1" seq="5">`,
		`<channel source="plugin:clawbridge:wake" kind="mail" count="1" seq="5">`,
		`<channel source="plugin:clawbridge:wake" kind="wake" count="one" seq="5">`,
		`<channel source="plugin:clawbridge:wake" kind="wake" count="1" seq="5x">`,
		`<channel source="plugin:clawbridge:wake" kind="wake" count="1" seq="-5">`,
		`<channel source="plugin:clawbridge:wake" kind="wake" count="1" seq="0x5">`,
		`<channel source="plugin:clawbridge:wake" kind="wake" count="1" seq=" 5">`,
		`<channel source="plugin:clawbridge:wake" kind="wake" count="1" seq="">`,
		`<channel source="plugin:clawbridge:wake" kind="wake" count="1" seq="0">`,
		`<channel source="plugin:clawbridge:wake" kind="wake" count="1" seq="9007199254740992">`,
		`<channel source="plugin:clawbridge:wake" kind="wake" count="1" seq="99999999999999999999999999">`,
		`<channel source="plugin:clawbridge:wake" kind="wake" count="1" seq="` + strings.Repeat("9", 5000) + `">`,
		`<channel source="plugin:clawbridge:wake" kind="wake" count="1" seq="5"`,
		`<channel source="plugin:clawbridge:wake" kind="wake" count="1" seq="5"/>`,
		`<channel source="plugin:clawbridge:wake" kind="wake" count="1" seq="5"x>`,
		`<channel source="plugin:clawbridge:wake" kind="wake" count="1" seq="５">`,
		`<channel source="plugin:clawbridge:wake"` + strings.Repeat(` kind="wake"`, 1000) + `>`,
		strings.Repeat(" ", 10000) + `<channel source="plugin:clawbridge:wake" kind="wake" count="1" seq="5">`,
		strings.Repeat(" ", 100) + `<channel source="plugin:clawbridge:wake" kind="wake" count="1" seq="5">`,
		`<channel source="plugin:clawbridge:wakX" kind="wake" count="1" seq="5"><channel source="plugin:clawbridge:wake" kind="wake" count="1" seq="6">`,
		`<channel source="plugin:clawbridge:wake" kind="wake" count="12345678901234567" seq="5">`,
	}
	for _, prompt := range bad {
		if got, ok := ChannelTurnSeq(prompt); ok || got != 0 {
			t.Fatalf("%.120q was taken: %d", prompt, got)
		}
	}
}

// A channel turn's mark keeps only the seq and the time, and starts the
// turn; a later ordinary turn keeps the evidence.
func TestMarkChannelTurn(t *testing.T) {
	dir := t.TempDir()
	at := time.Date(2026, 10, 30, 10, 0, 0, 5, time.UTC)
	if err := MarkChannelTurn(dir, "agent", 17, at); err != nil {
		t.Fatal(err)
	}
	rec, ok := LoadTurn(dir, "agent")
	if !ok || rec.ChannelSeq != 17 || rec.ChannelAt != at.Format(time.RFC3339Nano) || rec.StartedAt != rec.ChannelAt || Idle(rec, ok, at) {
		t.Fatal(rec)
	}
	if err := MarkTurnStarted(dir, "agent", at.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if rec, _ = LoadTurn(dir, "agent"); rec.ChannelSeq != 17 {
		t.Fatal("an ordinary turn dropped the channel mark", rec)
	}
	data, _ := os.ReadFile(filepath.Join(dir, "agent.turn.json"))
	var fields map[string]any
	_ = json.Unmarshal(data, &fields)
	if _, ended := fields["ended_at"]; len(fields) != 4 || ended {
		t.Fatal(string(data))
	}
	if err := MarkChannelTurn(dir, "agent", 0, at); err == nil {
		t.Fatal("a zero seq was marked")
	}
}
