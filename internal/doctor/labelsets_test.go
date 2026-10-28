package doctor

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestCheckLabelSets(t *testing.T) {
	run := func(sets []LabelSet, err error) Check {
		var got Check
		checkLabelSets(context.Background(), func(context.Context) ([]LabelSet, error) { return sets, err }, func(c Check) { got = c })
		return got
	}
	const sub = "abababab-abab-4bab-8bab-abababababab"
	c := run([]LabelSet{{Team: sub, Name: "needs-agent", Scopes: []string{"team", "parent"}}, {Team: sub, Name: "agent-working", Scopes: []string{"parent"}, Apply: true}}, nil)
	if c.ID != "linear.labels" || c.Status != Pass || !strings.Contains(c.Message, "abababab needs-agent: team+parent") || !strings.Contains(c.Message, "agent-working: parent") || !strings.Contains(c.Message, "hourly") {
		t.Fatal(c)
	}
	for name, sets := range map[string][]LabelSet{
		"no needs-agent":  {{Team: sub, Name: "needs-agent"}},
		"none to apply":   {{Team: sub, Name: "agent-working", Scopes: []string{"team"}}},
		"two in one team": {{Team: sub, Name: "needs-agent", Err: "two in one team"}},
	} {
		if c = run(sets, nil); c.Status != Warn || c.Action == "" {
			t.Fatal(name, c)
		}
	}
	if c = run(nil, errors.New("down")); c.Status != Warn {
		t.Fatal(c)
	}
}
