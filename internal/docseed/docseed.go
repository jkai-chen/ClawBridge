// Package docseed renders the Linear guide documents ClawBridge seeds when a
// board is linked (live/DOC-SEED-DESIGN.md). The templates are generic: the
// repository is public, and each team adds its own practice by editing the
// seeded documents, which ClawBridge then never overwrites.
//
// docs/templates holds the readable source; templates/ here is the copy the
// binary embeds, and a test keeps the two identical.
package docseed

import (
	"bytes"
	"crypto/rand"
	"embed"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"text/template"
	"unicode"
)

//go:embed templates/*.md
var files embed.FS

// Template is one guide document. Version rises whenever the text changes,
// so an untouched seeded document can be offered the update.
type Template struct {
	Name    string `json:"template"`
	Title   string `json:"title"`
	File    string `json:"-"`
	Version int    `json:"template_version"`
}

// Templates are the seeded documents, in the order they are created.
var Templates = []Template{
	{Name: "developer", Title: "Working with Linear through ClawBridge", File: "linear-developer-guide.md", Version: 3},
	{Name: "reviewer", Title: "Giving feedback in Linear", File: "linear-reviewer-guide.md", Version: 3},
}

// Labels the templates name; they are the labels ClawBridge itself uses.
const (
	NeedsAgentLabel   = "needs-agent"
	AgentWorkingLabel = "agent-working"
)

// Values fill a template. Each is shown as a code span, so it can never be
// read as a link, formatting or a mention.
type Values struct {
	ServiceURL string
	Version    string
}

type fields struct {
	ServiceURL, NeedsAgentLabel, AgentWorkingLabel string
}

// code renders a value as a Markdown code span: one printable line, no
// backtick, bounded.
func code(v string) (string, error) {
	if len(v) > 200 || strings.ContainsRune(v, '`') || strings.ContainsFunc(v, func(r rune) bool { return !unicode.IsPrint(r) }) {
		return "", fmt.Errorf("value %q cannot be shown in a guide document", v)
	}
	if v == "" {
		return "", nil
	}
	return "`" + v + "`", nil
}

// Render is the document's content: the template, its footer, and the seed
// token line that lets an uncertain create be found again.
func Render(t Template, v Values, token string) (string, error) {
	if !tokenPattern(token) {
		return "", errors.New("a seed token is 12 lowercase hex characters")
	}
	raw, err := files.ReadFile("templates/" + t.File)
	if err != nil {
		return "", err
	}
	tpl, err := template.New(t.Name).Option("missingkey=error").Parse(string(raw))
	if err != nil {
		return "", err
	}
	var f fields
	for dst, src := range map[*string]string{&f.ServiceURL: v.ServiceURL, &f.NeedsAgentLabel: NeedsAgentLabel, &f.AgentWorkingLabel: AgentWorkingLabel} {
		if *dst, err = code(src); err != nil {
			return "", err
		}
	}
	version, err := code(v.Version)
	if err != nil || version == "" {
		return "", errors.New("the ClawBridge version is required")
	}
	var out bytes.Buffer
	if err = tpl.Execute(&out, f); err != nil {
		return "", err
	}
	fmt.Fprintf(&out, "\n---\n\nSeeded by ClawBridge %s (template %d). Edit freely: ClawBridge never overwrites your edits.\n\n%s\n", version, t.Version, TokenLine(token))
	return out.String(), nil
}

// NewToken is a fresh seed token.
func NewToken() (string, error) {
	var b [6]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

// TokenLine is the line that carries a seed token.
func TokenLine(token string) string { return "seed: " + token }

// HasToken reports whether content, as Linear returns it, carries token.
func HasToken(content, token string) bool {
	return tokenPattern(token) && strings.Contains(content, TokenLine(token))
}

func tokenPattern(t string) bool {
	if len(t) != 12 {
		return false
	}
	for _, r := range t {
		if !(r >= '0' && r <= '9' || r >= 'a' && r <= 'f') {
			return false
		}
	}
	return true
}
