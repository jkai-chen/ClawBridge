package tui

import (
	"reflect"
	"testing"

	"clawbridge/internal/bus"
	"clawbridge/internal/uiflow"
)

func TestLinkDraftRoundTrip(t *testing.T) {
	m := Model{}
	m.linear.bindingV2 = &bus.TaskboardBindingV2{}
	want := uiflow.LinkDraft{
		Chosen: []string{"team-a", "team-b"}, DefaultTeam: "team-a",
		SelectedProjects: []string{"proj-1", "proj-2"}, Project: "proj-1", TeamBoard: true,
		Closes: map[string]string{"proj-1": "agent", "proj-2": "human"}, V2: true,
	}
	m.setLinkDraft(want)
	got := m.linkDraft()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("round trip lost a field:\n got %+v\nwant %+v", got, want)
	}
	if m.linear.defaultTeam != "team-a" || m.linear.project != "proj-1" || !m.linear.teamBoard || len(m.linear.closes) != 2 {
		t.Fatalf("the model's own fields were not set: %+v", m.linear)
	}
	got.Chosen[0], got.Closes["proj-1"] = "x", "x"
	if m.linear.chosen[0] != "team-a" || m.linear.closes["proj-1"] != "agent" {
		t.Fatal("linkDraft shares memory with the model")
	}
}
