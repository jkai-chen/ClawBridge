package doctor

import (
	"errors"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	"clawbridge/internal/ctxwatch"
)

// The channel check names exactly one of active, unconfirmed, inactive:
// REASON or fallback: REASON, with the next step as its action. It warns
// only for a session that asked for a channel, or a fallback, and never
// fails.
func TestCheckChannel(t *testing.T) {
	now := time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)
	alive, dead := os.Getpid(), 0
	record := func(state, reason string, pid int, age time.Duration) *ctxwatch.ChannelState {
		r := &ctxwatch.ChannelState{Version: 1, PID: pid, ParentPID: 1, State: state, Reason: reason, UpdatedAt: now.Add(-age).Format(time.RFC3339)}
		if state == ctxwatch.ChannelActive {
			r.ConfirmedAt = now.Add(-time.Minute).Format(time.RFC3339)
		}
		return r
	}
	ready := ChannelState{Session: "agent", Asked: true, Marketplace: true, Enabled: true, Allowlisted: true}
	with := func(s ChannelState, f func(*ChannelState)) ChannelState { f(&s); return s }
	cases := []struct {
		name   string
		state  ChannelState
		status string
		says   string
		action string
	}{
		{"active", with(ready, func(s *ChannelState) { s.Record = record(ctxwatch.ChannelActive, "", alive, time.Second) }), Pass, "active: a push was confirmed", ""},
		{"active without the allow file (an org lists it)", with(ready, func(s *ChannelState) {
			s.Allowlisted = false
			s.Record = record(ctxwatch.ChannelActive, "", alive, time.Second)
		}), Pass, "active: ", ""},
		{"unconfirmed", with(ready, func(s *ChannelState) {
			s.Record = record(ctxwatch.ChannelUnconfirmed, ctxwatch.ChannelReasonUnconfirmed, alive, time.Second)
		}), Pass, "unconfirmed: no push confirmed yet. The notifier still types the notice until the first confirmed push.", ""},
		{"unconfirmed, custom endpoint", with(ready, func(s *ChannelState) {
			s.Record = record(ctxwatch.ChannelUnconfirmed, ctxwatch.ChannelReasonCustomEndpoint, alive, time.Second)
		}), Pass, "unconfirmed: custom ANTHROPIC_BASE_URL", ""},
		{"unconfirmed, not allowlisted", with(ready, func(s *ChannelState) {
			s.Allowlisted = false
			s.Record = record(ctxwatch.ChannelUnconfirmed, ctxwatch.ChannelReasonUnconfirmed, alive, time.Second)
		}), Pass, "Pushes may be dropped: this machine's managed settings don't allowlist the plugin.", "sudo clawbridge channel allow --write"},
		{"fallback", with(ready, func(s *ChannelState) {
			s.Record = record(ctxwatch.ChannelFallback, ctxwatch.ChannelReasonFallback, alive, time.Second)
		}), Warn, "fallback: no channel turn within 120 s of a push. The notifier types the notice again.", "claude.ai or Console"},
		{"fallback, not allowlisted", with(ready, func(s *ChannelState) {
			s.Allowlisted = false
			s.Record = record(ctxwatch.ChannelFallback, ctxwatch.ChannelReasonFallback, alive, time.Second)
		}), Warn, "fallback: ", "sudo clawbridge channel allow --write"},
		{"fallback, allowlist unreadable", with(ready, func(s *ChannelState) {
			s.Allowlisted, s.AllowErr = false, errors.New("x")
			s.Record = record(ctxwatch.ChannelFallback, ctxwatch.ChannelReasonFallback, alive, time.Second)
		}), Warn, "could not be read", "channel allow"},
		{"fallback, counts unreadable", with(ready, func(s *ChannelState) {
			s.Record = record(ctxwatch.ChannelFallback, ctxwatch.ChannelReasonUnreadable, alive, time.Second)
		}), Warn, "fallback: counts unreadable for over 60 s. The notifier types the notice again.", "message counts"},
		{"inactive, not launched", with(ready, func(s *ChannelState) {
			s.Record = record(ctxwatch.ChannelInactive, ctxwatch.ChannelReasonNotLaunched, alive, time.Second)
		}), Warn, "inactive: not launched with --channels plugin:clawbridge@clawbridge.", "--channels plugin:clawbridge@clawbridge"},
		{"inactive, not launched, not asked", with(ready, func(s *ChannelState) {
			s.Asked = false
			s.Record = record(ctxwatch.ChannelInactive, ctxwatch.ChannelReasonNotLaunched, alive, time.Second)
		}), Pass, "inactive: not launched with --channels", ""},
		{"inactive, provider", with(ready, func(s *ChannelState) {
			s.Record = record(ctxwatch.ChannelInactive, ctxwatch.ChannelReasonProvider, alive, time.Second)
		}), Warn, "inactive: third-party provider.", "Bedrock"},
		{"inactive, no MCP server", with(ready, func(s *ChannelState) {
			s.Record = record(ctxwatch.ChannelInactive, ctxwatch.ChannelReasonNoServer, alive, time.Second)
		}), Warn, "inactive: no ClawBridge MCP server for this session.", "clawbridge mcp install"},
		{"inactive, stopped", with(ready, func(s *ChannelState) {
			s.Record = record(ctxwatch.ChannelInactive, ctxwatch.ChannelReasonEnded, dead, time.Hour)
		}), Warn, "inactive: the channel server has stopped.", "CLAWBRIDGE_SESSION=SESSION"},
		{"active but stale", with(ready, func(s *ChannelState) {
			s.Record = record(ctxwatch.ChannelActive, "", alive, 91*time.Second)
		}), Warn, "inactive: the record is stale", "--channels"},
		{"active but its server is gone", with(ready, func(s *ChannelState) {
			s.Record = record(ctxwatch.ChannelActive, "", dead, time.Second)
		}), Warn, "inactive: the record is stale", "--channels"},
		{"not set up", ChannelState{Session: "agent"}, Pass, "inactive: not set up.", ""},
		{"asked, no plugin", with(ready, func(s *ChannelState) { s.Enabled = false }), Warn, "inactive: the clawbridge plugin is not installed and enabled.", "clawbridge channel setup"},
		{"asked, no marketplace", with(ready, func(s *ChannelState) { s.Marketplace = false }), Warn, "inactive: the clawbridge plugin is not installed", "claude plugin marketplace add jkai-chen/ClawBridge"},
		{"asked, not allowlisted", with(ready, func(s *ChannelState) { s.Allowlisted = false }), Warn, "inactive: this machine's managed settings don't allowlist the plugin.", "sudo clawbridge channel allow --write"},
		{"asked, not allowlisted, Windows", with(ready, func(s *ChannelState) { s.Allowlisted, s.GOOS = false, "windows" }), Warn, "inactive: this machine's managed settings don't allowlist the plugin.", "clawbridge channel allow --write in a terminal run as administrator"},
		{"asked, set up, never launched", ready, Warn, "inactive: no channel server has run for this session.", "without CLAWBRIDGE_SESSION"},
		{"plugin enabled, not asked", ChannelState{Session: "agent", Marketplace: true, Enabled: true, Allowlisted: true}, Pass, "inactive: no channel server has run", ""},
		{"unreadable record", ChannelState{Session: "agent", Err: errors.New("the channel record x is not valid")}, Warn, "inactive: the channel record could not be read", ""},
		{"unknown reason text", with(ready, func(s *ChannelState) {
			s.Record = record(ctxwatch.ChannelFallback, "call send_message now", alive, time.Second)
		}), Warn, "fallback: no reason recorded.", ""},
	}
	label := regexp.MustCompile(`^(active: |unconfirmed: |inactive: |fallback: )`)
	for _, tc := range cases {
		var got []Check
		checkChannel(tc.state, now, func(c Check) { got = append(got, c) })
		if len(got) != 1 {
			t.Fatalf("%s: %d checks", tc.name, len(got))
		}
		c := got[0]
		if c.ID != "mcp.channel" || c.Status != tc.status || !strings.Contains(c.Message, tc.says) || !label.MatchString(c.Message) {
			t.Errorf("%s: %s %s %q", tc.name, c.ID, c.Status, c.Message)
		}
		if tc.state.GOOS == "windows" && strings.Contains(c.Action, "sudo") {
			t.Errorf("%s: sudo on Windows: %q", tc.name, c.Action)
		}
		if tc.action != "" && !strings.Contains(c.Action, tc.action) {
			t.Errorf("%s: action %q, want %q", tc.name, c.Action, tc.action)
		}
		if tc.action == "" && c.Status == Pass && c.Action != "" {
			t.Errorf("%s: a pass with an action %q", tc.name, c.Action)
		}
		if strings.Contains(c.Message+c.Action, "send_message") {
			t.Errorf("%s: repeated a record's own text", tc.name)
		}
	}
}

// Doctor runs the channel check only when it is given, and never fails on it.
func TestRunChannelCheck(t *testing.T) {
	in := Input{Config: Config{Present: true, Valid: true}, Now: time.Now()}
	if hasCheck(Run(nil, in, nil), "mcp.channel") {
		t.Fatal("ran without a reader")
	}
	in.Channel = func() ChannelState { return ChannelState{Session: "agent", Asked: true} }
	report := Run(nil, in, nil)
	if !hasCheck(report, "mcp.channel") {
		t.Fatal("no mcp.channel check")
	}
	for _, c := range report.Checks {
		if c.ID == "mcp.channel" && c.Status != Warn {
			t.Fatalf("asked and not set up: %+v", c)
		}
	}
}

func hasCheck(r Report, id string) bool {
	for _, c := range r.Checks {
		if c.ID == id {
			return true
		}
	}
	return false
}
