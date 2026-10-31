package uiflow

import (
	"fmt"
	"reflect"
	"testing"

	"clawbridge/internal/linearboard"
)

func TestWriteGate(t *testing.T) {
	yes, no := true, false
	cases := []struct {
		name string
		gate WriteGate
		want bool
	}{
		{"local never blocks", WriteGate{Online: &no, Source: "offline-mirror"}, false},
		{"cloud offline blocks", WriteGate{Cloud: true, Online: &no}, true},
		{"cloud offline-mirror blocks", WriteGate{Cloud: true, Online: &yes, Source: "offline-mirror"}, true},
		{"cloud online does not", WriteGate{Cloud: true, Online: &yes, Source: "live"}, false},
		{"cloud unknown does not", WriteGate{Cloud: true}, false},
	}
	for _, c := range cases {
		if got := c.gate.Blocked(); got != c.want {
			t.Errorf("%s: Blocked = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestTypedMatches(t *testing.T) {
	if TypedMatches("", "") {
		t.Fatal("an empty want matched")
	}
	if !TypedMatches("alpha", "alpha") || TypedMatches("alpha", "Alpha") || TypedMatches("alpha", "alpha ") {
		t.Fatal("typed names must compare exactly")
	}
}

func TestTypedListMatches(t *testing.T) {
	want := []string{"Canceled", "Duplicate"}
	for csv, ok := range map[string]bool{
		"Canceled,Duplicate":     true,
		" Canceled , Duplicate ": true,
		"Duplicate,Canceled":     false,
		"Canceled,Duplicate,X":   false,
		"Canceled":               false,
		"canceled,Duplicate":     false,
		"":                       false,
	} {
		if got := TypedListMatches(want, csv); got != ok {
			t.Errorf("%q: got %v, want %v", csv, got, ok)
		}
	}
}

func TestToggleTeamCapsAt20(t *testing.T) {
	d := LinkDraft{}
	var err error
	for i := 0; i < 20; i++ {
		if d, err = d.ToggleTeam(fmt.Sprintf("team-%d", i)); err != nil {
			t.Fatal(err)
		}
	}
	over, err := d.ToggleTeam("team-x")
	if err == nil || err.Error() != "Select at most 20 teams." || len(over.Chosen) != 20 {
		t.Fatalf("over the cap: %v %v", over.Chosen, err)
	}
	back, err := d.ToggleTeam("team-3")
	if err != nil || len(back.Chosen) != 19 || has(back.Chosen, "team-3") {
		t.Fatalf("remove: %v %v", back.Chosen, err)
	}
}

func TestToggleProjectTeamBoardClearsProjects(t *testing.T) {
	d, err := LinkDraft{V2: true}.ToggleProject("p1")
	if err != nil || d.Project != "p1" || len(d.SelectedProjects) != 1 {
		t.Fatalf("first project: %+v %v", d, err)
	}
	if d, _ = d.ToggleProject("p2"); d.Project != "" || len(d.SelectedProjects) != 2 {
		t.Fatalf("two projects: %+v", d)
	}
	d, _ = d.ToggleProject("team-board")
	if !d.TeamBoard || d.SelectedProjects != nil || d.Project != "" {
		t.Fatalf("team board: %+v", d)
	}
	if d, _ = d.ToggleProject("p3"); d.TeamBoard || d.Project != "p3" {
		t.Fatalf("project after board: %+v", d)
	}
}

func TestToggleProjectCapsAt50(t *testing.T) {
	d := LinkDraft{V2: true, TeamBoard: true}
	for i := 0; i < 50; i++ {
		d, _ = d.ToggleProject(fmt.Sprintf("p%d", i))
	}
	over, err := d.ToggleProject("extra")
	if err == nil || err.Error() != "Select at most 50 projects." || len(over.SelectedProjects) != 50 {
		t.Fatalf("over the cap: %d %v", len(over.SelectedProjects), err)
	}
}

func TestPickProject(t *testing.T) {
	if d := (LinkDraft{V2: true}).PickProject("p1"); !reflect.DeepEqual(d.SelectedProjects, []string{"p1"}) || d.Project != "p1" {
		t.Fatalf("v2 pick: %+v", d)
	}
	if d := (LinkDraft{V2: true, SelectedProjects: []string{"a", "b"}}).PickProject("c"); len(d.SelectedProjects) != 2 || d.Project != "" {
		t.Fatalf("v2 keeps the chosen: %+v", d)
	}
	if d := (LinkDraft{}).PickProject("team-board"); !d.TeamBoard || d.Project != "" {
		t.Fatalf("v1 board: %+v", d)
	}
	if d := (LinkDraft{TeamBoard: true}).PickProject("p1"); d.TeamBoard || d.Project != "p1" {
		t.Fatalf("v1 project: %+v", d)
	}
}

func TestInputsV2Closes(t *testing.T) {
	d := LinkDraft{Chosen: []string{"t1"}, DefaultTeam: "t1", SelectedProjects: []string{"p1"}, V2: true, Closes: map[string]string{"p1": linearboard.ClosesAgent}}
	v1, v2 := d.Inputs(true, 7, true)
	if v2 == nil || v2.Via != linearboard.ClosesViaTUI || !v2.MinimalPolicy || !v2.Confirm || v2.ExpectedRevision != 7 || v1.ExpectedRevision != 7 {
		t.Fatalf("confirm: %+v", v2)
	}
	d.Closes["p1"] = "changed"
	if v2.Closes["p1"] != linearboard.ClosesAgent {
		t.Fatal("the closes map is shared with the draft")
	}
	if _, v2 = d.Inputs(false, 7, true); v2.MinimalPolicy {
		t.Fatal("a preview must not agree to the minimal policy")
	}
	d.TeamBoard = true
	if _, v2 = d.Inputs(true, 7, true); v2.Closes != nil || v2.Via != "" || v2.MinimalPolicy {
		t.Fatalf("team board sends no closes: %+v", v2)
	}
	if _, v2 = (LinkDraft{}).Inputs(true, 1, false); v2 != nil {
		t.Fatal("a V1 draft built a V2 input")
	}
}

func TestClosesToTypeStale(t *testing.T) {
	lines := []linearboard.ClosesLine{
		{LinearProject: "p1", Name: "One", Closes: "people", Set: linearboard.ClosesAgent},
		{LinearProject: "p2", Name: "Two", Closes: "agents"},
	}
	out, err := ClosesToType(lines, map[string]string{"p1": linearboard.ClosesAgent, "p2": linearboard.ClosesAgent})
	if err != nil || len(out) != 1 || out[0].LinearProject != "p1" {
		t.Fatalf("fresh: %v %v", out, err)
	}
	if _, err = ClosesToType(lines, map[string]string{"p1": linearboard.ClosesHuman}); err == nil {
		t.Fatal("a line set the other way was accepted")
	}
	if _, err = ClosesToType(lines, map[string]string{"p1": linearboard.ClosesAgent, "p3": linearboard.ClosesAgent}); err == nil || err.Error() != "the preview is out of date; reopen the link review. Nothing was changed" {
		t.Fatalf("a project not shown: %v", err)
	}
	if _, err = ClosesToType([]linearboard.ClosesLine{{LinearProject: "p4", Closes: "people"}}, map[string]string{"p4": linearboard.ClosesAgent}); err == nil {
		t.Fatal("an agent answer the preview does not show was accepted")
	}
}

func TestDraftMethodsDoNotAlias(t *testing.T) {
	d := LinkDraft{Chosen: []string{"t1", "t2"}, SelectedProjects: []string{"p1", "p2"}, Closes: map[string]string{"p1": linearboard.ClosesHuman}, V2: true}
	orig := LinkDraft{Chosen: []string{"t1", "t2"}, SelectedProjects: []string{"p1", "p2"}, Closes: map[string]string{"p1": linearboard.ClosesHuman}, V2: true}
	a, _ := d.ToggleTeam("t1")
	b, _ := d.ToggleProject("p1")
	c := d.ToggleCloses(linearboard.ClosesLine{LinearProject: "p1", Set: linearboard.ClosesHuman})
	e := d.PickProject("p9")
	v1, v2 := d.Inputs(true, 1, false)
	if !reflect.DeepEqual(d, orig) {
		t.Fatalf("a method changed its receiver: %+v", d)
	}
	a.Chosen[0], b.SelectedProjects[0], c.Closes["p1"] = "x", "x", "x"
	e.SelectedProjects = append(e.SelectedProjects, "x")
	v1.TeamIDs[0], v2.ProjectIDs[0] = "x", "x"
	if !reflect.DeepEqual(d, orig) {
		t.Fatalf("a result shares memory with the draft: %+v", d)
	}
}
