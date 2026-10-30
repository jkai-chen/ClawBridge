package doctor

import (
	"time"

	"clawbridge/internal/ctxwatch"
)

// ChannelState is what the client read locally about this session's
// channel wake (FR26): the channel server's record, whether the session
// asked for a channel, and the plugin's and the machine's setup. It holds
// no prompt and no message.
type ChannelState struct {
	Session string
	// Path is the channel record's path.
	Path   string
	Record *ctxwatch.ChannelState
	Err    error
	// Asked says ClawBridge's turn hook (mcp install --channel) is
	// installed in a settings file this project reads.
	Asked bool
	// Marketplace and Enabled are the plugin's state in Claude Code's
	// settings, read only.
	Marketplace, Enabled bool
	// Allowlisted says this machine's managed-settings files list the
	// plugin as a channel with channels enabled; AllowErr that they could
	// not be read. MDM and claude.ai organisation settings aren't seen.
	Allowlisted bool
	AllowErr    error
	// GOOS words the admin step: sudo, or on Windows a terminal run as
	// administrator.
	GOOS string
}

// The channel's next steps, as doctor's actions name them.
const (
	channelInstallAction = "Install the plugin in a plain terminal: clawbridge channel setup (or claude plugin marketplace add jkai-chen/ClawBridge, then claude plugin install clawbridge@clawbridge --scope user)."
	channelLaunchAction  = "Relaunch the agent with its session exported: CLAWBRIDGE_SESSION=SESSION CLAWBRIDGE_INSTANCE=INSTANCE claude --channels plugin:clawbridge@clawbridge"
)

// channelAllowAction is the admin step's action on goos.
func channelAllowAction(goos string) string {
	run := "sudo clawbridge channel allow --write"
	if goos == "windows" {
		run = "clawbridge channel allow --write in a terminal run as administrator"
	}
	return "Allowlist the plugin on this machine, once, as an administrator: " + run + ", typing yes when it asks (on claude.ai Team or Enterprise, or under MDM, an admin lists it there instead)."
}

// channelReasons are the records' reasons doctor repeats; any other text
// in a record is not shown.
var channelReasons = map[string]bool{
	ctxwatch.ChannelReasonUnconfirmed: true, ctxwatch.ChannelReasonCustomEndpoint: true,
	ctxwatch.ChannelReasonNoArgv: true, ctxwatch.ChannelReasonNotLaunched: true,
	ctxwatch.ChannelReasonProvider: true, ctxwatch.ChannelReasonNoServer: true,
	ctxwatch.ChannelReasonFallback: true, ctxwatch.ChannelReasonEnded: true,
	ctxwatch.ChannelReasonUnreadable: true,
}

// checkChannel reports the session's channel wake as exactly one of
// active, unconfirmed, inactive: REASON or fallback: REASON. It warns, and
// never fails; a session that never asked for a channel is a plain pass.
func checkChannel(s ChannelState, now time.Time, add func(Check)) {
	c := Check{ID: "mcp.channel", Title: "Channel wake (Claude Code)", Status: Pass, Path: s.Path}
	inactive := func(reason, action string) {
		c.Message = "inactive: " + reason + "."
		if s.Asked {
			c.Status = Warn
			c.Message += " The session asked for a channel (its turn hook is installed); the notifier keeps typing the notice."
			c.Action = action
		}
	}
	// setup is the first setup step still missing, if any.
	setup := func() (string, string) {
		switch {
		case !s.Marketplace || !s.Enabled:
			return "the clawbridge plugin is not installed and enabled", channelInstallAction
		case s.AllowErr != nil:
			return "this machine's managed settings could not be read", channelAllowAction(s.GOOS)
		case !s.Allowlisted:
			return "this machine's managed settings don't allowlist the plugin", channelAllowAction(s.GOOS)
		}
		return "", ""
	}
	r := s.Record
	switch {
	case s.Err != nil:
		c.Status = Warn
		c.Message = "inactive: the channel record could not be read (" + s.Err.Error() + ")."
	case r == nil && !s.Asked && !s.Enabled:
		c.Message = "inactive: not set up. The notifier wakes this session; see docs/notifications.md to wake it through a channel."
	case r == nil:
		reason, action := setup()
		if reason == "" {
			reason, action = "no channel server has run for this session", channelLaunchAction+" (without CLAWBRIDGE_SESSION the plugin's server has no session and writes nothing)"
		}
		inactive(reason, action)
	case r.State != ctxwatch.ChannelInactive && !channelRecordLive(*r, now):
		inactive("the record is stale: no channel server runs for it now", channelLaunchAction)
	case r.State == ctxwatch.ChannelActive:
		c.Message = "active: a push was confirmed by the session's own channel turn"
		if r.ConfirmedAt != "" {
			c.Message += " at " + r.ConfirmedAt
		}
		c.Message += ". The notifier stands down while it stays so."
	case r.State == ctxwatch.ChannelUnconfirmed:
		c.Message = "unconfirmed: " + channelReason(r.Reason) + ". The notifier still types the notice until the first confirmed push."
		if reason, action := setup(); reason != "" {
			c.Message += " Pushes may be dropped: " + reason + "."
			c.Action = action
		}
	case r.State == ctxwatch.ChannelFallback:
		c.Status = Warn
		c.Message = "fallback: " + channelReason(r.Reason) + ". The notifier types the notice again."
		if reason, action := setup(); reason != "" {
			c.Message += " Pushes may be dropped: " + reason + "."
			c.Action = action
		} else if r.Reason == ctxwatch.ChannelReasonUnreadable {
			c.Action = "The channel server can't read this session's message counts from its database or server; check that the session's ClawBridge MCP server and its route work (clawbridge --session NAME doctor's other checks). Once counts are read again, a confirmed push makes the channel active again."
		} else {
			c.Action = "Check that Claude Code is signed in with claude.ai or Console (not Bedrock, Vertex, Foundry or a gateway) and that an organisation's settings allow channels; the notifier keeps waking the session meanwhile."
		}
	default:
		reason := channelReason(r.Reason)
		action := channelLaunchAction
		switch r.Reason {
		case ctxwatch.ChannelReasonProvider:
			action = "Channels need a claude.ai or Console login: Bedrock, Vertex and Foundry drop them, so this session stays on the notifier."
		case ctxwatch.ChannelReasonNoServer:
			action = "Start the agent with its ClawBridge MCP entry (clawbridge mcp install) and the session's CLAWBRIDGE_SESSION and CLAWBRIDGE_INSTANCE exported."
		}
		inactive(reason, action)
	}
	add(c)
}

// channelRecordLive says whether the record's channel server still runs:
// the record is fresh and its process alive.
func channelRecordLive(r ctxwatch.ChannelState, now time.Time) bool {
	at, err := time.Parse(time.RFC3339, r.UpdatedAt)
	if err != nil || now.Sub(at) > ctxwatch.ChannelStateFresh || at.Sub(now) > time.Minute {
		return false
	}
	return ctxwatch.ProcessAlive(r.PID)
}

// channelReason is a record's reason when it is one the channel server
// writes, else a neutral word: doctor repeats no other text.
func channelReason(reason string) string {
	if channelReasons[reason] {
		return reason
	}
	return "no reason recorded"
}
