package bus

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"
	"time"
)

// webhooksLive turns the gate on, configures teams three hours ago and
// records a delivery two hours ago for each of delivered, so those teams
// are live.
func (h *harness) webhooksLive(configured []string, delivered ...string) {
	h.t.Helper()
	ctx := context.Background()
	now := time.Now().UTC()
	h.s.SetWebhooksProven(true)
	if err := h.s.WebhookTeamsConfigured(ctx, configured, now.Add(-3*time.Hour)); err != nil {
		h.t.Fatal(err)
	}
	if err := h.s.WebhookGateOn(ctx, now.Add(-3*time.Hour)); err != nil {
		h.t.Fatal(err)
	}
	for _, team := range delivered {
		h.record(issueUpdate("sha256:live-"+team, team, now.Add(-2*time.Hour)))
	}
}

// addBoundTeam adds team to pid's binding's write teams.
func (h *harness) addBoundTeam(pid, team string) {
	h.t.Helper()
	var raw string
	if err := h.s.db.QueryRow(`SELECT mapping FROM taskboard_bindings WHERE project_id=?`, pid).Scan(&raw); err != nil {
		h.t.Fatal(err)
	}
	m := map[string]any{}
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		h.t.Fatal(err)
	}
	m["linear_team_ids"] = append(m["linear_team_ids"].([]any), team)
	b, _ := json.Marshal(m)
	if _, err := h.s.db.Exec(`UPDATE taskboard_bindings SET mapping=? WHERE project_id=?`, string(b), pid); err != nil {
		h.t.Fatal(err)
	}
}

// intakeWait sets an intake cursor from a read that started a second ago
// and returns its wait.
func (h *webhookHarness) intakeWait(source, subject string, signal bool) time.Duration {
	h.t.Helper()
	polled := time.Now().UTC().Add(-time.Second)
	if err := h.s.IntakeCursorSet(context.Background(), h.pid, source, subject, layout(polled), signal); err != nil {
		h.t.Fatal(err)
	}
	next, _ := h.cursorState(h.pid, source, subject)
	return at(next).Sub(polled)
}

// §6 P4: S1 waits an hour while its team is live, today's cadence
// otherwise.
func TestLiveTeamCadenceHourly(t *testing.T) {
	h := newWebhookHarness(t)
	h.addBoundTeam(h.pid, whTeamB)
	if d := h.intakeWait("intake-s1", sfTeam, false); d != 15*time.Minute {
		t.Fatal("new source:", d)
	}
	if d := h.intakeWait("intake-s1", sfTeam, false); d != 15*time.Minute {
		t.Fatal("not live:", d)
	}
	h.webhooksLive([]string{sfTeam, whTeamB}, sfTeam)
	if d := h.intakeWait("intake-s1", sfTeam, false); d != time.Hour {
		t.Fatal("live:", d)
	}
	if d := h.intakeWait("intake-s1", sfTeam, true); d != time.Hour {
		t.Fatal("live, with a signal:", d)
	}
	h.intakeWait("intake-s1", whTeamB, false)
	if d := h.intakeWait("intake-s1", whTeamB, true); d != 5*time.Minute {
		t.Fatal("another team, not live:", d)
	}
}

// Q14: subject app sources relax only when every bound write team is
// live.
func TestAppSourcesRelaxOnlyWhenAllTeamsLive(t *testing.T) {
	h := newWebhookHarness(t)
	h.addBoundTeam(h.pid, whTeamB)
	h.webhooksLive([]string{sfTeam, whTeamB}, sfTeam)
	for _, src := range []string{"intake-s2", "intake-s3"} {
		h.intakeWait(src, "app", false)
		if d := h.intakeWait(src, "app", false); d != 15*time.Minute {
			t.Fatal(src, "one team live:", d)
		}
	}
	h.record(issueUpdate("sha256:live-b", whTeamB, time.Now().UTC().Add(-time.Hour)))
	for _, src := range []string{"intake-s2", "intake-s3"} {
		if d := h.intakeWait(src, "app", false); d != time.Hour {
			t.Fatal(src, "every team live:", d)
		}
	}
	// A project with no bound team is never live.
	tx, err := h.s.beginIntake(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if live, err := h.s.webhookProjectLiveTx(context.Background(), tx, h.pid+"-none", time.Now().UTC()); err != nil || live {
		t.Fatal("no binding:", live, err)
	}
}

// ownComment records a session's own comment on watchIssue now, which
// holds the issue on the 5-minute cadence.
func (w *watchHarness) ownComment(digest string) {
	w.t.Helper()
	if _, e := w.s.db.Exec(`INSERT INTO linear_comment_ledger(workspace_id,project_id,operation_digest,issue_uuid,body_sha256,session_id,principal,state,reserved_at) SELECT workspace_id,id,?,?,'x','ses','p','posted',? FROM projects WHERE id=(SELECT project_id FROM watch_cursors WHERE source='comments' AND subject=? LIMIT 1)`, digest, watchIssue, layout(time.Now()), watchIssue); e != nil {
		w.t.Fatal(e)
	}
}

func (w *watchHarness) commentWait() time.Duration {
	w.t.Helper()
	w.dueNow()
	out := w.poll(watchIssue)
	return time.Until(at(out["next_poll_at"].(string)))
}

// Comment cursors carry no team: they relax to hourly only when every
// bound write team of the project is live.
func TestCommentCadenceHourlyWhenAllLive(t *testing.T) {
	w := newWatchHarness(t)
	w.link(0, []string{cpOne})
	pid := w.slugID("p")
	w.addBoundTeam(pid, whTeamB)
	w.ownComment("own-1")
	if d := w.commentWait(); d > 6*time.Minute {
		t.Fatal("not live:", d)
	}
	w.webhooksLive([]string{sfTeam, whTeamB}, sfTeam)
	if d := w.commentWait(); d > 6*time.Minute {
		t.Fatal("one team live:", d)
	}
	w.record(issueUpdate("sha256:live-b", whTeamB, time.Now().UTC().Add(-time.Hour)))
	if d := w.commentWait(); d < 59*time.Minute {
		t.Fatal("every team live:", d)
	}
}

// §6 P5: a miss degrades the team and puts it back on today's cadence for
// 24 hours.
func TestMissCountedAndDegrades(t *testing.T) {
	h := newWebhookHarness(t)
	ctx := context.Background()
	h.webhooksLive([]string{sfTeam}, sfTeam)
	h.intakeWait("intake-s1", sfTeam, false)
	if d := h.intakeWait("intake-s1", sfTeam, false); d != time.Hour {
		t.Fatal("live:", d)
	}
	now := time.Now().UTC()
	if counted, err := h.s.WebhookMiss(ctx, sfTeam, now.Add(-30*time.Minute), now); err != nil || !counted {
		t.Fatal(counted, err)
	}
	if team, _ := h.teamStatus(); team["state"] != "degraded" || team["missed_24h"] != 1 {
		t.Fatal(team)
	}
	for _, src := range [][2]string{{"intake-s1", sfTeam}, {"intake-s2", "app"}} {
		h.intakeWait(src[0], src[1], false)
		if d := h.intakeWait(src[0], src[1], false); d != 15*time.Minute {
			t.Fatal(src, "degraded:", d)
		}
	}
	// 24 hours after the miss, hourly again.
	if _, err := h.s.db.Exec(`UPDATE webhook_teams SET last_missed_at=?`, layout(now.Add(-webhookDegrade-time.Minute))); err != nil {
		t.Fatal(err)
	}
	if d := h.intakeWait("intake-s1", sfTeam, false); d != time.Hour {
		t.Fatal("a day after the miss:", d)
	}
}

// The gate off changes no cadence, whatever the teams' state.
func TestGateOffChangesNoCadence(t *testing.T) {
	h := newWebhookHarness(t)
	h.webhooksLive([]string{sfTeam}, sfTeam)
	h.s.SetWebhooksProven(false)
	for _, src := range [][2]string{{"intake-s1", sfTeam}, {"intake-s2", "app"}, {"intake-s3", "app"}} {
		h.intakeWait(src[0], src[1], false)
		if d := h.intakeWait(src[0], src[1], false); d != 15*time.Minute {
			t.Fatal(src, d)
		}
		if d := h.intakeWait(src[0], src[1], true); d != 5*time.Minute {
			t.Fatal(src, "signal:", d)
		}
	}
	if n, err := h.s.WebhookCatchUp(context.Background(), time.Now().UTC()); err != nil || len(n) != 0 {
		t.Fatal("a catch-up with the gate off:", n, err)
	}

	w := newWatchHarness(t)
	w.link(0, []string{cpOne})
	w.ownComment("own-off")
	w.webhooksLive([]string{sfTeam}, sfTeam)
	w.s.SetWebhooksProven(false)
	if d := w.commentWait(); d > 6*time.Minute {
		t.Fatal("comments:", d)
	}
}

// §6 P6: a drain's end marks every live team's cursors due once, and
// never cuts a back-off short.
func TestDrainEndMarksLiveTeamsDue(t *testing.T) {
	h := newWebhookHarness(t)
	ctx := context.Background()
	h.addBoundTeam(h.pid, whTeamB)
	h.webhooksLive([]string{sfTeam, whTeamB}, sfTeam)
	for _, c := range [][2]string{{"intake-s1", sfTeam}, {"intake-s1", whTeamB}, {"intake-s2", "app"}, {"intake-s3", "app"}, {"comments", whIssue}, {"comments", whIssueOff}} {
		h.cursor(h.pid, c[0], c[1], "")
	}
	if _, err := h.s.db.Exec(`UPDATE watch_cursors SET failures=2 WHERE source='comments' AND subject=?`, whIssueOff); err != nil {
		t.Fatal(err)
	}
	// Another project, bound to a team that isn't live, is left alone.
	if _, err := h.s.Execute(ctx, Request{Operation: "taskboard_set_v2", Workspace: "w", Project: "listed", Args: map[string]any{"expected_revision": 0, "mode": "project_set", "linear_team_ids": []string{whTeamB}, "linear_default_team_id": whTeamB, "linear_project_ids": []string{cpTwo}}}); err != nil {
		t.Fatal(err)
	}
	other := h.slugID("listed")
	h.cursor(other, "intake-s1", whTeamB, "")

	now := time.Now().UTC()
	if err := h.s.WebhookDrainEnded(ctx, now); err != nil {
		t.Fatal(err)
	}
	got, err := h.s.WebhookCatchUp(ctx, now)
	if err != nil || !reflect.DeepEqual(got, []string{h.pid}) {
		t.Fatal(got, err)
	}
	for _, c := range [][2]string{{"intake-s1", sfTeam}, {"intake-s2", "app"}, {"intake-s3", "app"}, {"comments", whIssue}} {
		if next, _ := h.cursorState(h.pid, c[0], c[1]); next != layout(now) {
			t.Fatal(c, "not due:", next)
		}
	}
	for _, c := range [][2]string{{"intake-s1", whTeamB}, {"comments", whIssueOff}} {
		if next, _ := h.cursorState(h.pid, c[0], c[1]); next != whFar {
			t.Fatal(c, "marked:", next)
		}
	}
	if next, _ := h.cursorState(other, "intake-s1", whTeamB); next != whFar {
		t.Fatal("another project marked:", next)
	}
}

// reportComment polls watchIssue once, reporting one human comment created
// at created and the issue's team.
func (w *watchHarness) reportComment(id string, created time.Time, team string) map[string]any {
	w.t.Helper()
	w.dueNow()
	w.call("comment_watch_due", "c", w.fenced(map[string]any{}))
	return w.call("comment_watch_report", "c", w.fenced(map[string]any{"issue_uuid": watchIssue, "team_id": team, "comments": []any{comment(id, "", created)}})).(map[string]any)
}

// §6 P5 for comments: a notified comment more than 10 minutes old, while
// its issue's team is live, is a miss for that team, which puts the
// project's comment watch back on today's cadence. A young one, or one
// with the gate off, is not; nor is a team outside the binding.
func TestCommentMissCounted(t *testing.T) {
	w := newWatchHarness(t)
	w.link(0, []string{cpOne})
	w.webhooksLive([]string{sfTeam}, sfTeam)
	// Comments since the watch started count; start it an hour ago.
	if _, err := w.s.db.Exec(`UPDATE watch_cursors SET watch_started_at=? WHERE source='comments'`, layout(time.Now().UTC().Add(-time.Hour))); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	out := w.reportComment(humanOne, now.Add(-2*time.Minute), sfTeam)
	if len(out["notified"].([]string)) != 1 || w.webhookCountOf(sfTeam, "missed") != 0 {
		t.Fatal("a young comment:", out, w.webhookCountOf(sfTeam, "missed"))
	}
	if d := time.Until(at(out["next_poll_at"].(string))); d < 59*time.Minute {
		t.Fatal("live cadence:", d)
	}
	// A team outside the binding is never charged.
	w.reportComment("00000000-0000-4000-8000-0000000000c3", now.Add(-30*time.Minute), whTeamB)
	if w.webhookCountOf(whTeamB, "missed") != 0 || w.webhookCountOf(sfTeam, "missed") != 0 {
		t.Fatal("an unbound team was charged")
	}
	w.s.SetWebhooksProven(false)
	w.reportComment("00000000-0000-4000-8000-0000000000c4", now.Add(-30*time.Minute), sfTeam)
	if w.webhookCountOf(sfTeam, "missed") != 0 {
		t.Fatal("counted with the gate off")
	}
	w.s.SetWebhooksProven(true)
	out = w.reportComment(humanTwo, now.Add(-30*time.Minute), sfTeam)
	if len(out["notified"].([]string)) != 1 || w.webhookCountOf(sfTeam, "missed") != 1 {
		t.Fatal("an old comment:", out, w.webhookCountOf(sfTeam, "missed"))
	}
	if live, err := w.s.WebhookTeamLive(context.Background(), sfTeam, time.Now().UTC()); err != nil || live {
		t.Fatal("still live:", live, err)
	}
	if d := time.Until(at(out["next_poll_at"].(string))); d > 6*time.Minute {
		t.Fatal("the cadence stayed hourly after the miss:", d)
	}
	// A report without a team counts nothing, and is still accepted.
	w.reportComment("00000000-0000-4000-8000-0000000000c5", now.Add(-30*time.Minute), "")
	if w.webhookCountOf(sfTeam, "missed") != 1 {
		t.Fatal("a report without a team counted")
	}
}
