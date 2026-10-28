package cli

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"clawbridge/internal/bus"
	"clawbridge/internal/linearboard"
	"clawbridge/internal/linearkey"
)

const viewTeamID = "11111111-1111-4111-8111-111111111111"

// view create takes --team; view update takes the view's ID and no
// --team; both take --name, --filter-file, --operation-id and --confirm.
func TestLinearViewWriteParsing(t *testing.T) {
	v, pos, err := parseLinear("view-create", []string{"--profile", "p", "--team", viewTeamID, "--name", "Urgent bugs", "--filter-file", "f.json", "--operation-id", "op-1", "--confirm"}, &bytes.Buffer{})
	if err != nil || v.team != viewTeamID || v.viewName != "Urgent bugs" || v.filterFile != "f.json" || v.operation != "op-1" || !v.confirm || len(pos) != 0 {
		t.Fatal(v, err)
	}
	if v, pos, err = parseLinear("view-update", []string{viewTeamID, "--profile", "p", "--name", "Bugs", "--operation-id", "op-2"}, &bytes.Buffer{}); err != nil || pos[0] != viewTeamID || v.confirm {
		t.Fatal(v, pos, err)
	}
	for command, args := range map[string][]string{
		"view-update": {"view-1", "--profile", "p", "--team", viewTeamID, "--operation-id", "op"},
		"view-create": {"view-1", "--profile", "p", "--team", viewTeamID, "--operation-id", "op"},
	} {
		if _, _, err = parseLinear(command, args, &bytes.Buffer{}); err == nil {
			t.Fatal("parsed", command, args)
		}
	}
	if v, pos, err = parseLinear("view-delete", []string{viewTeamID, "--profile", "p", "--operation-id", "op-3", "--confirm"}, &bytes.Buffer{}); err != nil || pos[0] != viewTeamID || !v.confirm || v.operation != "op-3" {
		t.Fatal(v, pos, err)
	}
	for _, args := range [][]string{{"--profile", "p", "--operation-id", "op"}, {viewTeamID, "--profile", "p", "--name", "x", "--operation-id", "op"}} {
		if _, _, err = parseLinear("view-delete", args, &bytes.Buffer{}); err == nil {
			t.Fatal("parsed", args)
		}
	}
	if v, _, err = parseLinear("view-apply", []string{"--profile", "p", "--file", "set.json", "--adopt", "Triage", "--adopt", "Bugs", "--operation-id", "set-1", "--confirm"}, &bytes.Buffer{}); err != nil || v.filterFile != "set.json" || len(v.adoptNames) != 2 || v.adoptNames[1] != "Bugs" || !v.confirm {
		t.Fatal(v, err)
	}
	if _, _, err = parseLinear("view-apply", []string{"extra", "--profile", "p", "--file", "f", "--operation-id", "op"}, &bytes.Buffer{}); err == nil {
		t.Fatal("apply took a positional")
	}
	var out bytes.Buffer
	if err = Run(context.Background(), []string{"linear", "--help"}, &out, &bytes.Buffer{}); err != nil || !strings.Contains(out.String(), "apply --file F [--adopt NAME]") {
		t.Fatal(out.String(), err)
	}
}

// K2: under an agent's environment the write is refused before anything
// is read, named by its marker.
func TestLinearViewWriteRefusesAnAgent(t *testing.T) {
	child(t, "TestLinearViewWriteRefusesAnAgent", "CLAUDECODE=1")
	v := linearFlags{adminAction: "create", team: viewTeamID, viewName: "x", operation: "op", profile: "p"}
	if _, err := linearViewWrite(context.Background(), nil, bus.Request{}, t.TempDir(), v, nil); err == nil || !strings.Contains(err.Error(), "CLAUDECODE") {
		t.Fatal(err)
	}
}

// The preview's next line repeats the write with --confirm, its names
// terminal-safe.
func TestViewConfirmLine(t *testing.T) {
	v := linearFlags{profile: "p", filterFile: "f.json"}
	line := viewConfirmLine(v, linearboard.ViewWriteInput{Action: "create", Team: viewTeamID, Name: "Bugs\u202e\x1b[2J", OperationID: "op-1"}, linearboard.ViewWriteResult{BindingRevision: 3, FilterSHA: "abc"})
	if line != `clawbridge linear view create --team `+viewTeamID+` --name "Bugs[2J" --filter-file "f.json" --operation-id op-1 --profile p --expected-revision 3 --expected-filter-sha256 abc --confirm` {
		t.Fatal(line)
	}
	line = viewConfirmLine(linearFlags{profile: "p"}, linearboard.ViewWriteInput{Action: "update", ViewID: "v-1", OperationID: "op-2"}, linearboard.ViewWriteResult{BindingRevision: 4, FilterSHA: "def", ExpectedFilterAsOf: "2026-10-27T21:00:00.000Z"})
	if line != `clawbridge linear view update v-1 --operation-id op-2 --profile p --expected-revision 4 --expected-filter-sha256 def --expected-filter-as-of "2026-10-27T21:00:00.000Z" --confirm` {
		t.Fatal(line)
	}
}

// The key's errors are sorted by what reached Linear.
func TestViewKeyErrors(t *testing.T) {
	for err, want := range map[error]error{
		linearkey.ErrForbidden:    linearboard.ErrKeyNotSent,
		linearkey.ErrRejected:     linearboard.ErrKeyNotSent,
		linearkey.ErrNoKey:        linearboard.ErrKeyNotSent,
		linearkey.ErrViewRefused:  linearboard.ErrKeyRefused,
		linearkey.ErrViewNotFound: linearboard.ErrKeyNotFound,
	} {
		if _, got := (viewKey{}).wrap(linearkey.View{}, err); !errors.Is(got, want) || !errors.Is(got, err) {
			t.Fatal(err, got)
		}
	}
	if _, got := (viewKey{}).wrap(linearkey.View{}, linearkey.ErrUnavailable); errors.Is(got, linearboard.ErrKeyNotSent) || errors.Is(got, linearboard.ErrKeyRefused) {
		t.Fatal("an unavailable write was taken as not sent:", got)
	}
	if v, err := (viewKey{}).wrap(linearkey.View{ID: "v", TeamID: "t", Shared: true, Filter: "{}"}, nil); err != nil || v.ID != "v" || v.TeamID != "t" || !v.Shared || v.Filter != "{}" {
		t.Fatal(v, err)
	}
}
