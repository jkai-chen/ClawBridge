package cli

import (
	"bytes"
	"context"
	"reflect"
	"testing"

	"clawbridge/internal/linearconn"
)

func TestLinearV2ExplicitDestinationsAndClearIntent(t *testing.T) {
	const a = "11111111-1111-4111-8111-111111111111"
	const b = "22222222-2222-4222-8222-222222222222"
	globals, rest, err := extractGlobals([]string{"--project", "cb-scope", "linear", "create", "--linear-project", a})
	if err != nil || globals.Project != "cb-scope" || !reflect.DeepEqual(rest, []string{"linear", "create", "--linear-project", a}) {
		t.Fatal("provider destination confused with authority scope", globals, rest, err)
	}
	link, _, err := parseLinear("link", []string{"--profile", "native", "--team", a, "--default-team", a, "--linear-project", b, "--linear-project", a, "--expected-revision", "0"}, &bytes.Buffer{})
	if err != nil || !reflect.DeepEqual(link.projects, []string{a, b}) {
		t.Fatal("explicit project set lost", link.projects, err)
	}
	for _, command := range []string{"create", "update"} {
		args := []string{"--profile", "native", "--operation-id", "explicit", "--title", "new", "--linear-project", a, "--parent", b, "--assignee", a, "--label", b, "--label", a}
		if command == "update" {
			args = append([]string{a, "--expect-label", b}, args...)
		}
		v, pos, e := parseLinear(command, args, &bytes.Buffer{})
		if e != nil {
			t.Fatal(command, e)
		}
		input, e := linearWriteInput(command, v, pos)
		if e != nil || !input.Project.Present || input.Project.Value == nil || *input.Project.Value != a || !input.Parent.Present || input.Parent.Value == nil || *input.Parent.Value != b || !input.Assignee.Present || input.Assignee.Value == nil || !input.Labels.Present || !reflect.DeepEqual(input.Labels.Value, []string{a, b}) {
			t.Fatalf("typed intent lost: %+v %v", input, e)
		}
	}
	v, pos, err := parseLinear("update", []string{a, "--profile", "native", "--operation-id", "clear", "--clear-project", "--clear-labels", "--expect-no-labels", "--clear-parent", "--clear-assignee"}, &bytes.Buffer{})
	if err != nil {
		t.Fatal(err)
	}
	input, err := linearWriteInput("update", v, pos)
	if err != nil || !input.Project.Present || input.Project.Value != nil || !input.Parent.Present || input.Parent.Value != nil || !input.Assignee.Present || input.Assignee.Value != nil || !input.Labels.Present || input.Labels.Value == nil || len(input.Labels.Value) != 0 {
		t.Fatalf("clear collapsed to omission: %+v %v", input, err)
	}
	v, pos, err = parseLinear("update", []string{a, "--profile", "native", "--operation-id", "legacy", "--title", "new"}, &bytes.Buffer{})
	if err != nil {
		t.Fatal(err)
	}
	input, err = linearWriteInput("update", v, pos)
	if err != nil || input.Project.Present || input.Parent.Present || input.Assignee.Present || input.Labels.Present || input.TeamID != "" {
		t.Fatal("legacy-only intent gained new fields", input, err)
	}
}

func TestLinearV2HelpAndInvalidInputBeforeManager(t *testing.T) {
	isolated(t)
	previous := newLinearManager
	t.Cleanup(func() { newLinearManager = previous })
	newLinearManager = func(string) *linearconn.Manager {
		t.Fatal("help/invalid arguments touched manager")
		return nil
	}
	for _, command := range []string{"labels", "members", "relation", "create", "update", "link"} {
		var out, diag bytes.Buffer
		if err := Run(context.Background(), []string{"linear", command, "--help"}, &out, &diag); err != nil {
			t.Fatal(command, err)
		}
	}
	const id = "11111111-1111-4111-8111-111111111111"
	const other = "22222222-2222-4222-8222-222222222222"
	for _, args := range [][]string{
		{"linear", "update", id, "--profile", "p", "--operation-id", "op", "--parent", other, "--clear-parent"},
		{"linear", "update", id, "--profile", "p", "--operation-id", "op", "--label", other, "--clear-labels"},
		{"linear", "update", id, "--profile", "p", "--operation-id", "op", "--linear-project", other, "--clear-project"},
		{"linear", "update", id, "--profile", "p", "--operation-id", "op", "--assignee", other, "--clear-assignee"},
		{"linear", "update", id, "--profile", "p", "--operation-id", "op", "--label", other, "--label", other},
		{"linear", "create", "--profile", "p", "--operation-id", "op", "--title", "x", "--parent", ""},
		{"linear", "members", "--profile", "p", "--team", "FJA"},
		{"linear", "labels", "--profile", "p"},
		{"linear", "relation", "replace", id, other, "--profile", "p", "--operation-id", "op", "--type", "blocks"},
		{"linear", "relation", "add", id, id, "--profile", "p", "--operation-id", "op", "--type", "blocks"},
		{"linear", "relation", "remove", id, other, "--profile", "p", "--operation-id", "op", "--type", "duplicate"},
	} {
		rejectCLI(t, args...)
	}
}
