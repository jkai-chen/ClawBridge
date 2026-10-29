package doctor

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// Doctor shows each team listed without a project as inert or as a stale
// entry, adds no check when none is listed, and warns when it can't read.
func TestTeamsWithoutProjectCheck(t *testing.T) {
	run := func(teams []TeamWithoutProject, err error) []Check {
		var got []Check
		checkTeamsWithoutProject(context.Background(), func(context.Context, string, string) ([]TeamWithoutProject, error) { return teams, err }, "w", "p", func(c Check) { got = append(got, c) })
		return got
	}
	if got := run(nil, nil); len(got) != 0 {
		t.Fatal("a check for a binding listing none:", got)
	}
	got := run([]TeamWithoutProject{{Label: "AI: no project yet (inert)", Inert: true}}, nil)
	if len(got) != 1 || got[0].Status != Pass || got[0].Message != "AI: no project yet (inert)." || got[0].Action != "" {
		t.Fatal(got)
	}
	got = run([]TeamWithoutProject{{Label: "AI: no project yet (inert)", Inert: true}, {Label: "Ops: a bound project carries it (stale entry; the next link drops it)"}}, nil)
	if len(got) != 1 || got[0].Status != Pass || !strings.Contains(got[0].Message, "AI: no project yet (inert); Ops: a bound project carries it (stale entry") || !strings.Contains(got[0].Action, "drops its entry") {
		t.Fatal(got)
	}
	if got = run(nil, errors.New("boom")); len(got) != 1 || got[0].Status != Warn {
		t.Fatal(got)
	}
}
