package cli

import (
	"strings"

	"clawbridge/internal/doctor"
)

// doctorAnswer is how doctor's human view says a check came out (O3, the
// owner's request): a short answer for each status, in place of the
// check's message on its line. The message keeps its values (counts,
// paths, versions, hosts) and is shown under the answer, never parsed;
// --json still carries it unchanged. An empty answer keeps the message on
// the line: it already reads as one.
type doctorAnswer struct{ pass, warn, fail string }

func (a doctorAnswer) in(status string) string {
	switch status {
	case doctor.Pass:
		return a.pass
	case doctor.Warn:
		return a.warn
	case doctor.Fail:
		return a.fail
	}
	return ""
}

// doctorAnswers has an entry for every check doctor emits; a test fails
// when a new check has none. An answer must be true of every message its
// check can give with that status. All of one agent's MCP entries (mcp.AGENT.NAME)
// share "mcp.entry".
var doctorAnswers = map[string]doctorAnswer{
	"config.trusted": {
		pass: "Your ClawBridge settings are complete.",
		fail: "Your ClawBridge settings can't be used.",
	},
	"authority.health": {
		pass: "Your workspace answers.",
		fail: "Your workspace can't be reached.",
	},
	// A pass can be "not used here" (local mode): it keeps its message.
	"auth.token-selection": {
		fail: "Your Google sign-in doesn't work.",
	},
	"auth.device": {
		fail: "This device's key can't be used.",
	},
	"project.resolution": {
		pass: "Your project is found.",
		fail: "Your project can't be found.",
	},
	"mirror.freshness": {
		warn: "The offline copy is missing or old, so reads need the server.",
	},
	"mcp.entries": {
		warn: "No agent's ClawBridge connection was checked.",
	},
	"mcp.entry": {
		pass: "This agent is connected to ClawBridge.",
		warn: "This agent's ClawBridge connection is missing or can't start.",
		fail: "This agent's ClawBridge connection is wrong.",
	},
	"skill.version": {
		warn: "Your agents' ClawBridge skill needs attention.",
	},
	"linear.capabilities": {
		pass: "Your Linear connection works.",
		warn: "Your Linear connection couldn't be checked, or some of what it can do is off.",
	},
	"linear.binding": {
		warn: "The task board couldn't be checked.",
		fail: "The task board link is broken.",
	},
	// These messages already read as answers.
	"auth.clock":                {},
	"context":                   {},
	"queue":                     {},
	"coordinator.playbook":      {},
	"linear.team_roles":         {},
	"linear.team_roles_doc":     {},
	"linear.api_key":            {},
	"linear.app":                {},
	"linear.label_policy":       {},
	"linear.issue_subscribers":  {},
	"linear.labels":             {},
	"linear.markers":            {},
	"linear.outbox":             {},
	"linear.instances":          {},
	"linear.watch":              {},
	"dispatcher.model.agent":    {},
	"dispatcher.model.capacity": {},
	"dispatcher.model.key":      {},
	"dispatcher.model.launcher": {},
	"dispatcher.model.projects": {},
	"dispatcher.model.seal":     {},
}

// doctorAnswerKey is the table's key for a check ID.
func doctorAnswerKey(id string) string {
	if strings.HasPrefix(id, "mcp.") && id != "mcp.entries" {
		return "mcp.entry"
	}
	return id
}

// doctorAnswerOf is a check's answer for its status, or "" when its
// message already reads as one. The TUI's Doctor tab shows the same.
func doctorAnswerOf(c doctor.Check) string {
	return doctorAnswers[doctorAnswerKey(c.ID)].in(c.Status)
}

// doctorLine is a check's line and the detail under it: the answer and
// the message, or the message alone.
func doctorLine(c doctor.Check) (line, detail string) {
	line = firstNonEmpty(c.Title, c.ID)
	if answer := doctorAnswerOf(c); answer != "" {
		return line + ": " + answer, c.Message
	}
	if c.Message != "" {
		line += ": " + c.Message
	}
	return line, ""
}
