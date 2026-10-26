package cli

import (
	"bytes"
	"strings"
	"testing"
)

const closesProject = "33333333-3333-4333-8333-333333333333"

// typing stands in for a terminal where a person types input.
func typing(t *testing.T, terminal bool, input string, environ ...string) *bytes.Buffer {
	t.Helper()
	oldIn, oldTerm, oldEnv := closesStdin, stdinTerminal, closesEnv
	closesStdin = strings.NewReader(input)
	stdinTerminal = func() bool { return terminal }
	closesEnv = func() []string { return environ }
	t.Cleanup(func() { closesStdin, stdinTerminal, closesEnv = oldIn, oldTerm, oldEnv })
	return &bytes.Buffer{}
}

// §10.8 SF3-M1: loosening is typed by a person at a terminal, never under
// an agent's environment or a session.
func TestAgentClosesIsConfirmedByAPerson(t *testing.T) {
	names := map[string]string{closesProject: "Payments"}
	stderr := typing(t, true, "Payments\n")
	if err := confirmAgentCloses(Config{}, names, stderr); err != nil || !strings.Contains(stderr.String(), `"Payments"`) {
		t.Fatal(err, stderr.String())
	}
	for name, c := range map[string]struct {
		terminal bool
		input    string
		environ  []string
		flags    Config
		want     string
	}{
		"not a terminal":   {false, "Payments\n", nil, Config{}, "not a terminal"},
		"wrong name":       {true, "payments\n", nil, Config{}, "not \"Payments\""},
		"nothing typed":    {true, "", nil, Config{}, "no confirmation"},
		"claude code":      {true, "Payments\n", []string{"CLAUDECODE=1"}, Config{}, "CLAUDECODE"},
		"codex":            {true, "Payments\n", []string{"PATH=/bin", "CODEX_SANDBOX=seatbelt"}, Config{}, "CODEX_SANDBOX"},
		"a session's env":  {true, "Payments\n", []string{"CLAWBRIDGE_SESSION=s1"}, Config{}, "CLAWBRIDGE_SESSION"},
		"an instance":      {true, "Payments\n", []string{"CLAWBRIDGE_INSTANCE=i"}, Config{}, "CLAWBRIDGE_INSTANCE"},
		"a session's flag": {true, "Payments\n", nil, Config{Session: "s1"}, "--session"},
	} {
		stderr := typing(t, c.terminal, c.input, c.environ...)
		if err := confirmAgentCloses(c.flags, names, stderr); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Fatal(name, err)
		}
	}
	// Several projects: each name, in order of UUID.
	stderr = typing(t, true, "Alpha\nBeta\n")
	if err := confirmAgentCloses(Config{}, map[string]string{"44444444-4444-4444-8444-444444444444": "Beta", closesProject: "Alpha"}, stderr); err != nil {
		t.Fatal(err)
	}
}

func TestLinkClosesFlags(t *testing.T) {
	v, _, err := parseLinear("link", []string{"--profile", "native", "--team", closesProject, "--default-team", closesProject, "--linear-project", closesProject, "--expected-revision", "0", "--closes", closesProject + "=agent", "--minimal-status-policy"}, &bytes.Buffer{})
	if err != nil || !v.minimalPolicy {
		t.Fatal(v, err)
	}
	got, err := linkCloses(v.closesPairs)
	if err != nil || got[closesProject] != "agent" {
		t.Fatal(got, err)
	}
	for _, bad := range [][]string{{closesProject + "=bot"}, {"FIX-1=agent"}, {closesProject}, {closesProject + "=agent", closesProject + "=human"}} {
		if _, err = linkCloses(bad); err == nil {
			t.Fatal("accepted", bad)
		}
	}
	if v, _, err = parseLinear("project", []string{"Q4", "--team", closesProject, "--operation-id", "op-1", "--profile", "native", "--closes", "agent"}, &bytes.Buffer{}); err != nil || v.closesWho != "agent" {
		t.Fatal(v, err)
	}
	if _, _, err = parseLinear("label", []string{"Bug", "--team", closesProject, "--operation-id", "op-1", "--profile", "native", "--closes", "agent"}, &bytes.Buffer{}); err == nil {
		t.Fatal("--closes on label create")
	}
	if v, _, err = parseLinear("closes", []string{"--linear-project", closesProject, "--human", "--expected-revision", "3"}, &bytes.Buffer{}); err != nil || !v.closesHuman || v.revision != 3 {
		t.Fatal(v, err)
	}
}

func TestClosesSetNeedsOneValueAndARevision(t *testing.T) {
	for _, args := range [][]string{
		{"closes", "set", "--linear-project", closesProject, "--expected-revision", "1"},
		{"closes", "set", "--linear-project", closesProject, "--agent", "--human", "--expected-revision", "1"},
		{"closes", "set", "--linear-project", closesProject, "--human"},
		{"closes", "set", "--linear-project", "FIX-1", "--human", "--expected-revision", "1"},
		{"closes", "list"},
	} {
		want := "closes set requires"
		if args[1] == "list" {
			want = "closes show|set"
		}
		if err := runLinear(t.Context(), Config{}, args, &bytes.Buffer{}, &bytes.Buffer{}); err == nil || !strings.Contains(err.Error(), want) {
			t.Fatal(args, err)
		}
	}
}
