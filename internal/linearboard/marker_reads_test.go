package linearboard

import (
	"context"
	"net/http"
	"testing"
)

// A claim marker is one held operation: one connection, one binding proof,
// one issue read and one label page, whatever its two parts need. On
// native Linear that is tools/list, get_team and list_projects per bound
// team, get_issue, list_issue_labels, plus the comment's and the label's
// readbacks: 7 reads for a one-team binding.
func TestMarkerClaimReadCounts(t *testing.T) {
	s, _, p, scope := markerTest(t)
	var n readCounts
	opens := 0
	s.cfg.Open = func(context.Context, *http.Client, bool) (Provider, error) { opens++; return countingV2{p, &n}, nil }
	out := s.ApplyMarker(context.Background(), scope, "asg_1", "claim", nil)
	if out.Comment != "done" || out.Label != "done" || p.writes != 2 {
		t.Fatal(out, p.writes)
	}
	t.Logf("opens=%d reads=%+v", opens, n)
	if want := (readCounts{labelPages: 1, teams: 2, projects: 2, issues: 1, capabilities: 2}); opens != 1 || n != want {
		t.Fatalf("opens %d reads %+v, want 1 and %+v", opens, n, want)
	}
}
