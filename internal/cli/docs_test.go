package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"clawbridge/internal/bus"
)

// The owner sets the document policy from a file at its revision; it reads
// back, and the document reads refuse ambiguous arguments before any
// Linear call.
func TestCLIDocPolicyAndDocArguments(t *testing.T) {
	isolated(t)
	invoke(t, "setup", "--local", "--workspace", "w")
	invoke(t, "project", "create", "p")
	t.Setenv("CLAWBRIDGE_PROJECT", "p")
	show := func() string {
		t.Helper()
		var out, errs bytes.Buffer
		if e := Run(context.Background(), []string{"linear", "doc-policy", "show"}, &out, &errs); e != nil {
			t.Fatal(e, errs.String())
		}
		return out.String()
	}
	if got := show(); !strings.Contains(got, `"revision":0`) || !strings.Contains(got, `"initiatives":[]`) {
		t.Fatal(got)
	}
	file := filepath.Join(t.TempDir(), "policy.json")
	write := func(body string) {
		if e := os.WriteFile(file, []byte(body), 0o600); e != nil {
			t.Fatal(e)
		}
	}
	write(`{"source_root": "~/src/example", "deny_patterns": [{"id": "internal-name", "regex": "internal-name-[0-9]+"}], "initiatives": ["1a1a0000-0000-4000-8000-000000000001"]}`)
	var out, errs bytes.Buffer
	if e := Run(context.Background(), []string{"linear", "doc-policy", "set", "--file", file, "--expected-revision", "0"}, &out, &errs); e != nil || !strings.Contains(out.String(), `"revision":1`) {
		t.Fatal(e, out.String(), errs.String())
	}
	if got := show(); !strings.Contains(got, "1a1a0000-0000-4000-8000-000000000001") || !strings.Contains(got, `"regex":"internal-name-[0-9]+"`) {
		t.Fatal(got)
	}
	rejectCLI(t, "linear", "doc-policy", "set", "--file", file, "--expected-revision", "0")
	write(`{"source_root": "~/a", "source_root": "~/b"}`)
	rejectCLI(t, "linear", "doc-policy", "set", "--file", file, "--expected-revision", "1")
	write(`{"deny_patterns": [{"id": "x", "regex": "("}]}`)
	rejectCLI(t, "linear", "doc-policy", "set", "--file", file, "--expected-revision", "1")
	rejectCLI(t, "linear", "doc-policy", "set", "--file", file)
	rejectCLI(t, "linear", "doc-policy", "drop")
	rejectCLI(t, "linear", "doc", "publish")
	rejectCLI(t, "linear", "doc", "list")
	rejectCLI(t, "linear", "initiative", "show", "1a1a0000-0000-4000-8000-000000000001")
}

// linearDoc refuses mixed or missing arguments without reaching the board.
func TestLinearDocArguments(t *testing.T) {
	for name, c := range map[string]struct {
		action string
		v      linearFlags
		pos    []string
	}{
		"get without an ID":   {"get", linearFlags{}, nil},
		"get with a parent":   {"get", linearFlags{team: "t"}, []string{"d"}},
		"get with a cursor":   {"get", linearFlags{cursor: "c"}, []string{"d"}},
		"list with an ID":     {"list", linearFlags{}, []string{"d"}},
		"list with max bytes": {"list", linearFlags{maxBytes: 5}, nil},
		"list two parents":    {"list", linearFlags{team: "t", initiative: "i"}, nil},
	} {
		if _, e := linearDoc(context.Background(), nil, bus.Request{}, c.action, c.v, c.pos); e == nil {
			t.Fatal(name, "accepted")
		}
	}
}

// initiative show is a read: it takes a profile and one UUID, and no
// operation ID; the initiative writes still require one.
func TestInitiativeShowTakesNoOperationID(t *testing.T) {
	isolated(t)
	const initiative = "1a1a0000-0000-4000-8000-000000000001"
	v, pos, err := parseLinear("initiative-show", []string{initiative, "--profile", "native"}, &bytes.Buffer{})
	if err != nil || len(pos) != 1 || pos[0] != initiative || v.profile != "native" {
		t.Fatal(v, pos, err)
	}
	for _, args := range [][]string{{"--profile", "native"}, {initiative, "x", "--profile", "native"}, {initiative}, {initiative, "--profile", "native", "--operation-id", "x"}} {
		if _, _, err := parseLinear("initiative-show", args, &bytes.Buffer{}); err == nil {
			t.Fatal("accepted", args)
		}
	}
	if _, _, err := parseLinear("initiative", []string{"X", "--profile", "native"}, &bytes.Buffer{}); err == nil || !strings.Contains(err.Error(), "--operation-id") {
		t.Fatal("an initiative write without an operation ID:", err)
	}
	// Through the command: it gets past parsing to the profile, not the
	// operation ID.
	var out, errs bytes.Buffer
	err = Run(context.Background(), []string{"linear", "initiative", "show", initiative, "--profile", "native"}, &out, &errs)
	if err != nil && strings.Contains(err.Error(), "operation-id") {
		t.Fatal(err)
	}
}

// linearPublish refuses a missing file or an ambiguous parent before the
// board.
func TestLinearPublishArguments(t *testing.T) {
	emit := func(any) error { t.Fatal("emitted"); return nil }
	for name, c := range map[string]struct {
		v   linearFlags
		pos []string
	}{
		"no file":       {linearFlags{team: "t"}, nil},
		"two files":     {linearFlags{team: "t"}, []string{"a.md", "b.md"}},
		"two parents":   {linearFlags{team: "t", initiative: "i"}, []string{"a.md"}},
		"with a cursor": {linearFlags{team: "t", cursor: "c"}, []string{"a.md"}},
	} {
		if e := linearPublish(context.Background(), nil, bus.Request{}, c.v, c.pos, emit); e == nil {
			t.Fatal(name, "accepted")
		}
	}
	rejectCLI(t, "linear", "doc", "publish", "plan.md", "--team", "11111111-2222-3333-4444-555555555555", "--operation-id", "op")
}
