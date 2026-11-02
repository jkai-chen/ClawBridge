package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"clawbridge/internal/bus"
	"clawbridge/internal/clirender"
	"clawbridge/internal/linearboard"
)

const (
	cliMilestone = "f5000000-0000-4000-8000-0000000000a1"
	cliProject   = "e5000000-0000-4000-8000-0000000000e1"
)

// --milestone takes a milestone UUID on create and update; --clear-milestone
// is a change on an update but is refused by the write's own checks; a
// name, or both flags, is refused before any connection.
func TestLinearV2Milestone(t *testing.T) {
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
		input, err := build(command, "--milestone", cliMilestone)
		if err != nil || !input.Milestone.Present || input.Milestone.Value == nil || *input.Milestone.Value != cliMilestone {
			t.Fatal(command, input.Milestone, err)
		}
	}
	if input, err := build("update", "--title", "t"); err != nil || input.Milestone.Present {
		t.Fatal("milestone invented", input.Milestone, err)
	}
	// A clear is a change, and the write refuses it with its text.
	_, err := build("update", "--clear-milestone")
	if err == nil || err.Error() != "clearing a milestone isn't available yet; nothing was sent" {
		t.Fatal("clear:", err)
	}
	if _, err = build("update", "--milestone", cliMilestone, "--clear-milestone"); err == nil || !strings.Contains(err.Error(), "conflict") {
		t.Fatal("both flags accepted:", err)
	}
	if _, err = build("create", "--clear-milestone"); err == nil {
		t.Fatal("create took --clear-milestone")
	}
	for _, bad := range []string{"Sprint 1", "", "M-1"} {
		if _, err = build("update", "--milestone", bad); err == nil || !strings.Contains(err.Error(), "milestone UUID") {
			t.Fatal(bad, err)
		}
	}
}

func TestLinearMilestoneListParses(t *testing.T) {
	v, pos, err := parseLinear("milestone-list", []string{cliProject, "--profile", "native"}, &bytes.Buffer{})
	if err != nil || len(pos) != 1 || pos[0] != cliProject || v.profile != "native" {
		t.Fatal(v, pos, err)
	}
	if _, _, err = parseLinear("milestone-list", []string{"Website", "--profile", "native"}, &bytes.Buffer{}); err == nil || !strings.Contains(err.Error(), "project's UUID") {
		t.Fatal(err)
	}
	if _, _, err = parseLinear("milestone-list", []string{cliProject}, &bytes.Buffer{}); err == nil {
		t.Fatal("no profile accepted")
	}
	// The command word alone, or another action, is the usage.
	for _, args := range [][]string{{"milestone"}, {"milestone", "delete", cliProject}} {
		if err = runLinear(context.Background(), Config{}, args, &bytes.Buffer{}, &bytes.Buffer{}); err == nil || !strings.Contains(err.Error(), "usage: clawbridge linear milestone list PROJECT_UUID") {
			t.Fatal(args, err)
		}
	}
}

func milestoneFixture() linearboard.MilestoneListV2 {
	fifty, zero := 50, 0
	date := "2026-12-01"
	order := 1.0
	return linearboard.MilestoneListV2{Project: cliProject, Milestones: []linearboard.MilestoneV2{
		{ID: cliMilestone, Name: "Beta", TargetDate: &date, TargetDateKnown: true, ProgressKnown: true, Progress: &fifty, SortOrder: &order},
		{ID: "f5000000-0000-4000-8000-0000000000a2", Name: "Gamma", TargetDateKnown: true, ProgressKnown: true, Progress: &zero},
		{ID: "f5000000-0000-4000-8000-0000000000a3", Name: "Odd\x1b[2J"},
	}}
}

// The text view is a line per milestone: name, date or -, whole percent
// or ?, then the ID; a name is made safe for the terminal.
func TestLinearMilestoneListText(t *testing.T) {
	var out bytes.Buffer
	if err := clirender.Write(&out, clirender.Human, "linear_milestone_list", bus.Result{Source: "linear", Data: milestoneFixture()}); err != nil {
		t.Fatal(err)
	}
	want := strings.Join([]string{
		"Beta  2026-12-01  50%  " + cliMilestone,
		"Gamma  -  0%  f5000000-0000-4000-8000-0000000000a2",
		"Odd\\x1b[2J  ?  ?  f5000000-0000-4000-8000-0000000000a3",
	}, "\n") + "\n"
	if out.String() != want {
		t.Fatalf("got\n%s\nwant\n%s", out.String(), want)
	}
	// Control characters, a newline and a bidi override in a name are escaped.
	out.Reset()
	hostile := linearboard.MilestoneListV2{Project: cliProject, Milestones: []linearboard.MilestoneV2{{ID: cliMilestone, Name: "a\nb\tc\u202ed\x07", TargetDateKnown: true}}}
	_ = clirender.Write(&out, clirender.Human, "linear_milestone_list", bus.Result{Source: "linear", Data: hostile})
	if out.String() != "a↵b⇥c\\u202ed\\x07  -  ?  "+cliMilestone+"\n" {
		t.Fatalf("%q", out.String())
	}
	out.Reset()
	_ = clirender.Write(&out, clirender.Human, "linear_milestone_list", bus.Result{Source: "linear", Data: linearboard.MilestoneListV2{Project: cliProject, Milestones: []linearboard.MilestoneV2{}}})
	if out.String() != "No milestones.\n" {
		t.Fatalf("%q", out.String())
	}
}

// --json carries the neutral keys: id, name, target_date, progress (0-100
// or null) and sort_order, and no provider spelling.
func TestLinearMilestoneListJSON(t *testing.T) {
	var out bytes.Buffer
	if err := clirender.Write(&out, clirender.JSON, "linear_milestone_list", bus.Result{Source: "linear", Data: milestoneFixture()}); err != nil {
		t.Fatal(err)
	}
	var got struct {
		Source string `json:"source"`
		Data   struct {
			Project    string           `json:"project"`
			Milestones []map[string]any `json:"milestones"`
		} `json:"data"`
	}
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatal(err, out.String())
	}
	m := got.Data.Milestones
	if m[0]["target_date_known"] != true || m[0]["progress_known"] != true || m[1]["target_date_known"] != true || m[2]["target_date_known"] != false || m[2]["progress_known"] != false {
		t.Fatalf("%+v", m)
	}
	if got.Source != "linear" || got.Data.Project != cliProject || len(m) != 3 || m[0]["target_date"] != "2026-12-01" || m[0]["progress"] != float64(50) || m[0]["sort_order"] != float64(1) || m[1]["target_date"] != nil || m[2]["progress"] != nil {
		t.Fatalf("%+v", got)
	}
	for _, spelled := range []string{"projectMilestone", "targetDate", "sortOrder"} {
		if strings.Contains(out.String(), spelled) {
			t.Fatal("provider spelling in the output:", spelled)
		}
	}
}

// The command reads through the board and prints its answer.
func TestLinearMilestoneListRuns(t *testing.T) {
	scopeFixture(t)
	previous := milestoneRead
	t.Cleanup(func() { milestoneRead = previous })
	var asked string
	milestoneRead = func(_ *linearboard.Service, _ context.Context, _ bus.Request, project string) (linearboard.MilestoneListV2, error) {
		asked = project
		return milestoneFixture(), nil
	}
	var out bytes.Buffer
	if err := runLinear(clirender.WithMode(context.Background(), clirender.Human), Config{Project: "p"}, []string{"milestone", "list", cliProject, "--profile", "prof-1"}, &out, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	if asked != cliProject || !strings.Contains(out.String(), "Beta  2026-12-01  50%") {
		t.Fatal(asked, out.String())
	}
}
