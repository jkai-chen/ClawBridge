package server

import (
	"errors"
	"strings"
	"testing"

	"clawbridge/internal/bus"
)

const (
	pstStarted  = "f1000000-0000-4000-8000-0000000000a1"
	pstStarted2 = "f1000000-0000-4000-8000-0000000000a2"
	pstPlanned  = "f1000000-0000-4000-8000-0000000000a3"
)

// A type maps to the workspace's one unarchived status of it; none or
// several is ambiguous unless status_id names one; a long list maps
// nothing.
func TestProjectStatusTarget(t *testing.T) {
	f := projectStatusFacts{Statuses: []ProjectStatus{{pstPlanned, "Planned", "planned"}, {pstStarted, "Doing", "started"}, {pstStarted2, "Doing too", "started"}}}
	if got, e := projectStatusTarget(f, "planned", ""); e != nil || got.ID != pstPlanned {
		t.Fatal(got, e)
	}
	for _, c := range []struct{ typ, id, want string }{
		{"started", "", "several"},
		{"completed", "", "no unarchived"},
		{"started", pstPlanned, "not an unarchived project status of type started"},
	} {
		if _, e := projectStatusTarget(f, c.typ, c.id); e == nil || !strings.Contains(e.Error(), c.want) {
			t.Fatal(c, e)
		}
	}
	if got, e := projectStatusTarget(f, "started", pstStarted2); e != nil || got.ID != pstStarted2 {
		t.Fatal(got, e)
	}
	f.More = true
	if _, e := projectStatusTarget(f, "planned", ""); e == nil || !strings.Contains(e.Error(), "more than 50") {
		t.Fatal(e)
	}
}

// --suggest is today's rule over a full read; a partial one proposes
// nothing (L5).
func TestSuggestProjectStatus(t *testing.T) {
	for want, n := range map[string]projectIssues{
		"backlog":   {Complete: true},
		"planned":   {Total: 2, Open: 1, ByType: map[string]int{"unstarted": 1, "completed": 1}, Complete: true},
		"started":   {Total: 2, Open: 1, ByType: map[string]int{"completed": 1, "started": 1}, Complete: true},
		"completed": {Total: 2, ByType: map[string]int{"completed": 1, "canceled": 1}, Complete: true},
		"":          {Total: 500, ByType: map[string]int{"completed": 500}},
	} {
		if n.ByType == nil {
			n.ByType = map[string]int{}
		}
		if got := suggestProjectStatus(n); got != want {
			t.Fatal(want, got)
		}
	}
	// All canceled is not done: it suggests canceled.
	if got := suggestProjectStatus(projectIssues{Total: 1, ByType: map[string]int{"canceled": 1}, Complete: true}); got != "canceled" {
		t.Fatal(got)
	}
}

// A project status refusal keeps its words through HTTP; any other error
// is untouched.
func TestPolicyRefusalKeepsTheWords(t *testing.T) {
	e := policyRefusal(bus.PublicError("forbidden", "people close Linear project X"))
	if code, _ := serviceError(e); code != "invalid" || responseMessage(code, e) != "forbidden: people close Linear project X" {
		t.Fatal(code, e)
	}
	plain := errors.New("database down")
	if policyRefusal(plain) != plain || policyRefusal(nil) != nil {
		t.Fatal("changed another error")
	}
	other := bus.PublicError("conflict", "x")
	if policyRefusal(other) != other {
		t.Fatal("changed a conflict")
	}
}
