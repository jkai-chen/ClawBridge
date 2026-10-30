package watcher

import (
	"context"
	"testing"
	"time"

	"clawbridge/internal/bus"
	"clawbridge/internal/linearprovider"
)

// A webhook pass (FR14) runs the comment watch, then intake, under the
// project's lease, and nothing else: no markers, subscribers, health,
// documents or dispatcher, and it isn't the project's tick.
func TestWebhookPassSkipsHealthAndDocs(t *testing.T) {
	e := setup(t)
	proposed := e.call("assignment_propose", "eng", map[string]any{"issue_uuid": issueA, "issue_identifier": "FIX-3"})
	e.call("assignment_confirm", "coord", map[string]any{"target_session": "eng", "revision": proposed["assignment"].(map[string]any)["revision"]})
	e.call("intake_trust_add", "coord", map[string]any{"linear_user_id": trustedUser})
	if err := e.w.Webhook(context.Background(), e.pid); err != nil {
		t.Fatal("first pass:", err)
	}
	var ticks int
	e.queryRow(`SELECT COUNT(*) FROM watch_ticks`, &ticks)
	if ticks != 0 {
		t.Fatal("a webhook pass counted as the project's tick")
	}
	if e.f.notes != 0 {
		t.Fatal("a webhook pass applied a claim marker:", e.f.notes)
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	e.f.comments = []linearprovider.Comment{{ID: humanNote, AuthorKind: "unknown", CreatedAt: now, UpdatedAt: now, UntrustedBody: "a question"}}
	e.f.delegated = []string{issueB}
	e.f.history[issueB] = []map[string]any{entry(now, trustedUser, map[string]any{"toDelegate": map[string]any{"id": appUser}})}
	e.dueNow()
	e.f.events = nil
	tick := e.w.webhook(context.Background(), e.pid)
	if tick.Error != "" || tick.Comments == nil || len(tick.Comments.Notified) != 1 {
		t.Fatalf("the comment watch did not run: %+v %+v", tick, tick.Comments)
	}
	if tick.Intake == nil || len(tick.Intake.Queued) != 1 {
		t.Fatalf("intake did not run: %+v", tick.Intake)
	}
	if tick.Health != nil || tick.Docs != nil || tick.Subscribers != nil || tick.Dispatch != nil || len(tick.Comments.Markers) != 0 || e.f.notes != 0 {
		t.Fatalf("a webhook pass ran more than comments and intake: %+v notes=%d", tick, e.f.notes)
	}
	for _, ev := range e.f.events {
		if ev != "comments" {
			t.Fatal("a webhook pass read more than comments:", e.f.events)
		}
	}
	e.queryRow(`SELECT COUNT(*) FROM watch_ticks`, &ticks)
	if ticks != 0 {
		t.Fatal("a webhook pass counted as the project's tick")
	}
	// The claim marker was due all along: a tick applies it.
	e.tick()
	if e.f.notes == 0 {
		t.Fatal("the marker was not due")
	}
}

// §6 P5: a queued candidate whose transition is older than ten minutes,
// for a team live at that time, is a miss for the issue's team.
func TestIntakeQueuedCountsAMiss(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	start := time.Now().UTC().Add(-3 * time.Hour)
	e.s.SetWebhooksProven(true)
	if err := e.s.WebhookTeamsConfigured(ctx, []string{team}, start); err != nil {
		t.Fatal(err)
	}
	if err := e.s.WebhookGateOn(ctx, start); err != nil {
		t.Fatal(err)
	}
	if _, err := e.s.WebhookRecord(ctx, bus.WebhookDelivery{DeliveryID: "sha256:first", TeamID: team, EventType: "Issue", Action: "update", EntityID: issueA, LinearAt: start.Add(time.Hour), ReceivedAt: start.Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	e.call("intake_trust_add", "coord", map[string]any{"linear_user_id": trustedUser})
	e.tick()
	old := time.Now().UTC().Add(-30 * time.Minute).Format(time.RFC3339Nano)
	young := time.Now().UTC().Add(-time.Minute).Format(time.RFC3339Nano)
	e.f.delegated = []string{issueA, issueB}
	e.f.history[issueA] = []map[string]any{entry(old, trustedUser, map[string]any{"toDelegate": map[string]any{"id": appUser}})}
	e.f.history[issueB] = []map[string]any{entry(young, trustedUser, map[string]any{"toDelegate": map[string]any{"id": appUser}})}
	e.dueNow()
	if tick := e.tick(); tick.Intake == nil || len(tick.Intake.Queued) != 2 {
		t.Fatalf("intake: %+v", tick.Intake)
	}
	var missed int
	e.queryRow(`SELECT COALESCE(SUM(n),0) FROM webhook_counts WHERE reason='missed' AND team_id='`+team+`'`, &missed)
	if missed != 1 {
		t.Fatal("misses:", missed)
	}
	if live, err := e.s.WebhookTeamLive(ctx, team, time.Now().UTC()); err != nil || live {
		t.Fatal("a team with a miss is still live:", live, err)
	}
	// A repeat finds the issues already queued: no second miss.
	e.dueNow()
	e.tick()
	e.queryRow(`SELECT COALESCE(SUM(n),0) FROM webhook_counts WHERE reason='missed'`, &missed)
	if missed != 1 {
		t.Fatal("misses after a repeat:", missed)
	}
}

// The comment poll's own issue read names the team: a notified comment
// more than 10 minutes old, while that team is live, is its miss.
func TestCommentPollCountsAMissForTheIssueTeam(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	start := time.Now().UTC().Add(-3 * time.Hour)
	e.s.SetWebhooksProven(true)
	if err := e.s.WebhookTeamsConfigured(ctx, []string{team}, start); err != nil {
		t.Fatal(err)
	}
	if err := e.s.WebhookGateOn(ctx, start); err != nil {
		t.Fatal(err)
	}
	if _, err := e.s.WebhookRecord(ctx, bus.WebhookDelivery{DeliveryID: "sha256:first", TeamID: team, EventType: "Issue", Action: "update", EntityID: issueA, LinearAt: start.Add(time.Hour), ReceivedAt: start.Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	proposed := e.call("assignment_propose", "eng", map[string]any{"issue_uuid": issueA, "issue_identifier": "FIX-3"})
	e.call("assignment_confirm", "coord", map[string]any{"target_session": "eng", "revision": proposed["assignment"].(map[string]any)["revision"]})
	e.tick()
	e.sql(`UPDATE watch_cursors SET watch_started_at=? WHERE source='comments'`, start.Format("2006-01-02T15:04:05.000000000Z07:00"))
	old := time.Now().UTC().Add(-30 * time.Minute).Format(time.RFC3339Nano)
	e.f.comments = []linearprovider.Comment{{ID: humanNote, AuthorKind: "unknown", CreatedAt: old, UpdatedAt: old, UntrustedBody: "a question"}}
	e.dueNow()
	if tick := e.w.webhook(ctx, e.pid); tick.Comments == nil || len(tick.Comments.Notified) != 1 {
		t.Fatalf("not notified: %+v", tick.Comments)
	}
	var missed int
	e.queryRow(`SELECT COALESCE(SUM(n),0) FROM webhook_counts WHERE reason='missed' AND team_id='`+team+`'`, &missed)
	if missed != 1 {
		t.Fatal("comment misses:", missed)
	}
}
