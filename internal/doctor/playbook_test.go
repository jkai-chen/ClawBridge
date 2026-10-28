package doctor

import (
	"context"
	"errors"
	"strings"
	"testing"

	"clawbridge/internal/playbook"
)

// Only the coordinator gets the playbook check; it passes when the default
// branch has docs/COORDINATOR.md, and warns, never fails, otherwise.
func TestCheckPlaybook(t *testing.T) {
	config := Config{Workspace: "w", Project: "p", Session: "coord", Instance: "inst-1"}
	overview := func(name string) map[string]any {
		return map[string]any{"coordinator": map[string]any{"session_id": "ses_1", "name": name}}
	}
	cases := []struct {
		name   string
		data   map[string]any
		err    error
		state  playbook.State
		status string
		words  string
	}{
		{"not the coordinator", overview("api-1"), nil, playbook.State{}, "", ""},
		{"no coordinator", map[string]any{"coordinator": nil}, nil, playbook.State{}, "", ""},
		{"overview refused", nil, errors.New("forbidden"), playbook.State{}, "", ""},
		{"merged", overview("coord"), nil, playbook.State{Root: "/r", Ref: "origin/main", Merged: true, Local: true}, Pass, "is on origin/main"},
		{"only here", overview("coord"), nil, playbook.State{Root: "/r", Ref: "origin/main", Local: true}, Warn, "not on origin/main"},
		{"none", overview("coord"), nil, playbook.State{Root: "/r", Ref: "origin/main"}, Warn, "runs on the skill's defaults"},
		{"no default branch", overview("coord"), nil, playbook.State{Root: "/r", Err: errors.New("no origin/HEAD")}, Warn, "default branch is unknown"},
		{"outside a checkout", overview("coord"), nil, playbook.State{}, Warn, "Not in a git checkout"},
	}
	for _, tc := range cases {
		q := &queueCounts{data: tc.data, err: tc.err}
		var got []Check
		checkPlaybook(context.Background(), func() playbook.State { return tc.state }, config, q, func(c Check) { got = append(got, c) })
		if q.request.Operation != "project_overview" || q.request.Session != "coord" || q.request.Args["instance"] != "inst-1" {
			t.Fatalf("%s: read %+v", tc.name, q.request)
		}
		if tc.status == "" {
			if len(got) != 0 {
				t.Fatalf("%s: %+v", tc.name, got)
			}
			continue
		}
		if len(got) != 1 || got[0].ID != "coordinator.playbook" || got[0].Status != tc.status || !strings.Contains(got[0].Message, tc.words) {
			t.Fatalf("%s: %+v", tc.name, got)
		}
		if tc.status == Warn && got[0].Action == "" {
			t.Fatalf("%s: a warning with no action", tc.name)
		}
	}
	// Without a session there is nothing to ask.
	q := &queueCounts{}
	checkPlaybook(context.Background(), func() playbook.State { return playbook.State{} }, Config{Workspace: "w", Project: "p"}, q, func(Check) { t.Fatal("checked without a session") })
	if q.request.Operation != "" {
		t.Fatal("read without a session")
	}
}
