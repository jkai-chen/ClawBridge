package cli

import (
	"bytes"
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"clawbridge/internal/bus"
	"clawbridge/internal/linearboard"
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
		{"linear", "labels", "--profile", "p", "--limit", "5"},
		{"linear", "labels", "--profile", "p", "--cursor", "c"},
		{"linear", "labels", "--profile", "p", "--team", ""},
		{"linear", "labels", "--profile", "p", "--team", "x"},
		{"linear", "relation", "replace", id, other, "--profile", "p", "--operation-id", "op", "--type", "blocks"},
		{"linear", "relation", "add", id, id, "--profile", "p", "--operation-id", "op", "--type", "blocks"},
		{"linear", "relation", "remove", id, other, "--profile", "p", "--operation-id", "op", "--type", "duplicate"},
	} {
		rejectCLI(t, args...)
	}
}

func TestLinearV2TextGuardFlagsReachTheWriteInput(t *testing.T) {
	const a = "11111111-1111-4111-8111-111111111111"
	sum := "0000000000000000000000000000000000000000000000000000000000000000"
	v, pos, err := parseLinear("update", []string{a, "--profile", "native", "--operation-id", "guard", "--title", "new", "--expect-title", "", "--description", "body", "--expect-description-sha256", sum}, &bytes.Buffer{})
	if err != nil {
		t.Fatal(err)
	}
	input, err := linearWriteInput("update", v, pos)
	if err != nil || input.ExpectedTitle == nil || *input.ExpectedTitle != "" || input.ExpectedDescriptionSHA256 == nil || *input.ExpectedDescriptionSHA256 != sum {
		t.Fatalf("text guards lost (an empty expected title is still a guard): %+v %v", input, err)
	}
	v, pos, _ = parseLinear("update", []string{a, "--profile", "native", "--operation-id", "plain", "--title", "new"}, &bytes.Buffer{})
	if input, err = linearWriteInput("update", v, pos); err != nil || input.ExpectedTitle != nil || input.ExpectedDescriptionSHA256 != nil {
		t.Fatal("unguarded update gained guards", input, err)
	}
}

func TestLinearV2LabelDeltaFlagsReachTheWriteInput(t *testing.T) {
	const a = "11111111-1111-4111-8111-111111111111"
	const b = "22222222-2222-4222-8222-222222222222"
	v, pos, err := parseLinear("update", []string{a, "--profile", "native", "--operation-id", "delta", "--add-label", b, "--remove-label", a}, &bytes.Buffer{})
	if err != nil {
		t.Fatal(err)
	}
	input, err := linearWriteInput("update", v, pos)
	if err != nil || !reflect.DeepEqual(input.AddLabels, []string{b}) || !reflect.DeepEqual(input.RemoveLabels, []string{a}) || input.Labels.Present {
		t.Fatalf("label deltas lost: %+v %v", input, err)
	}
}

func TestLinearV2CommentFlagsAreOptIn(t *testing.T) {
	const a = "11111111-1111-4111-8111-111111111111"
	v, pos, err := parseLinear("issue", []string{a, "--profile", "native", "--comments", "--comments-since", "2026-10-22T07:00:00Z"}, &bytes.Buffer{})
	if err != nil || !v.comments || v.commentsSince != "2026-10-22T07:00:00Z" || pos[0] != a {
		t.Fatal("comment flags lost", v.comments, v.commentsSince, err)
	}
	if v, _, err = parseLinear("issue", []string{a, "--profile", "native"}, &bytes.Buffer{}); err != nil || v.comments {
		t.Fatal("plain issue read opted into comments", err)
	}
	if _, _, err = parseLinear("issue", []string{a, "--profile", "native", "--comments-since", "2026-10-22T07:00:00Z"}, &bytes.Buffer{}); err == nil {
		t.Fatal("--comments-since without --comments accepted")
	}
	if _, _, err = parseLinear("issues", []string{"--profile", "native", "--comments"}, &bytes.Buffer{}); err == nil {
		t.Fatal("--comments accepted on the issue list")
	}
}

func TestUnlinkRepairRequiresConfirm(t *testing.T) {
	isolated(t)
	var out, diag bytes.Buffer
	err := Run(context.Background(), []string{"linear", "unlink", "--expected-revision", "1", "--repair"}, &out, &diag)
	if err == nil || !strings.Contains(err.Error(), "--confirm") {
		t.Fatal("repair ran without --confirm", err)
	}
}

func TestLinearCommentReplyToIsTheCommentParent(t *testing.T) {
	const issue = "11111111-1111-4111-8111-111111111111"
	const parent = "c5000000-0000-4000-8000-000000000005"
	v, pos, err := parseLinear("comment", []string{issue, "an answer", "--profile", "native", "--operation-id", "reply-1", "--reply-to", parent}, &bytes.Buffer{})
	if err != nil {
		t.Fatal(err)
	}
	in, err := linearWriteInput("comment", v, pos)
	if err != nil || in.Operation != "comment" || in.Body != "an answer" || !in.Parent.Present || in.Parent.Value == nil || *in.Parent.Value != parent {
		t.Fatal("--reply-to not mapped to the comment parent", in, err)
	}
	v, pos, _ = parseLinear("comment", []string{issue, "plain", "--profile", "native", "--operation-id", "plain-1"}, &bytes.Buffer{})
	if in, err = linearWriteInput("comment", v, pos); err != nil || in.Parent.Present {
		t.Fatal("plain comment sent a parent", in, err)
	}
	for _, bad := range []string{"SYN-6", ""} {
		if _, _, err = parseLinear("comment", []string{issue, "x", "--profile", "native", "--operation-id", "reply-2", "--reply-to", bad}, &bytes.Buffer{}); err == nil {
			t.Fatalf("--reply-to %q accepted", bad)
		}
	}
	if _, _, err = parseLinear("update", []string{issue, "--profile", "native", "--operation-id", "u", "--title", "t", "--reply-to", parent}, &bytes.Buffer{}); err == nil {
		t.Fatal("--reply-to accepted on update")
	}
}

func TestLinearAdminCommandsMapToAdminWrites(t *testing.T) {
	const team = "11111111-1111-4111-8111-111111111111"
	parse := func(command string, args ...string) (linearFlags, []string) {
		t.Helper()
		v, pos, err := parseLinear(command, append(args, "--profile", "native", "--operation-id", "op-1", "--team", team), &bytes.Buffer{})
		if err != nil {
			t.Fatal(command, err)
		}
		return v, pos
	}
	v, pos := parse("label", "Bugs", "Bug old")
	in, err := linearAdminInput("label", "rename", v, pos)
	if err != nil || in.Kind != "label_rename" || in.From != "Bugs" || in.Name != "Bug old" || in.TeamID != team || in.OperationID != "op-1" {
		t.Fatal(in, err)
	}
	v, pos = parse("project", "Q4")
	if in, err = linearAdminInput("project", "create", v, pos); err != nil || in.Kind != "project_create" || in.Name != "Q4" || in.From != "" {
		t.Fatal(in, err)
	}
	if _, err = linearAdminInput("label", "create", v, []string{"A", "B"}); err == nil {
		t.Fatal("label create with two names accepted")
	}
	if _, _, err = parseLinear("label", []string{"Bug", "--profile", "native", "--operation-id", "x"}, &bytes.Buffer{}); err == nil {
		t.Fatal("label without --team accepted")
	}
}

func TestLinearInitiativeCommandsMapToAdminWrites(t *testing.T) {
	const initiative = "e6000000-0000-4000-8000-000000000006"
	const project = "e7000000-0000-4000-8000-000000000007"
	v, pos, err := parseLinear("initiative", []string{"Product Review", "--description", "Reviews", "--profile", "native", "--operation-id", "i-1"}, &bytes.Buffer{})
	if err != nil {
		t.Fatal(err)
	}
	in, err := linearAdminInput("initiative", "create", v, pos)
	if err != nil || in.Kind != "initiative_create" || in.Name != "Product Review" || in.Description != "Reviews" || in.TeamID != "" {
		t.Fatal(in, err)
	}
	v, pos, err = parseLinear("initiative", []string{initiative, project, "--profile", "native", "--operation-id", "i-2"}, &bytes.Buffer{})
	if err != nil {
		t.Fatal(err)
	}
	if in, err = linearAdminInput("initiative", "add-project", v, pos); err != nil || in.Kind != "initiative_add_project" || in.InitiativeID != initiative || in.ProjectID != project {
		t.Fatal(in, err)
	}
	if _, err = linearAdminInput("initiative", "add-project", v, pos[:1]); err == nil {
		t.Fatal("add-project with one argument accepted")
	}
	if _, _, err = parseLinear("initiative", []string{"X", "--profile", "native"}, &bytes.Buffer{}); err == nil {
		t.Fatal("initiative without --operation-id accepted")
	}
	if _, _, err = parseLinear("initiative", []string{"X", "--profile", "native", "--operation-id", "x", "--team", project}, &bytes.Buffer{}); err == nil {
		t.Fatal("initiative with --team accepted")
	}
}

// admin audit-replay needs only the operation ID: it reads no Linear, so it
// takes no profile, and no positional arguments.
func TestLinearAdminAuditReplayParses(t *testing.T) {
	v, pos, err := parseLinear("admin", []string{"--operation-id", "cb-rp-01"}, &bytes.Buffer{})
	if err != nil || v.operation != "cb-rp-01" || len(pos) != 0 {
		t.Fatal(v, pos, err)
	}
	for name, args := range map[string][]string{"no operation": {}, "positional": {"x", "--operation-id", "y"}, "bad operation": {"--operation-id", "a b"}} {
		if _, _, err := parseLinear("admin", args, &bytes.Buffer{}); err == nil {
			t.Fatal(name, "accepted")
		}
	}
	for _, args := range [][]string{{"admin"}, {"admin", "replay", "--operation-id", "x"}} {
		if err := runLinear(context.Background(), Config{}, args, &bytes.Buffer{}, &bytes.Buffer{}); err == nil || !strings.Contains(err.Error(), "admin audit-replay") {
			t.Fatal(args, err)
		}
	}
}

// seed-docs takes its three switches and no positionals; link may skip the
// guide documents.
func TestLinearSeedDocsParses(t *testing.T) {
	v, pos, err := parseLinear("seed-docs", []string{"--profile", "native", "--confirm", "--update", "--reseed"}, &bytes.Buffer{})
	if err != nil || !v.confirm || !v.update || !v.reseed || len(pos) != 0 {
		t.Fatal(v, pos, err)
	}
	if _, _, err = parseLinear("seed-docs", []string{"--profile", "native", "extra"}, &bytes.Buffer{}); err == nil {
		t.Fatal("a positional accepted")
	}
	if _, _, err = parseLinear("seed-docs", []string{"--confirm"}, &bytes.Buffer{}); err == nil {
		t.Fatal("seed-docs without a profile accepted")
	}
	const team = "11111111-1111-4111-8111-111111111111"
	v, _, err = parseLinear("link", []string{"--profile", "native", "--team", team, "--default-team", team, "--team-board", "--expected-revision", "0", "--no-seed-docs"}, &bytes.Buffer{})
	if err != nil || !v.noSeedDocs {
		t.Fatal(v, err)
	}
}

// The link preview names the guides and where they go, from the binding
// being previewed: the default team, or a one-project set's project.
func TestLinkPreviewNamesTheGuideDocuments(t *testing.T) {
	const team = "11111111-1111-4111-8111-111111111111"
	const project = "22222222-2222-4222-8222-222222222222"
	for _, c := range []struct {
		v          linearFlags
		kind, want string
	}{
		{linearFlags{defaultTeam: team, teamBoard: true}, "team", team},
		{linearFlags{defaultTeam: team, projects: []string{project}}, "project", project},
		{linearFlags{defaultTeam: team, projects: []string{project, team}}, "team", team},
	} {
		got := linkWithSeedDocs(context.Background(), nil, bus.Request{}, Config{}, c.v, linearboard.LinkPreviewV2{}).(linearboard.LinkPreviewV2)
		seed, _ := got.SeedDocs.(map[string]any)
		if seed["parent_kind"] != c.kind || seed["parent_id"] != c.want || len(seed["documents"].([]string)) != 2 {
			t.Fatal(c.v, got.SeedDocs)
		}
	}
}

type refusingExecutor struct{}

func (refusingExecutor) Execute(context.Context, bus.Request) (bus.Result, error) {
	return bus.Result{}, errors.New("authority unavailable")
}

// A confirmed link whose seeding fails still reports the link, with the
// seeding's error and the command that retries it.
func TestConfirmedLinkSurvivesASeedingFailure(t *testing.T) {
	t.Setenv("CLAWBRIDGE_HOME", t.TempDir())
	service := linearboard.New(linearboard.Config{Executor: refusingExecutor{}})
	link := map[string]any{"revision": 1}
	got := linkWithSeedDocs(context.Background(), service, bus.Request{}, Config{}, linearFlags{confirm: true, profile: "lin_x"}, link).(map[string]any)
	seed, _ := got["seed_docs"].(map[string]any)
	if got["revision"] != 1 || seed["error"] == nil || seed["retry"] != "clawbridge linear seed-docs --confirm --profile lin_x" {
		t.Fatal(got)
	}
}

// --priority takes Linear's names or 0-4 on create and update, counts as
// an update's change, and a bad value is refused before any connection.
func TestLinearV2Priority(t *testing.T) {
	const a = "11111111-1111-4111-8111-111111111111"
	for _, c := range []struct {
		command, value string
		want           int
	}{{"create", "urgent", 1}, {"create", "Medium", 3}, {"update", "none", 0}, {"update", "4", 4}} {
		args := []string{"--profile", "native", "--operation-id", "p", "--priority", c.value}
		if c.command == "create" {
			args = append(args, "--title", "new")
		} else {
			args = append([]string{a}, args...)
		}
		v, pos, err := parseLinear(c.command, args, &bytes.Buffer{})
		if err != nil {
			t.Fatal(c, err)
		}
		input, err := linearWriteInput(c.command, v, pos)
		if err != nil || input.Priority == nil || *input.Priority != c.want {
			t.Fatalf("%v: %+v %v", c, input.Priority, err)
		}
	}
	v, pos, err := parseLinear("update", []string{a, "--profile", "native", "--operation-id", "p", "--title", "new"}, &bytes.Buffer{})
	if err != nil {
		t.Fatal(err)
	}
	if input, err := linearWriteInput("update", v, pos); err != nil || input.Priority != nil {
		t.Fatal("priority invented", input.Priority, err)
	}
	isolated(t)
	previous := newLinearManager
	t.Cleanup(func() { newLinearManager = previous })
	newLinearManager = func(string) *linearconn.Manager {
		t.Fatal("a bad priority touched the manager")
		return nil
	}
	for _, bad := range []string{"5", "critical", "-1", "1.5"} {
		var out, diag bytes.Buffer
		if err := Run(context.Background(), []string{"linear", "update", a, "--profile", "p", "--operation-id", "op", "--priority", bad}, &out, &diag); err == nil || !strings.Contains(err.Error(), "priority") {
			t.Fatal(bad, err)
		}
	}
}

// --due-date takes YYYY-MM-DD on create and update, --clear-due-date clears
// it on an update and counts as its change, and a bad value, or both flags,
// is refused before any connection.
func TestLinearV2DueDate(t *testing.T) {
	const a = "11111111-1111-4111-8111-111111111111"
	base := []string{"--profile", "native", "--operation-id", "p"}
	build := func(command string, extra ...string) (linearboard.WriteInputV2, error) {
		args := append(append([]string{}, base...), extra...)
		if command == "create" {
			args = append(args, "--title", "new")
		} else {
			args = append([]string{a}, args...)
		}
		v, pos, err := parseLinear(command, args, &bytes.Buffer{})
		if err != nil {
			return linearboard.WriteInputV2{}, err
		}
		return linearWriteInput(command, v, pos)
	}
	for _, command := range []string{"create", "update"} {
		input, err := build(command, "--due-date", "2026-11-14")
		if err != nil || !input.DueDate.Present || input.DueDate.Value == nil || *input.DueDate.Value != "2026-11-14" {
			t.Fatal(command, input.DueDate, err)
		}
	}
	// An update with only a due date, or only a clear, is a change.
	input, err := build("update", "--clear-due-date")
	if err != nil || !input.DueDate.Present || input.DueDate.Value != nil {
		t.Fatal("clear:", input.DueDate, err)
	}
	if input, err = build("update", "--title", "t"); err != nil || input.DueDate.Present {
		t.Fatal("due date invented", input.DueDate, err)
	}
	if _, err = build("update", "--due-date", "2026-11-14", "--clear-due-date"); err == nil {
		t.Fatal("both flags accepted")
	}
	if _, err = build("create", "--clear-due-date"); err == nil {
		t.Fatal("create took --clear-due-date")
	}
	for _, bad := range []string{"2026-02-30", "tomorrow", "2026-11-14T00:00:00Z", "1999-01-01", ""} {
		if _, err = build("update", "--due-date", bad); err == nil || !strings.Contains(err.Error(), "due date") {
			t.Fatal(bad, err)
		}
	}
}

// linear move takes the issue, --team, and on a confirm the operation ID
// and the preview's plan digest; its help touches nothing.
func TestLinearMoveParses(t *testing.T) {
	const issue = "11111111-1111-4111-8111-111111111111"
	const team = "22222222-2222-4222-8222-222222222222"
	sha := strings.Repeat("a", 64)
	v, pos, err := parseLinear("move", []string{issue, "--profile", "native", "--team", team, "--operation-id", "mv-1", "--confirm", "--expected-plan-sha256", sha}, &bytes.Buffer{})
	if err != nil || len(pos) != 1 || pos[0] != issue || v.team != team || v.operation != "mv-1" || !v.confirm || v.expectedSHA != sha {
		t.Fatal(v, pos, err)
	}
	if v, pos, err = parseLinear("move", []string{"--profile", "native", "--file", "moves.json", "--operation-id", "b-1"}, &bytes.Buffer{}); err != nil || len(pos) != 0 || v.moveFile != "moves.json" {
		t.Fatal("a batch didn't parse:", v, pos, err)
	}
	if v, _, err = parseLinear("move", []string{issue, "--profile", "native", "--team", team, "--family"}, &bytes.Buffer{}); err != nil || !v.family {
		t.Fatal("--family didn't parse:", err)
	}
	var out, diag bytes.Buffer
	if err = Run(context.Background(), []string{"linear", "move", "--help"}, &out, &diag); err != nil {
		t.Fatal(err)
	}
	if err = Run(context.Background(), []string{"linear", "--help"}, &out, &diag); err != nil || !strings.Contains(out.String(), "move ISSUE_UUID --team UUID") {
		t.Fatal(out.String(), err)
	}
}
