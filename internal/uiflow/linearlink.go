package uiflow

import (
	"errors"

	"clawbridge/internal/linearboard"
)

// LinkDraft is what a person has chosen so far on the Linear link steps.
// Its methods return a new draft and never share slices or maps with the
// receiver.
type LinkDraft struct {
	Chosen           []string
	DefaultTeam      string
	SelectedProjects []string
	Project          string
	TeamBoard        bool
	Closes           map[string]string
	V2               bool
}

// LinearPrevious is the step Esc goes back to from each step.
var LinearPrevious = map[string]string{
	"teams": "profiles", "default": "teams", "projects": "default", "preview": "projects",
	"tasks": "profiles", "details": "tasks", "unlink": "tasks", "intake": "tasks", "status": "tasks", "views": "tasks", "roles": "tasks", "cancel": "tasks",
	"writes": "profiles",
}

func (d LinkDraft) clone() LinkDraft {
	if d.Chosen != nil {
		d.Chosen = append([]string{}, d.Chosen...)
	}
	if d.SelectedProjects != nil {
		d.SelectedProjects = append([]string{}, d.SelectedProjects...)
	}
	if d.Closes != nil {
		closes := make(map[string]string, len(d.Closes))
		for id, who := range d.Closes {
			closes[id] = who
		}
		d.Closes = closes
	}
	return d
}

func has(ids []string, id string) bool {
	for _, v := range ids {
		if v == id {
			return true
		}
	}
	return false
}

func without(ids []string, id string) []string {
	out := []string{}
	for _, v := range ids {
		if v != id {
			out = append(out, v)
		}
	}
	return out
}

// ToggleTeam adds the team to the allowed ones, or removes it. At most 20
// teams; past that the draft is returned unchanged with an error.
func (d LinkDraft) ToggleTeam(id string) (LinkDraft, error) {
	d = d.clone()
	if has(d.Chosen, id) {
		d.Chosen = without(d.Chosen, id)
		return d, nil
	}
	if len(d.Chosen) >= 20 {
		return d, errors.New("Select at most 20 teams.")
	}
	d.Chosen = append(d.Chosen, id)
	return d, nil
}

// ToggleProject is Space on the projects step of a V2 link. "team-board"
// links the whole board and clears the projects. At most 50 projects;
// past that the returned draft has already left team-board, as the TUI
// always did, and carries the error.
func (d LinkDraft) ToggleProject(id string) (LinkDraft, error) {
	d = d.clone()
	if id == "team-board" {
		d.TeamBoard = true
		d.SelectedProjects = nil
		d.Project = ""
		return d, nil
	}
	d.TeamBoard = false
	if has(d.SelectedProjects, id) {
		d.SelectedProjects = without(d.SelectedProjects, id)
	} else {
		if len(d.SelectedProjects) >= 50 {
			return d, errors.New("Select at most 50 projects.")
		}
		d.SelectedProjects = append(d.SelectedProjects, id)
	}
	d.Project = ""
	if len(d.SelectedProjects) == 1 {
		d.Project = d.SelectedProjects[0]
	}
	return d, nil
}

// PickProject is Enter on the projects step. In V2 it chooses the
// highlighted project only when nothing is chosen yet; in V1 it chooses
// it as the one project, or the team board.
func (d LinkDraft) PickProject(id string) LinkDraft {
	d = d.clone()
	if d.V2 {
		if len(d.SelectedProjects) == 0 && !d.TeamBoard {
			if id == "team-board" {
				d.TeamBoard = true
			} else {
				d.SelectedProjects = []string{id}
			}
		}
		d.Project = ""
		if len(d.SelectedProjects) == 1 {
			d.Project = d.SelectedProjects[0]
		}
		return d
	}
	d.TeamBoard = id == "team-board"
	d.Project = ""
	if !d.TeamBoard {
		d.Project = id
	}
	return d
}

// ToggleCloses switches the project of the preview line between people
// and agents.
func (d LinkDraft) ToggleCloses(l linearboard.ClosesLine) LinkDraft {
	d = d.clone()
	now := l.Set
	if now == "" {
		now = linearboard.ClosesHuman
		if l.Closes == "agents" {
			now = linearboard.ClosesAgent
		}
	}
	next := linearboard.ClosesAgent
	if now == linearboard.ClosesAgent {
		next = linearboard.ClosesHuman
	}
	if d.Closes == nil {
		d.Closes = map[string]string{}
	}
	d.Closes[l.LinearProject] = next
	return d
}

// Inputs builds what the link sends: the V1 input always, the V2 one only
// for a V2 draft. Closes go along unless it is a team board, marked as
// confirmed in the TUI; the minimal policy is agreed to only when the
// link is confirmed and minimal says it would create one.
func (d LinkDraft) Inputs(confirm bool, revision int64, minimal bool) (linearboard.LinkInput, *linearboard.LinkInputV2) {
	d = d.clone()
	v1 := linearboard.LinkInput{TeamIDs: append([]string{}, d.Chosen...), DefaultTeamID: d.DefaultTeam, ProjectID: d.Project, ExpectedRevision: revision, TeamBoard: d.TeamBoard, Confirm: confirm}
	if !d.V2 {
		return v1, nil
	}
	v2 := &linearboard.LinkInputV2{TeamIDs: append([]string{}, d.Chosen...), DefaultTeamID: d.DefaultTeam, ProjectIDs: append([]string{}, d.SelectedProjects...), ExpectedRevision: revision, TeamBoard: d.TeamBoard, Confirm: confirm}
	if len(d.Closes) > 0 && !d.TeamBoard {
		v2.Closes = d.Closes
		// Only a confirmation with agents in it has the person's typed
		// names behind it.
		v2.Via = linearboard.ClosesViaTUI
		v2.MinimalPolicy = confirm && minimal
	}
	return v1, v2
}

// ClosesToType is every project the link would let agents close that they
// don't already: each needs its name typed. An answer the shown preview
// doesn't reflect means the preview is stale, and nothing goes.
func ClosesToType(lines []linearboard.ClosesLine, closes map[string]string) ([]linearboard.ClosesLine, error) {
	shown := map[string]linearboard.ClosesLine{}
	for _, l := range lines {
		shown[l.LinearProject] = l
	}
	var out []linearboard.ClosesLine
	for _, l := range lines {
		want := closes[l.LinearProject]
		switch {
		case want == linearboard.ClosesAgent && l.Set == linearboard.ClosesAgent:
			out = append(out, l)
		case want == linearboard.ClosesAgent && l.Closes == "agents":
		case want == linearboard.ClosesAgent || l.Set != "" && l.Set != want:
			return nil, errors.New("the preview is out of date; press c again or reopen the link review. Nothing was changed")
		}
	}
	for id, who := range closes {
		if _, ok := shown[id]; !ok && who == linearboard.ClosesAgent {
			return nil, errors.New("the preview is out of date; reopen the link review. Nothing was changed")
		}
	}
	return out, nil
}
