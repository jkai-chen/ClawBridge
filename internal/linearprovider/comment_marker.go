package linearprovider

import (
	"crypto/sha256"
	"encoding/hex"
	"regexp"
	"strings"
	"unicode/utf8"
)

// CommentMarkerReserve is the room a caller's comment body leaves for the
// agent marker: callers may send at most MaxBodyBytes minus it.
const CommentMarkerReserve = 256

const commentMarkerLead = "— 🤖 ClawBridge agent"

var markerSessionName = regexp.MustCompile(`\A[A-Za-z0-9][A-Za-z0-9._-]{0,127}\z`)

// markerNameRunes is the most of a session name the marker shows; a longer
// name is cut to markerNameRunes-1 characters and "…".
const markerNameRunes = 32

// commentMarker matches the marker as a body's final paragraph, with or
// without a (possibly cut) session name, and with or without the engineer.
var commentMarker = regexp.MustCompile(`(?:\A|\n\n)— 🤖 ClawBridge agent(?: · session ([A-Za-z0-9][A-Za-z0-9._-]{0,31}|[A-Za-z0-9][A-Za-z0-9._-]{30}…))?(?: · [^\n·]{1,40})?\z`)

// MarkComment is the body ClawBridge posts for a caller's comment: the body,
// a blank line, and a fixed attribution line naming the session when its
// name is a plain session name, cut to markerNameRunes characters, and the
// engineer behind it, as the authority named them (bus.EngineerName). The
// line holds nothing secret beyond those and fits in CommentMarkerReserve.
func MarkComment(body, session, engineer string) string {
	line := commentMarkerLead
	if markerSessionName.MatchString(session) {
		if len(session) > markerNameRunes {
			session = session[:markerNameRunes-1] + "…"
		}
		line += " · session " + session
	}
	if engineer = strings.TrimSpace(engineer); engineer != "" && utf8.RuneCountInString(engineer) <= 40 && !strings.ContainsAny(engineer, "\n\r·") {
		line += " · " + engineer
	}
	return body + "\n\n" + line
}

// commentMarkerSession is the session a body's marker names, or "". Anyone
// can type the marker, so it is untrusted text and never proof that
// ClawBridge posted the comment.
func commentMarkerSession(body string) string {
	if m := commentMarker.FindStringSubmatch(body); m != nil {
		return m[1]
	}
	return ""
}

// bodySHA256 is the hex SHA-256 of the exact body posted.
func bodySHA256(body string) string {
	h := sha256.Sum256([]byte(body))
	return hex.EncodeToString(h[:])
}

// commentBodyMatches is false when a receipt or row reports a body other
// than the one posted; a row without a body proves nothing either way.
func commentBodyMatches(m map[string]any, body string) bool {
	raw, present := m["body"]
	if !present || raw == nil {
		return true
	}
	text, ok := raw.(string)
	return ok && text == body
}
