package ctxwatch

import (
	"bytes"
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// The channel server's states (FR26). Active means a push was confirmed: the
// agent called a tool after it. Unconfirmed is a channel that may work but
// hasn't shown it yet; fallback one whose push went unanswered; inactive
// one that can't work here, for the record's reason.
const (
	ChannelActive      = "active"
	ChannelUnconfirmed = "unconfirmed"
	ChannelInactive    = "inactive"
	ChannelFallback    = "fallback"
)

// ChannelState is what `clawbridge channel serve` tells the notify adapters
// and doctor about its channel, at <context dir>/<session>.channel.json. It
// holds counts, times and process IDs, never a message.
type ChannelState struct {
	Version   int    `json:"version"`
	PID       int    `json:"pid"`
	ParentPID int    `json:"parent_pid"`
	State     string `json:"state"`
	Reason    string `json:"reason,omitempty"`
	// LastPushSeq and LastPushAt are the newest message it woke the agent
	// for, and when; ConfirmedAt when a push was last confirmed.
	LastPushSeq int64  `json:"last_push_seq"`
	LastPushAt  string `json:"last_push_at,omitempty"`
	ConfirmedAt string `json:"confirmed_at,omitempty"`
	UpdatedAt   string `json:"updated_at"`
	// DevChannels says our plugin is named only through the development
	// flag (--dangerously-load-development-channels), not --channels.
	DevChannels bool `json:"dev_channels,omitempty"`
}

// The reasons a channel record gives, word for word: the channel server
// writes them and doctor explains them. None holds anything from a message.
const (
	ChannelReasonUnconfirmed    = "no push confirmed yet"
	ChannelReasonCustomEndpoint = "custom ANTHROPIC_BASE_URL: active only once a push is confirmed"
	ChannelReasonNoArgv         = "the agent's command line could not be read"
	ChannelReasonNotLaunched    = "not launched with --channels plugin:clawbridge@clawbridge"
	ChannelReasonProvider       = "third-party provider"
	ChannelReasonNoServer       = "no ClawBridge MCP server for this session"
	ChannelReasonFallback       = "no channel turn within 120 s of a push"
	ChannelReasonEnded          = "the channel server has stopped"
	ChannelReasonUnreadable     = "counts unreadable for over 60 s"
)

// ChannelStateFresh is how old a channel record may be and still be
// believed: the channel server rewrites it every 30 s.
const ChannelStateFresh = 90 * time.Second

// SaveChannelState writes session's channel record.
func SaveChannelState(dir, session string, st ChannelState) error {
	path, err := sidePath(dir, session, ".channel.json")
	if err != nil {
		return err
	}
	st.Version = 1
	data, err := json.Marshal(st)
	if err != nil {
		return err
	}
	return writePrivate("channel record", dir, path, data)
}

// LoadChannelState reads session's channel record. A missing one is not an
// error.
func LoadChannelState(dir, session string) (ChannelState, bool, error) {
	path, err := sidePath(dir, session, ".channel.json")
	if err != nil {
		return ChannelState{}, false, err
	}
	data, ok, err := readPrivate("channel record", path)
	if err != nil || !ok {
		return ChannelState{}, false, err
	}
	var st ChannelState
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if dec.Decode(&st) != nil || st.Version != 1 {
		return ChannelState{}, false, fmt.Errorf("the channel record %s is not valid", path)
	}
	switch st.State {
	case ChannelActive, ChannelUnconfirmed, ChannelInactive, ChannelFallback:
	default:
		return ChannelState{}, false, fmt.Errorf("the channel record %s is not valid", path)
	}
	return st, true, nil
}

// ChannelSkipsTyping says whether a notify adapter may leave the wake to
// the channel: its record is fresh, its server alive, and a push was
// confirmed. Anything less and the adapter types as before.
func ChannelSkipsTyping(st ChannelState, now time.Time) bool {
	at, err := time.Parse(time.RFC3339, st.UpdatedAt)
	if err != nil || now.Sub(at) > ChannelStateFresh || at.Sub(now) > time.Minute {
		return false
	}
	return st.State == ChannelActive && st.ConfirmedAt != "" && ProcessAlive(st.PID)
}

// TurnRecord is when the agent's current or last turn started and ended,
// at <context dir>/<session>.turn.json: the UserPromptSubmit hook marks the
// start, the Stop hook the end. The channel server wakes only an idle agent.
// A turn our channel started also leaves its push's seq and when it began:
// the one proof that a push was delivered (a typed hint can't forge it).
type TurnRecord struct {
	Version    int    `json:"version"`
	StartedAt  string `json:"started_at,omitempty"`
	EndedAt    string `json:"ended_at,omitempty"`
	ChannelSeq int64  `json:"channel_seq,omitempty"`
	ChannelAt  string `json:"channel_at,omitempty"`
}

// StaleTurn is how long a turn without an end counts as running: Claude
// Code runs no Stop hook for a turn the user interrupts, so an end may
// never come.
const StaleTurn = 10 * time.Minute

// LoadTurn reads session's turn record. A missing or unreadable one is not
// there: the agent counts as idle, as before the hooks were installed.
func LoadTurn(dir, session string) (TurnRecord, bool) {
	path, err := sidePath(dir, session, ".turn.json")
	if err != nil {
		return TurnRecord{}, false
	}
	data, ok, err := readPrivate("turn record", path)
	if err != nil || !ok {
		return TurnRecord{}, false
	}
	var rec TurnRecord
	if json.Unmarshal(data, &rec) != nil || rec.Version != 1 {
		return TurnRecord{}, false
	}
	return rec, true
}

// MarkTurnStarted notes that session's agent started a turn at now. A
// channel turn's mark is kept.
func MarkTurnStarted(dir, session string, now time.Time) error {
	return markTurn(dir, session, func(r *TurnRecord) { r.StartedAt = now.UTC().Format(time.RFC3339Nano) })
}

// MarkChannelTurn notes that session's agent started, at now, the turn our
// channel's push of seq began.
func MarkChannelTurn(dir, session string, seq int64, now time.Time) error {
	if seq <= 0 || seq > maxChannelSeq {
		return fmt.Errorf("%d is not a channel seq", seq)
	}
	at := now.UTC().Format(time.RFC3339Nano)
	return markTurn(dir, session, func(r *TurnRecord) { r.StartedAt, r.ChannelSeq, r.ChannelAt = at, seq, at })
}

// MarkTurnEnded notes that session's agent ended its turn at now.
func MarkTurnEnded(dir, session string, now time.Time) error {
	return markTurn(dir, session, func(r *TurnRecord) { r.EndedAt = now.UTC().Format(time.RFC3339Nano) })
}

func markTurn(dir, session string, mark func(*TurnRecord)) error {
	path, err := sidePath(dir, session, ".turn.json")
	if err != nil {
		return err
	}
	rec, _ := LoadTurn(dir, session)
	rec.Version = 1
	mark(&rec)
	data, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	return writePrivate("turn record", dir, path, data)
}

// Idle says whether the agent is between turns at now: no turn record, no
// start known, an end at or after the start, or a start more than
// StaleTurn ago with no end since.
func Idle(rec TurnRecord, ok bool, now time.Time) bool {
	if !ok {
		return true
	}
	started, err := time.Parse(time.RFC3339Nano, rec.StartedAt)
	if err != nil {
		return true
	}
	if ended, err := time.Parse(time.RFC3339Nano, rec.EndedAt); err == nil && !ended.Before(started) {
		return true
	}
	return now.Sub(started) > StaleTurn
}

// maxChannelSeq is the largest seq a push carries (notify.MaxSequence).
const maxChannelSeq int64 = 9007199254740991

// channelTag is the head of a turn our channel started, as Claude Code
// shows it: the tag with our source first, then attributes.
const channelTag = `<channel source="plugin:clawbridge:wake"`

var (
	channelAttr  = regexp.MustCompile(`\A[ \t\r\n]+([a-z]+)="([0-9a-z]{1,16})"`)
	channelClose = regexp.MustCompile(`\A[ \t\r\n]*>`)
)

// ChannelTurnSeq reads the seq of the push that started a turn from its
// prompt, or false when the prompt isn't our channel's. The prompt must
// begin, after a little whitespace, with our source's tag, whose other
// attributes are exactly kind="wake", count and seq, both digits. Only the
// seq is returned; nothing else of the prompt is kept.
func ChannelTurnSeq(prompt string) (int64, bool) {
	if len(prompt) > 1024 {
		prompt = prompt[:1024]
	}
	rest := strings.TrimLeft(prompt, " \t\r\n")
	if len(prompt)-len(rest) > 64 || !strings.HasPrefix(rest, channelTag) {
		return 0, false
	}
	rest = rest[len(channelTag):]
	attrs := map[string]string{}
	for len(attrs) < 4 {
		m := channelAttr.FindStringSubmatch(rest)
		if m == nil {
			break
		}
		if _, dup := attrs[m[1]]; dup {
			return 0, false
		}
		attrs[m[1]] = m[2]
		rest = rest[len(m[0]):]
	}
	if len(attrs) != 3 || attrs["kind"] != "wake" || !digitsOnly(attrs["count"]) || !digitsOnly(attrs["seq"]) || !channelClose.MatchString(rest) {
		return 0, false
	}
	seq, err := strconv.ParseInt(attrs["seq"], 10, 64)
	if err != nil || seq <= 0 || seq > maxChannelSeq {
		return 0, false
	}
	return seq, true
}

func digitsOnly(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}
