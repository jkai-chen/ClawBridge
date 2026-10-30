package tui

import (
	"strings"

	"clawbridge/internal/clirender"
)

// status is the line under the tabs (TUI-UX-AUDIT §3.3): what the last
// action did, as a word first and a colour second, so it reads the same
// under NO_COLOR. The words are clirender's: OK, ERROR, NOTE and "…".
type status struct {
	level clirender.Level
	text  string
	// sticky keeps the line through the one reload its action started, so
	// "Sent to …" outlives the inbox reload that follows a send (U29).
	sticky bool
}

func okStatus(text string) status   { return status{level: clirender.OK, text: text, sticky: true} }
func noteStatus(text string) status { return status{level: clirender.Info, text: text} }
func busyStatus(text string) status { return status{level: clirender.Busy, text: text} }

// errStatus is a failed action, named in words; the raw error follows.
func errStatus(err error) status {
	what, raw := describeError(displayError(err))
	if raw != "" && raw != what {
		what += " (" + raw + ")"
	}
	return status{level: clirender.Error, text: what}
}

// failStatus is a failed action whose words the caller already has.
func failStatus(text string) status { return status{level: clirender.Error, text: text} }

// kept is the status a reload leaves: a sticky one, once.
func (s status) kept() status {
	if !s.sticky {
		return status{}
	}
	s.sticky = false
	return s
}

func (s status) line() string {
	if s.text == "" {
		return ""
	}
	word := clirender.Word(s.level)
	switch s.level {
	case clirender.OK:
		word = passStyle.Render(word)
	case clirender.Error:
		word = errorStyle.Render(word)
	case clirender.Info, clirender.Busy:
		word = mutedStyle.Render(word)
	}
	return word + "  " + terminalText(s.text)
}

// describeError names what kind of failure a raw error is, with its next
// step, and returns the raw text to show muted beneath it (U10). Errors it
// doesn't recognise are shown as they are.
func describeError(raw string) (what, rest string) {
	low := strings.ToLower(raw)
	has := func(words ...string) bool {
		for _, w := range words {
			if strings.Contains(low, w) {
				return true
			}
		}
		return false
	}
	switch {
	case raw == "":
		return "", ""
	case has("unsupported operation", "unknown operation"):
		return "The server doesn't offer this. Check that the ClawBridge client and server versions match.", raw
	case has("connection refused", "no such host", "i/o timeout", "deadline exceeded", "network is unreachable",
		"502 ", "503 ", "504 ", "bad gateway", "service unavailable", "gateway timeout", "upstream connect", "unexpected eof"):
		return "The server can't be reached.", raw
	case has("401 ", "unauthenticated", "device_auth_failed", "not signed in", "sign in again", "credential expired", "token expired"):
		return "You're signed out. Run clawbridge login.", raw
	case has("403 ", "forbidden", "permission denied", "not allowed", "not permitted"):
		return "You don't have permission for this.", raw
	}
	return raw, ""
}

// The body's states (TUI-UX-AUDIT §3.5): every screen shows one of these
// in place of its rows while they can't be shown.

// loadingBody is shown while a screen's first load runs, so the empty text
// never shows before the answer (U8).
func loadingBody(what string) string { return "Loading " + what + "…" }

// errorBody is a failed load: what failed in words, the keys that help,
// then the raw error, muted (U9, U10).
func errorBody(m Model, what, raw string) string {
	summary, rest := describeError(raw)
	lines := []string{errorStyle.Render(clirender.Word(clirender.Error)) + "  Couldn't load " + what + ". " + terminalText(summary)}
	var keys []string
	for _, id := range []string{"reload", "tab:doctor"} {
		if k := keyHint(m, m.screen, id); k != "" && !(id == "tab:doctor" && m.screen == ScreenDoctor) {
			keys = append(keys, k)
		}
	}
	if len(keys) > 0 {
		lines = append(lines, strings.Join(keys, " · "))
	}
	if rest != "" {
		lines = append(lines, mutedStyle.Render(terminalText(rest)))
	}
	return strings.Join(lines, "\n")
}

// bodyState is the loading or error body for a screen whose rows are not
// loaded yet; ok is false when the rows should be shown.
func bodyState(m Model, what string, loaded bool) (string, bool) {
	switch {
	case m.loadErr != "":
		return errorBody(m, what, m.loadErr), true
	case m.loading && !loaded:
		return loadingBody(what), true
	}
	return "", false
}
