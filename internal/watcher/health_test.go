package watcher

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"clawbridge/internal/bus"
)

func linearProjectID(n int) string { return fmt.Sprintf("00000000-0000-4000-8000-%012d", n) }

// bindProjects binds bus project slug to n Linear projects and enables
// health on each, all due now.
func (e *env) bindProjects(slug string, rev, n int) {
	e.t.Helper()
	ids := []string{}
	for i := 0; i < n; i++ {
		ids = append(ids, linearProjectID(i+1))
	}
	do := func(op string, a map[string]any) {
		if _, err := e.s.Execute(context.Background(), bus.Request{Operation: op, Workspace: "w", Project: slug, Args: a}); err != nil {
			e.t.Fatal(op, err)
		}
	}
	do("taskboard_set_v2", map[string]any{"expected_revision": rev, "mode": "project_set", "linear_team_ids": []string{team}, "linear_default_team_id": team, "linear_project_ids": ids})
	for _, id := range ids {
		do("health_set", map[string]any{"linear_project": id, "expected_revision": 0, "health_policy": map[string]any{"enabled": true}})
	}
	e.healthDue()
}

func (e *env) healthDue() {
	e.t.Helper()
	e.sql(`UPDATE project_health SET next_health_at='2000-01-01T00:00:00.000000000Z'`)
}

func healthIssue(id, identifier, state string, updated time.Time, blockedBy string) map[string]any {
	rel := []any{}
	if blockedBy != "" {
		rel = append(rel, map[string]any{"type": "blocks", "issue": map[string]any{"state": map[string]any{"type": blockedBy}}})
	}
	return map[string]any{"id": id, "identifier": identifier, "updatedAt": updated.UTC().Format(time.RFC3339Nano), "state": map[string]any{"type": state}, "inverseRelations": map[string]any{"nodes": rel}}
}

func activity(historyActor string, commentUser string, at time.Time) map[string]any {
	stamp := at.UTC().Format(time.RFC3339Nano)
	comments := []any{}
	if commentUser != "" {
		comments = append(comments, map[string]any{"createdAt": stamp, "user": map[string]any{"id": commentUser}, "botActor": nil})
	}
	return map[string]any{
		"history":  map[string]any{"nodes": []any{map[string]any{"createdAt": stamp, "actorId": historyActor, "botActor": nil}, map[string]any{"createdAt": "2020-01-01T00:00:00.000Z", "actorId": trustedUser}}},
		"comments": map[string]any{"nodes": comments},
	}
}

// A run reads facts as the app, never writes, and the coordinator hears
// only a health two runs agree on. The app's own activity is not human
// activity (P9); a human comment is.
func TestHealthRunReadsFactsAndNotifiesOnTheSecondRun(t *testing.T) {
	e := setup(t)
	e.bindProjects("p", 1, 1)
	now := time.Now()
	e.f.healthIssues = []map[string]any{
		healthIssue(issueA, "FIX-1", "started", now.Add(-10*24*time.Hour), ""),
		healthIssue(issueB, "FIX-2", "started", now, ""),
		healthIssue(issueOut, "FIX-3", "started", now, ""),
		healthIssue(humanNote, "FIX-4", "unstarted", now, "started"),
		healthIssue(labelNeeds, "FIX-5", "unstarted", now, "completed"),
		healthIssue(strangerID, "FIX-6", "completed", now, ""),
	}
	e.f.activity = map[string]map[string]any{
		// Only the app touched FIX-2: its note and label.
		issueB: activity(appUser, appUser, now.Add(-time.Hour)),
		// A person commented on FIX-3.
		issueOut: activity(appUser, trustedUser, now.Add(-time.Hour)),
	}
	tick := e.tick()
	if len(tick.Health) != 1 || tick.Health[0].Health != "offTrack" || tick.Health[0].Acted != "" || tick.Health[0].Calls != 4 {
		t.Fatal("first run:", tick.Health, tick.Error)
	}
	if len(e.inbox("coord", "project-health")) != 0 {
		t.Fatal("a single run notified")
	}
	if again := e.tick(); len(again.Health) != 0 {
		t.Fatal("ran again before its slot:", again.Health)
	}
	e.healthDue()
	if tick = e.tick(); len(tick.Health) != 1 || tick.Health[0].Acted != "changed" {
		t.Fatal("second run:", tick.Health)
	}
	notes := e.inbox("coord", "project-health")
	body := fmt.Sprint(notes[0]["body"])
	if len(notes) != 1 || !strings.Contains(body, "Off track") || !strings.Contains(body, "2 of 3 started issues") || !strings.Contains(body, "FIX-1, FIX-2") || strings.Contains(body, "FIX-3") || !strings.Contains(body, "1 open issues are blocked") {
		t.Fatal(notes)
	}
	for _, q := range e.f.queries {
		if strings.Contains(q, "mutation") {
			t.Fatal("health wrote to Linear:", q)
		}
	}
}

// HP-M3: with 20 enabled Linear projects over two bus projects, all due,
// a tick runs at most HealthRunsPerTick of them.
func TestHealthRunsAreCappedPerTickAcrossProjects(t *testing.T) {
	e := setup(t)
	if _, err := e.s.Execute(context.Background(), bus.Request{Operation: "project_create", Workspace: "w", Args: map[string]any{"slug": "q"}}); err != nil {
		t.Fatal(err)
	}
	e.bindProjects("q", 0, 10)
	e.bindProjects("p", 1, 10)
	e.f.healthIssues = []map[string]any{healthIssue(issueA, "FIX-1", "unstarted", time.Now(), "")}
	runs := 0
	for _, tick := range e.w.Tick(context.Background()) {
		if tick.Error != "" {
			t.Fatal(tick.ProjectID, tick.Error)
		}
		runs += len(tick.Health)
	}
	if runs != HealthRunsPerTick {
		t.Fatal("health runs in one tick:", runs)
	}
}

// A project with more issues than a run reads is not judged on part of
// them, and is tried again later.
func TestHealthTooManyIssuesIsAFailureNotAPartialHealth(t *testing.T) {
	e := setup(t)
	e.bindProjects("p", 1, 1)
	e.f.healthIssues = []map[string]any{healthIssue(issueA, "FIX-1", "unstarted", time.Now(), "")}
	e.f.healthMore = true
	tick := e.tick()
	if len(tick.Health) != 1 || tick.Health[0].Error != "too_many_issues" || tick.Health[0].Calls != 1+bus.HealthIssuePages {
		t.Fatal(tick.Health)
	}
	if len(e.inbox("coord", "project-health")) != 0 {
		t.Fatal("a partial read notified")
	}
}
