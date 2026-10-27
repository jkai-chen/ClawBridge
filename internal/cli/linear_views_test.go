package cli

import (
	"bytes"
	"context"
	"reflect"
	"strings"
	"testing"

	"clawbridge/internal/linearconn"
	"clawbridge/internal/linearprovider"
)

// view list and view show parse their flags, and refuse bad ones before
// any profile or Linear call.
func TestLinearViewParsing(t *testing.T) {
	const team = "11111111-1111-4111-8111-111111111111"
	v, _, err := parseLinear("view-list", []string{"--profile", "p", "--team", team, "--type", "project", "--limit", "20"}, &bytes.Buffer{})
	if err != nil || v.team != team || v.viewKind != "project" || v.limit != 20 {
		t.Fatal(v, err)
	}
	if _, pos, err := parseLinear("view-show", []string{team, "--profile", "p"}, &bytes.Buffer{}); err != nil || !reflect.DeepEqual(pos, []string{team}) {
		t.Fatal(pos, err)
	}
	// Each bad flag or ID is refused by the parser itself.
	for command, args := range map[string][]string{
		"view-list":  {"--profile", "p", "--type", "initiative"},
		"view-show":  {"--profile", "p", "Open bugs"},
		"view-show ": {"--team", "ENG", "--profile", "p", team},
	} {
		if _, _, err := parseLinear(strings.TrimSpace(command), args, &bytes.Buffer{}); err == nil {
			t.Fatal("parsed", command, args)
		}
	}
	isolated(t)
	previous := newLinearManager
	t.Cleanup(func() { newLinearManager = previous })
	newLinearManager = func(string) *linearconn.Manager {
		t.Fatal("a refused view command touched the manager")
		return nil
	}
	for _, args := range [][]string{
		{"linear", "view"},
		{"linear", "view", "save"},
		{"linear", "view", "list", "--type", "initiative", "--profile", "p"},
		{"linear", "view", "list", "--type", "Issue", "--profile", "p"},
		{"linear", "view", "list", "--team", "ENG", "--profile", "p"},
		{"linear", "view", "show", "--profile", "p"},
		{"linear", "view", "show", "Open bugs", "--profile", "p"},
		{"linear", "view", "show", team, team, "--profile", "p"},
		{"linear", "view", "show", team, "--type", "issue", "--profile", "p"},
	} {
		rejectCLI(t, args...)
	}
	var out bytes.Buffer
	if err := Run(context.Background(), []string{"linear", "--help"}, &out, &bytes.Buffer{}); err != nil || !strings.Contains(out.String(), "view list") {
		t.Fatal(out.String(), err)
	}
}

// Views carry other people's text: the answer drops what could drive a
// terminal.
func TestViewAnswerIsTerminalSafe(t *testing.T) {
	out, err := safeData(linearprovider.ViewPage{Page: linearprovider.Page[linearprovider.View]{Items: []linearprovider.View{{ID: "a1a1a1a1-a1a1-4a1a-8a1a-a1a1a1a1a1a1", Name: "Bugs\u202e\x1b[2J", Kind: "issue"}}, Complete: true}, WorkspaceViews: 1})
	if err != nil {
		t.Fatal(err)
	}
	text := jsonText(out)
	if strings.ContainsAny(text, "\u202e\x1b") || !strings.Contains(text, "Bugs") || !strings.Contains(text, `"workspace_views":1`) {
		t.Fatal(text)
	}
}
