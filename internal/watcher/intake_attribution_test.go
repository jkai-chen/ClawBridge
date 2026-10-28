package watcher

import (
	"context"
	"strings"
	"testing"
	"time"

	"clawbridge/internal/bus"
	"clawbridge/internal/linearprovider"
)

const (
	mentionComment = "dddddddd-dddd-4ddd-8ddd-dddddddddd01"
	issueC         = "77777777-7777-4777-8777-777777777777"
)

// attributed sets up trustedUser as trusted and linked to the workspace
// owner, and starts every intake cursor.
func attributed(t *testing.T) (*env, string) {
	t.Helper()
	e := setup(t)
	owner := e.call("whoami", "", map[string]any{})["principal"].(string)
	e.call("intake_trust_add", "coord", map[string]any{"linear_user_id": trustedUser, "principal": owner})
	if e.f.extra == nil {
		e.f.extra = map[string]linearprovider.IssueV2{}
	}
	e.f.extra[issueC] = linearprovider.IssueV2{Issue: linearprovider.Issue{ID: issueC, Identifier: "FIX-7", Title: "title 7", TeamID: team}, ProjectKnown: true, DescriptionKnown: true, UpdatedAt: time.Now().UTC().Format(time.RFC3339Nano)}
	e.tick()
	return e, owner
}

// record is the server's record of an app write that added needs-agent to
// issue, or posted comment on it.
func (e *env) record(issue, owner, session, comment string) {
	e.t.Helper()
	w := bus.AppWrite{Workspace: "w", Project: "p", Issue: issue, Identity: bus.Identity{Principal: owner, Issuer: bus.DeviceIssuer, Subject: "dev_owner"}, Session: session}
	if comment != "" {
		w.Comment = comment
	} else {
		w.Labels = []string{labelNeeds}
	}
	if session != "" {
		w.AgentMarker = "CLAUDECODE"
	}
	if err := e.s.RecordAppWrite(context.Background(), w); err != nil {
		e.t.Fatal(err)
	}
}

func (e *env) caps() map[string]string {
	e.t.Helper()
	caps := map[string]string{}
	for _, row := range e.queue() {
		caps[row["issue_uuid"].(string)] = row["trigger_cap"].(string)
	}
	return caps
}

// S1 through the app: Linear names the app, and the one write in the
// window is the owner's own, so it is the trusted person's signal. A1: when
// an agent's session also added the label in the window, neither counts.
// A write outside the window is not seen (A5).
func TestIntakeS1CreditsTheOwnersWriteThroughTheApp(t *testing.T) {
	e, owner := attributed(t)
	now := time.Now().UTC()
	e.f.labelled = []string{issueA, issueB, issueC}
	byApp := func(at time.Time) []map[string]any {
		return []map[string]any{entry(at.Format(time.RFC3339Nano), appUser, map[string]any{"addedLabelIds": []string{labelNeeds}})}
	}
	e.f.history[issueA] = byApp(now)
	e.f.history[issueB] = byApp(now)
	e.f.history[issueC] = byApp(now.Add(5 * time.Minute))
	e.record(issueA, owner, "", "")
	e.record(issueB, owner, "", "")
	e.record(issueB, owner, "eng", "")
	e.record(issueC, owner, "", "")
	e.dueNow()
	tick := e.tick()
	if tick.Intake == nil || len(tick.Intake.Queued) != 3 || tick.Intake.Skipped["attribution_ambiguous"] != 1 {
		t.Fatalf("intake: %+v", tick.Intake)
	}
	if caps := e.caps(); caps[issueA] != "auto" || caps[issueB] != "notify" || caps[issueC] != "notify" {
		t.Fatal("caps:", caps)
	}
}

// Only the app's own transitions look for an attribution: a person Linear
// names is that person, whatever the app wrote around then.
func TestIntakeAttributionOnlyForTheApp(t *testing.T) {
	e, owner := attributed(t)
	now := time.Now().UTC().Format(time.RFC3339Nano)
	e.f.labelled = []string{issueA}
	e.f.history[issueA] = []map[string]any{entry(now, strangerID, map[string]any{"addedLabelIds": []string{labelNeeds}})}
	e.record(issueA, owner, "", "")
	e.dueNow()
	if tick := e.tick(); tick.Intake == nil || len(tick.Intake.Queued) != 1 {
		t.Fatalf("intake: %+v", tick.Intake)
	}
	if caps := e.caps(); caps[issueA] != "notify" {
		t.Fatal("a stranger's label was credited to the owner:", caps)
	}
	// S3 reads the mention's comment, to match it to the app write.
	if !strings.Contains(mentionsQuery, "comment { id }") {
		t.Fatal("the mention query does not read the comment:", mentionsQuery)
	}
}

// A label added just after the create leaves no history entry: the one
// write in the poll window stands for it, when it is the owner's own and
// the label is proven on the issue by ID; with none, the issue is skipped
// as before.
func TestIntakeS1WithoutATransitionUsesTheAttribution(t *testing.T) {
	e, owner := attributed(t)
	e.f.labelled = []string{issueA, issueB, issueC}
	e.f.issueLabels = map[string][]string{issueA: {labelNeeds}, issueB: {labelNeeds}, issueC: {labelNeeds}}
	e.record(issueA, owner, "", "")
	e.record(issueC, owner, "eng", "")
	e.dueNow()
	tick := e.tick()
	if tick.Intake == nil || len(tick.Intake.Queued) != 1 || tick.Intake.Skipped["transition_not_found"] != 1 || tick.Intake.Skipped["attributed_untrusted"] != 1 {
		t.Fatalf("intake: %+v", tick.Intake)
	}
	if caps := e.caps(); caps[issueA] != "auto" {
		t.Fatal("caps:", caps)
	}
}

// M1: with no transition, credit needs the label on the issue by ID now.
// Labels the app can't read, or a list without it (removed since the
// labelled read), credit nothing; a board read that carries label IDs is
// used as it stands.
func TestIntakeS1WithoutATransitionNeedsTheLabelByID(t *testing.T) {
	e, owner := attributed(t)
	e.f.labelled = []string{issueA, issueB, issueC}
	// issueA's list runs past one page, issueB's lacks the label, and
	// issueC's board read carries label IDs without it.
	e.f.issueLabels = map[string][]string{issueA: {labelNeeds}, issueB: {"cccccccc-cccc-4ccc-8ccc-cccccccccccc"}}
	e.f.labelsMore = true
	e.f.extra[issueC] = linearprovider.IssueV2{Issue: linearprovider.Issue{ID: issueC, Identifier: "FIX-7", Title: "title 7", TeamID: team}, ProjectKnown: true, DescriptionKnown: true, LabelsKnown: true, LabelIDs: []string{}, UpdatedAt: time.Now().UTC().Format(time.RFC3339Nano)}
	for _, issue := range []string{issueA, issueB, issueC} {
		e.record(issue, owner, "", "")
	}
	e.dueNow()
	tick := e.tick()
	if tick.Intake == nil || len(tick.Intake.Queued) != 0 || tick.Intake.Skipped["transition_not_found"] != 3 {
		t.Fatalf("credited without the label proven by ID: %+v", tick.Intake)
	}
	// The board's own label IDs, carrying the label, are enough.
	e.f.extra[issueC] = linearprovider.IssueV2{Issue: linearprovider.Issue{ID: issueC, Identifier: "FIX-7", Title: "title 7", TeamID: team}, ProjectKnown: true, DescriptionKnown: true, LabelsKnown: true, LabelIDs: []string{labelNeeds}, UpdatedAt: time.Now().UTC().Format(time.RFC3339Nano)}
	e.dueNow()
	if tick = e.tick(); len(tick.Intake.Queued) != 1 || e.caps()[issueC] != "auto" {
		t.Fatalf("intake: %+v %v", tick.Intake, e.caps())
	}
}

// S3 through the app: the mention's comment is the owner's own write.
func TestIntakeS3CreditsTheOwnersCommentThroughTheApp(t *testing.T) {
	e, owner := attributed(t)
	now := time.Now().UTC().Format(time.RFC3339Nano)
	e.f.mentions = []map[string]any{
		{"id": "n1", "type": "issueCommentMention", "createdAt": now, "issue": map[string]any{"id": issueA}, "actor": map[string]any{"id": appUser}, "comment": map[string]any{"id": mentionComment}},
		{"id": "n2", "type": "issueCommentMention", "createdAt": now, "issue": map[string]any{"id": issueB}, "actor": map[string]any{"id": appUser}},
	}
	e.record(issueA, owner, "", mentionComment)
	e.dueNow()
	tick := e.tick()
	if tick.Intake == nil || len(tick.Intake.Queued) != 2 {
		t.Fatalf("intake: %+v", tick.Intake)
	}
	if caps := e.caps(); caps[issueA] != "auto" || caps[issueB] != "notify" {
		t.Fatal("caps:", caps)
	}
}

// W2 (CROSS-TEAM-BINDING §3.2): with no transition, the label-on proof
// accepts any ID of the team's needs-agent set, so a sub-team issue read by
// its own copy still counts when it now carries the parent's label; the
// credit stays by the written label's ID, and a label outside the set
// proves nothing.
func TestIntakeAttributionLabelOnTakesTheSet(t *testing.T) {
	const subTeam = "abababab-abab-4bab-8bab-abababababab"
	const copyNeeds = "cdcdcdcd-cdcd-4dcd-8dcd-cdcdcdcdcdcd"
	e, owner := attributed(t)
	e.call("taskboard_set_v2", "coord", map[string]any{"expected_revision": 1, "mode": "team_board", "linear_team_ids": []string{team, subTeam}, "linear_default_team_id": team, "linear_project_ids": []string{}})
	needs := func(id, t string) linearprovider.Label {
		return linearprovider.Label{ID: id, Name: needsAgentLabel, TeamID: t, EligibilityKnown: true}
	}
	e.f.parents = map[string]string{subTeam: team}
	e.f.teamLabels = map[string][]linearprovider.Label{team: {needs(labelNeeds, team)}, subTeam: {needs(copyNeeds, subTeam)}}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	for _, id := range []string{issueA, issueB} {
		e.f.extra[id] = linearprovider.IssueV2{Issue: linearprovider.Issue{ID: id, Identifier: "SUB-" + id[:1], Title: "t", TeamID: subTeam}, ProjectKnown: true, DescriptionKnown: true, UpdatedAt: now}
	}
	// issueC's board read carries label IDs: the parent's.
	e.f.extra[issueC] = linearprovider.IssueV2{Issue: linearprovider.Issue{ID: issueC, Identifier: "SUB-C", Title: "t", TeamID: subTeam}, ProjectKnown: true, DescriptionKnown: true, LabelsKnown: true, LabelIDs: []string{labelNeeds}, UpdatedAt: now}
	e.f.labelled = []string{issueA, issueB, issueC}
	e.f.carries = map[string][]string{issueA: {copyNeeds}, issueB: {copyNeeds}, issueC: {copyNeeds}}
	// Both now carry another label than the one read: issueA the parent's
	// needs-agent, issueB a label outside the set.
	e.f.issueLabels = map[string][]string{issueA: {labelNeeds}, issueB: {"cccccccc-cccc-4ccc-8ccc-cccccccccccc"}}
	for _, issue := range []string{issueA, issueB, issueC} {
		w := bus.AppWrite{Workspace: "w", Project: "p", Issue: issue, Identity: bus.Identity{Principal: owner, Issuer: bus.DeviceIssuer, Subject: "dev_owner"}, Labels: []string{copyNeeds}}
		if err := e.s.RecordAppWrite(context.Background(), w); err != nil {
			t.Fatal(err)
		}
	}
	e.tick()
	e.dueNow()
	tick := e.tick()
	if tick.Intake == nil || len(tick.Intake.Queued) != 2 || tick.Intake.Skipped["transition_not_found"] != 1 {
		t.Fatalf("intake: %+v", tick.Intake)
	}
	if caps := e.caps(); caps[issueA] != "auto" || caps[issueC] != "auto" {
		t.Fatal("caps:", caps)
	}
}
