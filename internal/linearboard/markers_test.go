package linearboard

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"clawbridge/internal/bus"
	"clawbridge/internal/linearprovider"
)

const markerLabelID = "88888888-8888-4888-8888-888888888888"

const claimBody = "🤖 Claimed by ClawBridge agent session ses · ref cb-12345678\n\n— 🤖 ClawBridge agent · session ses"

func markerTest(t *testing.T) (*Service, *authorityV2, *providerV2, bus.Request) {
	t.Helper()
	s, a, p, scope := testV2(t)
	p.caps.LabelDeltas = true
	p.labels = []linearprovider.Label{{ID: markerLabelID, Name: bus.MarkerLabel, TeamID: teamID, EligibilityKnown: true}, {ID: labelA, Name: "bug", TeamID: teamID}}
	a.comments.gos = map[string]map[string]any{
		"comment": {"action": "post", "issue_uuid": issueID, "body": claimBody},
		"label":   {"action": "add", "issue_uuid": issueID, "label": bus.MarkerLabel},
	}
	return s, a, p, scope
}

func settled(a *authorityV2, part string) map[string]any {
	for _, s := range a.comments.markerSettles {
		if s["part"] == part {
			return s
		}
	}
	return nil
}

// The claim note is posted with the bus's exact text, outside the
// conversational ledger, and the label add is sent even when the label
// already reads as present.
func TestClaimMarkerPostsTheBusTextAndAlwaysSendsTheAdd(t *testing.T) {
	s, a, p, scope := markerTest(t)
	issue := p.issues[issueID]
	issue.LabelIDs, issue.LabelsKnown = []string{markerLabelID}, true
	p.issues[issueID] = issue
	out := s.ApplyMarker(context.Background(), scope, "asg_1", "claim", nil)
	if out.Comment != "done" || out.Label != "done" || out.Error != "" {
		t.Fatal(out)
	}
	if len(p.bodies) != 1 || p.bodies[0] != claimBody {
		t.Fatal("marker body not posted verbatim:", p.bodies)
	}
	if len(a.comments.reserves) != 0 {
		t.Fatal("a marker took a conversational reservation:", a.comments.reserves)
	}
	if c := settled(a, "comment"); c["outcome"] != "done" || c["comment_id"] != issueID || c["assignment_id"] != "asg_1" || c["phase"] != "claim" {
		t.Fatal("comment not settled with its receipt:", c)
	}
	if len(p.updated) != 1 || len(p.updated[0].AddLabels) != 1 || p.updated[0].AddLabels[0] != markerLabelID {
		t.Fatal("present label not added again:", p.updated)
	}
}

func TestCloseMarkerRemovesOnlyAPresentLabel(t *testing.T) {
	s, a, p, scope := markerTest(t)
	a.comments.gos["comment"] = map[string]any{"action": "none"}
	a.comments.gos["label"] = map[string]any{"action": "remove", "issue_uuid": issueID}
	issue := p.issues[issueID]
	issue.LabelIDs, issue.LabelsKnown = []string{}, true
	p.issues[issueID] = issue
	if out := s.ApplyMarker(context.Background(), scope, "asg_1", "close", nil); out.Label != "done" || len(p.updated) != 0 {
		t.Fatal("an absent label was removed:", out, p.updated)
	}
	issue.LabelIDs = []string{markerLabelID}
	p.issues[issueID] = issue
	if out := s.ApplyMarker(context.Background(), scope, "asg_1", "close", nil); out.Label != "done" || len(p.updated) != 1 || p.updated[0].RemoveLabels[0] != markerLabelID {
		t.Fatal("present label not removed:", out, p.updated)
	}
}

// Without exactly one agent-working label, nothing is guessed.
func TestMarkerLabelMissingOrAmbiguousIsNoLabel(t *testing.T) {
	for name, labels := range map[string][]linearprovider.Label{
		"missing":   {{ID: labelA, Name: "bug", TeamID: teamID}},
		"ambiguous": {{ID: markerLabelID, Name: bus.MarkerLabel, TeamID: teamID, EligibilityKnown: true}, {ID: labelA, Name: bus.MarkerLabel, TeamID: teamID, EligibilityKnown: true}},
	} {
		s, a, p, scope := markerTest(t)
		p.labels = labels
		out := s.ApplyMarker(context.Background(), scope, "asg_1", "claim", nil)
		if out.Label != "no_label" || len(p.updated) != 0 || settled(a, "label")["outcome"] != "no_label" {
			t.Fatal(name, out, p.updated)
		}
		if out.Comment != "done" {
			t.Fatal(name, "the claim note waited on the label:", out)
		}
	}
}

// A retry after an attempt that may have landed finds the exact body in
// that attempt's window and settles without posting again.
func TestMarkerRetryFindsTheEarlierPost(t *testing.T) {
	s, a, p, scope := markerTest(t)
	reserved := time.Now().Add(-20 * time.Minute).UTC()
	a.comments.gos["comment"] = map[string]any{"action": "post", "issue_uuid": issueID, "body": claimBody, "check_from": reserved.Add(-2 * time.Minute).Format(time.RFC3339Nano), "check_to": reserved.Add(10 * time.Minute).Format(time.RFC3339Nano)}
	a.comments.gos["label"] = map[string]any{"action": "none"}
	read, since, deadlines := []string{}, []string{}, []time.Time{}
	result := linearprovider.Comments{Complete: true, Items: []linearprovider.Comment{
		commentRow(humanComment, claimBody+" quoted", reserved.Add(time.Minute)),
		commentRow(ourComment, claimBody, reserved.Add(time.Minute)),
	}}
	c := commentingV2{providerV2: p, read: &read, since: &since, deadlines: &deadlines, result: &result}
	s.cfg.Open = func(context.Context, *http.Client, bool) (Provider, error) { return c, nil }
	out := s.ApplyMarker(context.Background(), scope, "asg_1", "claim", nil)
	if out.Comment != "done" || len(p.bodies) != 0 {
		t.Fatal("re-posted instead of finding the earlier post:", out, p.bodies)
	}
	if got := settled(a, "comment"); got["comment_id"] != ourComment || got["via"] != "body_match" {
		t.Fatal("settled with the wrong comment:", got)
	}
	// Outside the window the body is not ours: post again.
	result.Items = []linearprovider.Comment{commentRow(ourComment, claimBody, reserved.Add(15*time.Minute))}
	a.comments.markerSettles = nil
	if out = s.ApplyMarker(context.Background(), scope, "asg_1", "claim", nil); out.Comment != "done" || len(p.bodies) != 1 {
		t.Fatal("did not post after no match:", out, p.bodies)
	}
}

func TestMarkerFailuresSettleForRetry(t *testing.T) {
	s, a, p, scope := markerTest(t)
	p.issues[issueID] = linearprovider.IssueV2{Issue: linearprovider.Issue{ID: issueID, TeamID: teamID, ProjectID: projectB}, ProjectKnown: true}
	if out := s.ApplyMarker(context.Background(), scope, "asg_1", "claim", nil); out.Comment != "out_of_binding" || out.Label != "out_of_binding" {
		t.Fatal(out)
	}
	s, a, p, scope = markerTest(t)
	p.commentErr = errors.New("connection reset after send")
	a.comments.gos["label"] = map[string]any{"action": "none"}
	if out := s.ApplyMarker(context.Background(), scope, "asg_1", "claim", nil); out.Comment != "uncertain" || !strings.Contains(out.Error, "uncertain") {
		t.Fatal("an uncertain post was not left for the body check:", out)
	}
}

// A wait or nothing to do sends nothing.
func TestMarkerWaitSendsNothing(t *testing.T) {
	s, a, p, scope := markerTest(t)
	a.comments.gos["comment"] = map[string]any{"action": "wait"}
	a.comments.gos["label"] = map[string]any{"action": "none"}
	if out := s.ApplyMarker(context.Background(), scope, "asg_1", "close", nil); out.Comment != "wait" || out.Label != "none" || p.writes != 0 || len(a.comments.markerSettles) != 0 {
		t.Fatal(out)
	}
}

// The watcher applies due markers first, under its lease.
func TestWatchAppliesDueMarkersUnderTheLease(t *testing.T) {
	_, a, err := watchOnce(t, func(_ *Service, a *authorityV2) {
		a.comments.markersDue = []any{map[string]any{"assignment_id": "asg_9", "phase": "close", "issue_uuid": issueID}}
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(a.comments.markerGoes) != 2 || a.comments.markerGoes[0]["lease_token"] != "tok" || a.comments.markerGoes[0]["assignment_id"] != "asg_9" || a.comments.markerGoes[0]["phase"] != "close" {
		t.Fatal("due marker not applied under the lease:", a.comments.markerGoes)
	}
}
