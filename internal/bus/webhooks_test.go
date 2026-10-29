package bus

import (
	"context"
	"errors"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

const (
	whTeamB    = "22222222-2222-4222-8222-222222222222"
	whUnbound  = "99999999-9999-4999-8999-999999999999"
	whIssue    = "00000000-0000-4000-8000-0000000000e1"
	whIssueOff = "00000000-0000-4000-8000-0000000000e2"
	whFar      = "2999-01-01T00:00:00.000000000Z"
)

// webhookHarness binds project p to sfTeam.
type webhookHarness struct {
	*harness
	pid string
}

func newWebhookHarness(t *testing.T) *webhookHarness {
	t.Helper()
	h := newHarness(t)
	h.link(0, []string{cpOne})
	return &webhookHarness{harness: h, pid: h.slugID("p")}
}

func (h *harness) slugID(slug string) string {
	h.t.Helper()
	var pid string
	if err := h.s.db.QueryRow(`SELECT id FROM projects WHERE slug=?`, slug).Scan(&pid); err != nil {
		h.t.Fatal(err)
	}
	return pid
}

// cursor adds a watch cursor due far in the future; activeUntil "" is an
// active one.
func (h *harness) cursor(pid, source, subject, activeUntil string) {
	h.t.Helper()
	if _, err := h.s.db.Exec(`INSERT INTO watch_cursors(project_id,source,subject,cursor_time,watch_started_at,active_until,next_poll_at,updated_at) VALUES(?,?,?,?,?,?,?,?)`, pid, source, subject, stamp(), stamp(), nullable(activeUntil), whFar, stamp()); err != nil {
		h.t.Fatal(err)
	}
}

func (h *harness) cursorState(pid, source, subject string) (next, hinted string) {
	h.t.Helper()
	var hint *string
	if err := h.s.db.QueryRow(`SELECT next_poll_at,hinted_at FROM watch_cursors WHERE project_id=? AND source=? AND subject=?`, pid, source, subject).Scan(&next, &hint); err != nil {
		h.t.Fatal(err)
	}
	if hint != nil {
		hinted = *hint
	}
	return next, hinted
}

func (h *harness) webhookCountOf(team, reason string) int {
	h.t.Helper()
	var n int
	if err := h.s.db.QueryRow(`SELECT COALESCE(SUM(n),0) FROM webhook_counts WHERE team_id=? AND reason=?`, team, reason).Scan(&n); err != nil {
		h.t.Fatal(err)
	}
	return n
}

func (h *harness) record(d WebhookDelivery) WebhookResult {
	h.t.Helper()
	r, err := h.s.WebhookRecord(context.Background(), d)
	if err != nil {
		h.t.Fatal(err)
	}
	return r
}

func issueUpdate(id, team string, at time.Time) WebhookDelivery {
	return WebhookDelivery{DeliveryID: id, TeamID: team, WebhookID: "wh-1", EventType: "Issue", Action: "update", EntityID: whIssue, UpdatedKeys: []string{"stateId"}, LinearAt: at, ReceivedAt: at}
}

func commentCreate(id, team, issue string, at time.Time) WebhookDelivery {
	return WebhookDelivery{DeliveryID: id, TeamID: team, WebhookID: "wh-1", EventType: "Comment", Action: "create", EntityID: "00000000-0000-4000-8000-0000000000f1", IssueID: issue, LinearAt: at, ReceivedAt: at}
}

func TestWebhookRecordDedups(t *testing.T) {
	h := newWebhookHarness(t)
	now := time.Now().UTC()
	if r := h.record(issueUpdate("sha256:aa", sfTeam, now)); r.Outcome != "inert" {
		t.Fatal("first:", r)
	}
	if r := h.record(issueUpdate("sha256:aa", sfTeam, now)); r.Outcome != "duplicate" || len(r.Projects) != 0 {
		t.Fatal("second:", r)
	}
	var rows int
	if err := h.s.db.QueryRow(`SELECT COUNT(*) FROM webhook_deliveries`).Scan(&rows); err != nil || rows != 1 {
		t.Fatal("rows:", rows, err)
	}
	if h.webhookCountOf(sfTeam, "duplicate") != 1 {
		t.Fatal("duplicate not counted")
	}
}

func TestWebhookInertMarksNothing(t *testing.T) {
	h := newWebhookHarness(t)
	h.cursor(h.pid, "intake-s1", sfTeam, "")
	h.cursor(h.pid, "intake-s2", "app", "")
	r := h.record(issueUpdate("sha256:inert", sfTeam, time.Now().UTC()))
	if r.Outcome != "inert" || len(r.Projects) != 0 {
		t.Fatal(r)
	}
	for _, src := range [][2]string{{"intake-s1", sfTeam}, {"intake-s2", "app"}} {
		if next, hinted := h.cursorState(h.pid, src[0], src[1]); next != whFar || hinted != "" {
			t.Fatal(src, next, hinted)
		}
	}
	if h.webhookCountOf(sfTeam, "inert") != 1 {
		t.Fatal("inert not counted")
	}
}

func TestWebhookIssueUpdateMarksS1AndS2(t *testing.T) {
	h := newWebhookHarness(t)
	h.s.SetWebhooksProven(true)
	h.cursor(h.pid, "intake-s1", sfTeam, "")
	h.cursor(h.pid, "intake-s2", "app", "")
	h.cursor(h.pid, "intake-s3", "app", "")
	now := time.Now().UTC()
	r := h.record(issueUpdate("sha256:upd", sfTeam, now))
	if r.Outcome != "applied" || !reflect.DeepEqual(r.Projects, []string{h.pid}) {
		t.Fatal(r)
	}
	for _, src := range [][2]string{{"intake-s1", sfTeam}, {"intake-s2", "app"}} {
		if next, hinted := h.cursorState(h.pid, src[0], src[1]); next != layout(now) || hinted != layout(now) {
			t.Fatal(src, next, hinted)
		}
	}
	if next, _ := h.cursorState(h.pid, "intake-s3", "app"); next != whFar {
		t.Fatal("an issue update marked S3")
	}
	if h.webhookCountOf(sfTeam, "delivered") != 1 {
		t.Fatal("delivered not counted")
	}
	var outcome string
	if err := h.s.db.QueryRow(`SELECT outcome FROM webhook_deliveries`).Scan(&outcome); err != nil || outcome != "applied" {
		t.Fatal(outcome, err)
	}
}

func TestWebhookCommentMarksS3AndActiveWatch(t *testing.T) {
	h := newWebhookHarness(t)
	h.s.SetWebhooksProven(true)
	h.cursor(h.pid, "intake-s3", "app", "")
	h.cursor(h.pid, "comments", whIssue, "")
	h.cursor(h.pid, "comments", whIssueOff, stamp())
	h.cursor(h.pid, "intake-s1", sfTeam, "")
	now := time.Now().UTC()
	if r := h.record(commentCreate("sha256:c1", sfTeam, "00000000-0000-4000-8000-0000000000E1", now)); r.Outcome != "applied" || len(r.Projects) != 1 {
		t.Fatal(r)
	}
	for _, src := range [][2]string{{"intake-s3", "app"}, {"comments", whIssue}} {
		if next, _ := h.cursorState(h.pid, src[0], src[1]); next != layout(now) {
			t.Fatal(src, next)
		}
	}
	if next, _ := h.cursorState(h.pid, "comments", whIssueOff); next != whFar {
		t.Fatal("an inactive watch was marked")
	}
	if next, _ := h.cursorState(h.pid, "intake-s1", sfTeam); next != whFar {
		t.Fatal("a comment marked S1")
	}
	// A comment's update is not handled.
	d := commentCreate("sha256:c2", sfTeam, whIssue, now)
	d.Action = "update"
	if r := h.record(d); r.Outcome != "ignored_type" || len(r.Projects) != 0 {
		t.Fatal(r)
	}
}

func TestWebhookUnboundTeam(t *testing.T) {
	h := newWebhookHarness(t)
	h.s.SetWebhooksProven(true)
	h.cursor(h.pid, "intake-s2", "app", "")
	if r := h.record(issueUpdate("sha256:unb", whUnbound, time.Now().UTC())); r.Outcome != "unbound_team" || len(r.Projects) != 0 {
		t.Fatal(r)
	}
	if next, _ := h.cursorState(h.pid, "intake-s2", "app"); next != whFar {
		t.Fatal("marked")
	}
	if h.webhookCountOf(whUnbound, "unbound_team") != 1 {
		t.Fatal("not counted")
	}
}

// C4: a delivery marks only the cursors of the team its payload names.
func TestWebhookMovedIssueMarksPayloadTeam(t *testing.T) {
	h := newWebhookHarness(t)
	h.s.SetWebhooksProven(true)
	if _, err := h.s.Execute(context.Background(), Request{Operation: "taskboard_set_v2", Workspace: "w", Project: "listed", Args: map[string]any{"expected_revision": 0, "mode": "project_set", "linear_team_ids": []string{whTeamB}, "linear_default_team_id": whTeamB, "linear_project_ids": []string{cpTwo}}}); err != nil {
		t.Fatal(err)
	}
	other := h.slugID("listed")
	h.cursor(h.pid, "intake-s1", sfTeam, "")
	h.cursor(other, "intake-s1", whTeamB, "")
	h.cursor(h.pid, "intake-s2", "app", "")
	h.cursor(other, "intake-s2", "app", "")
	if got, err := h.s.ProjectsForTeam(context.Background(), strings.ToUpper(whTeamB)); err != nil || !reflect.DeepEqual(got, []string{other}) {
		t.Fatal(got, err)
	}
	now := time.Now().UTC()
	if r := h.record(issueUpdate("sha256:mv", whTeamB, now)); r.Outcome != "applied" || !reflect.DeepEqual(r.Projects, []string{other}) {
		t.Fatal(r)
	}
	if next, _ := h.cursorState(other, "intake-s1", whTeamB); next != layout(now) {
		t.Fatal("B's S1:", next)
	}
	if next, _ := h.cursorState(other, "intake-s2", "app"); next != layout(now) {
		t.Fatal("B's S2:", next)
	}
	for _, src := range [][2]string{{"intake-s1", sfTeam}, {"intake-s2", "app"}} {
		if next, _ := h.cursorState(h.pid, src[0], src[1]); next != whFar {
			t.Fatal("A's", src, next)
		}
	}
}

func TestWebhookIntakeOffSkipsIntakeCursors(t *testing.T) {
	ih := newIntakeHarness(t)
	ih.link(0, []string{cpOne})
	ih.call("intake_policy_set", "c", map[string]any{"mode": "off"})
	ih.s.SetWebhooksProven(true)
	pid := ih.slugID("p")
	ih.cursor(pid, "intake-s3", "app", "")
	ih.cursor(pid, "intake-s1", sfTeam, "")
	ih.cursor(pid, "comments", whIssue, "")
	now := time.Now().UTC()
	if r := ih.record(issueUpdate("sha256:off1", sfTeam, now)); r.Outcome != "applied" || len(r.Projects) != 0 {
		t.Fatal(r)
	}
	if r := ih.record(commentCreate("sha256:off2", sfTeam, whIssue, now)); r.Outcome != "applied" || len(r.Projects) != 1 {
		t.Fatal(r)
	}
	for _, src := range [][2]string{{"intake-s1", sfTeam}, {"intake-s3", "app"}} {
		if next, _ := ih.cursorState(pid, src[0], src[1]); next != whFar {
			t.Fatal("intake off marked", src, next)
		}
	}
	if next, _ := ih.cursorState(pid, "comments", whIssue); next != layout(now) {
		t.Fatal("the comment watch was skipped:", next)
	}
}

// §6 P3: a hint that arrives during a read keeps the cursor due.
func TestHintDuringReadKeepsCursorDue(t *testing.T) {
	h := newWebhookHarness(t)
	h.s.SetWebhooksProven(true)
	ctx := context.Background()
	t0 := time.Now().UTC().Add(-time.Minute)
	if err := h.s.IntakeCursorSet(ctx, h.pid, "intake-s1", sfTeam, layout(t0), false); err != nil {
		t.Fatal(err)
	}
	t1 := t0.Add(30 * time.Second)
	h.record(issueUpdate("sha256:mid", sfTeam, t1))
	if err := h.s.IntakeCursorSet(ctx, h.pid, "intake-s1", sfTeam, layout(t0), false); err != nil {
		t.Fatal(err)
	}
	if next, _ := h.cursorState(h.pid, "intake-s1", sfTeam); at(next).After(time.Now().UTC()) {
		t.Fatal("the hint was buried:", next)
	}
	// A read that started after the hint backs off as before.
	t2 := t1.Add(time.Second)
	if err := h.s.IntakeCursorSet(ctx, h.pid, "intake-s1", sfTeam, layout(t2), false); err != nil {
		t.Fatal(err)
	}
	if next, _ := h.cursorState(h.pid, "intake-s1", sfTeam); next != layout(t2.Add(15*time.Minute)) {
		t.Fatal("a later read:", next)
	}

	// A comment report: only a read that succeeded is protected (a failed
	// one backs off, TestWebhookHintDoesNotCutBackoff).
	w := newWatchHarness(t)
	w.dueNow()
	w.call("comment_watch_due", "c", w.fenced(map[string]any{}))
	if _, err := w.s.db.Exec(`UPDATE watch_cursors SET hinted_at=? WHERE source='comments'`, layout(time.Now().UTC().Add(time.Second))); err != nil {
		t.Fatal(err)
	}
	out := w.call("comment_watch_report", "c", w.fenced(map[string]any{"issue_uuid": watchIssue})).(map[string]any)
	var next string
	if err := w.s.db.QueryRow(`SELECT next_poll_at FROM watch_cursors WHERE source='comments'`).Scan(&next); err != nil {
		t.Fatal(err)
	}
	if at(next).After(time.Now().UTC()) || out["next_poll_at"] != next {
		t.Fatal("the hint was buried:", next, out)
	}
}

// A hint never cuts a failure's back-off short: it records hinted_at, but
// a backing-off cursor keeps its next poll, and a poll that then fails
// backs off as before.
func TestWebhookHintDoesNotCutBackoff(t *testing.T) {
	// (a) A hinted comment watch whose poll fails rate_limited waits the
	// hour, the issue's own cursor and the batch.
	w := newWatchHarness(t)
	w.assign("b", otherIssue)
	w.link(0, []string{cpOne})
	w.s.SetWebhooksProven(true)
	pid := w.slugID("p")
	w.dueNow()
	w.call("comment_watch_due", "c", w.fenced(map[string]any{}))
	now := time.Now().UTC()
	for i, issue := range []string{watchIssue, otherIssue} {
		if r := w.record(commentCreate("sha256:rl"+string(rune('a'+i)), sfTeam, issue, now)); r.Outcome != "applied" || len(r.Projects) != 1 {
			t.Fatal(r)
		}
	}
	w.call("comment_watch_report", "c", w.fenced(map[string]any{"issue_uuid": watchIssue, "error": "rate_limited"}))
	for _, issue := range []string{watchIssue, otherIssue} {
		next, hinted := w.cursorState(pid, "comments", issue)
		if at(next).Before(now.Add(59*time.Minute)) || hinted != layout(now) {
			t.Fatal("(a)", issue, "cut short:", next, hinted)
		}
	}
	// A hint after the 429, during its hour, for the other issue (whose
	// own cursor carries no failure) does not pull it back either.
	later := time.Now().UTC().Add(time.Second)
	if r := w.record(commentCreate("sha256:rl-after", sfTeam, otherIssue, later)); r.Outcome != "applied" || len(r.Projects) != 0 {
		t.Fatal("(a) a hint after the 429 listed the project:", r)
	}
	if next, hinted := w.cursorState(pid, "comments", otherIssue); at(next).Before(now.Add(59*time.Minute)) || hinted != layout(later) {
		t.Fatal("(a) a hint after the 429 cut the hour short:", next, hinted)
	}

	// (b, c) A cursor backing off (failures, or a last error) is hinted
	// but keeps its next poll, and its project is not listed.
	h := newWebhookHarness(t)
	h.s.SetWebhooksProven(true)
	h.cursor(h.pid, "comments", whIssue, "")
	h.cursor(h.pid, "comments", whIssueOff, "")
	h.cursor(h.pid, "intake-s1", sfTeam, "")
	h.cursor(h.pid, "intake-s3", "app", "")
	for _, q := range []string{
		`UPDATE watch_cursors SET failures=2 WHERE source='comments' AND subject='` + whIssue + `'`,
		`UPDATE watch_cursors SET last_error='no_recipient' WHERE source='comments' AND subject='` + whIssueOff + `'`,
		`UPDATE watch_cursors SET failures=1 WHERE source IN ('intake-s1','intake-s3')`,
	} {
		if _, err := h.s.db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	for i, d := range []WebhookDelivery{commentCreate("sha256:bo1", sfTeam, whIssue, now), commentCreate("sha256:bo2", sfTeam, whIssueOff, now), issueUpdate("sha256:bo3", sfTeam, now)} {
		if r := h.record(d); r.Outcome != "applied" || len(r.Projects) != 0 {
			t.Fatal("(b)", i, "listed a project that moved no cursor:", r)
		}
	}
	for _, src := range [][2]string{{"comments", whIssue}, {"comments", whIssueOff}, {"intake-s1", sfTeam}, {"intake-s3", "app"}} {
		if next, hinted := h.cursorState(h.pid, src[0], src[1]); next != whFar || hinted != layout(now) {
			t.Fatal("(b/c)", src, next, hinted)
		}
	}
}

func TestWebhookRetentionLongerThanWindow(t *testing.T) {
	if webhookRetention <= WebhookWindow {
		t.Fatal(webhookRetention, WebhookWindow)
	}
}

func TestPurgeWebhooks(t *testing.T) {
	h := newWebhookHarness(t)
	ctx := context.Background()
	now := time.Now().UTC()
	old := now.Add(-8 * 24 * time.Hour)
	h.record(issueUpdate("sha256:old", sfTeam, old))
	h.record(issueUpdate("sha256:new", sfTeam, now.Add(-time.Hour)))
	if err := h.s.WebhookCount(ctx, "", "signature", 3, old); err != nil {
		t.Fatal(err)
	}
	if err := h.s.WebhookCount(ctx, "", "signature", 2, now); err != nil {
		t.Fatal(err)
	}
	n, err := h.s.PurgeWebhooks(ctx, now)
	if err != nil || n != 4 { // the old delivery, its inert and delivered counts, and the old signature count
		t.Fatal("purged:", n, err)
	}
	var ids []string
	rows, _ := h.s.db.Query(`SELECT delivery_id FROM webhook_deliveries`)
	for rows.Next() {
		var id string
		rows.Scan(&id)
		ids = append(ids, id)
	}
	rows.Close()
	if !reflect.DeepEqual(ids, []string{"sha256:new"}) {
		t.Fatal(ids)
	}
	if h.webhookCountOf("", "signature") != 2 || h.webhookCountOf(sfTeam, "inert") != 1 {
		t.Fatal("counts:", h.webhookCountOf("", "signature"), h.webhookCountOf(sfTeam, "inert"))
	}
	if err := h.s.WebhookCount(ctx, "", "bad_payload", 1, now); err != nil {
		t.Fatal("bad_payload:", err)
	}
	if err := h.s.WebhookCount(ctx, "", "not-a-reason", 1, now); err == nil {
		t.Fatal("an unknown reason was counted")
	}
}

func (h *webhookHarness) teamStatus() (map[string]any, map[string]any) {
	h.t.Helper()
	out := h.call("webhook_status", "", nil).(map[string]any)
	teams := out["teams"].([]map[string]any)
	if len(teams) != 1 || teams[0]["team_id"] != sfTeam {
		h.t.Fatal(out)
	}
	return teams[0], out
}

func TestWebhookStatusStates(t *testing.T) {
	h := newWebhookHarness(t)
	ctx := context.Background()
	now := time.Now().UTC()
	if team, _ := h.teamStatus(); team["state"] != "unconfigured" || team["configured"] != false {
		t.Fatal(team)
	}
	if err := h.s.WebhookTeamsConfigured(ctx, []string{sfTeam}, now.Add(-2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if team, _ := h.teamStatus(); team["state"] != "inert" || team["configured"] != true {
		t.Fatal(team)
	}
	h.s.SetWebhooksProven(true)
	if err := h.s.WebhookGateOn(ctx, now.Add(-2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if team, _ := h.teamStatus(); team["state"] != "waiting_first" {
		t.Fatal(team)
	}
	h.record(issueUpdate("sha256:st1", sfTeam, now.Add(-time.Hour)))
	h.record(issueUpdate("sha256:st2", whUnbound, now.Add(-time.Hour)))
	if err := h.s.WebhookCount(ctx, sfTeam, "stale", 2, now); err != nil {
		t.Fatal(err)
	}
	if err := h.s.WebhookCount(ctx, "", "signature", 4, now); err != nil {
		t.Fatal(err)
	}
	team, out := h.teamStatus()
	if team["state"] != "live" || team["deliveries_24h"] != 1 || team["last_delivery_at"] != layout(now.Add(-time.Hour)) || team["missed_24h"] != 0 {
		t.Fatal(team)
	}
	if rejects := team["rejects_24h"].(map[string]int); !reflect.DeepEqual(rejects, map[string]int{"stale": 2}) {
		t.Fatal(rejects)
	}
	if out["signature_rejects_24h"] != 4 || out["unbound_team_deliveries_24h"] != 1 || !reflect.DeepEqual(out["unbound_teams"], []string{whUnbound}) {
		t.Fatal(out)
	}
	if counted, err := h.s.WebhookMiss(ctx, sfTeam, now.Add(-30*time.Minute), now); err != nil || !counted {
		t.Fatal(counted, err)
	}
	if team, _ := h.teamStatus(); team["state"] != "degraded" || team["missed_24h"] != 1 || team["last_missed_at"] != layout(now) {
		t.Fatal(team)
	}
	h.s.SetWebhooksProven(false)
	if team, _ := h.teamStatus(); team["state"] != "inert" {
		t.Fatal(team)
	}
	// A start that sees no secret clears the team.
	if err := h.s.WebhookTeamsConfigured(ctx, nil, now); err != nil {
		t.Fatal(err)
	}
	if team, _ := h.teamStatus(); team["state"] != "unconfigured" {
		t.Fatal(team)
	}
}

func TestWebhookMissOnlyWhileLive(t *testing.T) {
	h := newWebhookHarness(t)
	ctx := context.Background()
	now := time.Now().UTC()
	h.s.SetWebhooksProven(true)
	if err := h.s.WebhookTeamsConfigured(ctx, []string{sfTeam}, now.Add(-3*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := h.s.WebhookGateOn(ctx, now.Add(-3*time.Hour)); err != nil {
		t.Fatal(err)
	}
	miss := func(linearAt time.Time) bool {
		t.Helper()
		ok, err := h.s.WebhookMiss(ctx, sfTeam, linearAt, now)
		if err != nil {
			t.Fatal(err)
		}
		return ok
	}
	if miss(now.Add(-2 * time.Hour)) {
		t.Fatal("counted before the first delivery")
	}
	h.record(issueUpdate("sha256:first", sfTeam, now.Add(-2*time.Hour)))
	if err := h.s.WebhookDrainEnded(ctx, now.Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	if miss(now.Add(-90 * time.Minute)) {
		t.Fatal("counted before the drain's end")
	}
	if miss(now.Add(-5 * time.Minute)) {
		t.Fatal("counted a young event")
	}
	h.s.SetWebhooksProven(false)
	if miss(now.Add(-30 * time.Minute)) {
		t.Fatal("counted with the gate off")
	}
	h.s.SetWebhooksProven(true)
	if live, err := h.s.WebhookTeamLive(ctx, sfTeam, now); err != nil || !live {
		t.Fatal("live:", live, err)
	}
	if !miss(now.Add(-30 * time.Minute)) {
		t.Fatal("a live miss was not counted")
	}
	if h.webhookCountOf(sfTeam, "missed") != 1 {
		t.Fatal("missed count")
	}
	if team, _ := h.teamStatus(); team["state"] != "degraded" {
		t.Fatal(team)
	}
	if live, err := h.s.WebhookTeamLive(ctx, sfTeam, now); err != nil || live {
		t.Fatal("a degraded team is live:", live, err)
	}
}

// R4, C5: a delivery holds IDs, kinds and times only, never text.
func TestWebhookDeliveryHoldsNoText(t *testing.T) {
	want := map[string]string{
		"DeliveryID": "string", "TeamID": "string", "WebhookID": "string", "EventType": "string", "Action": "string",
		"EntityID": "string", "IssueID": "string", "UpdatedKeys": "[]string", "LinearAt": "time.Time", "ReceivedAt": "time.Time", "Size": "int",
	}
	typ := reflect.TypeOf(WebhookDelivery{})
	if typ.NumField() != len(want) {
		t.Fatal("fields:", typ.NumField())
	}
	for i := 0; i < typ.NumField(); i++ {
		f := typ.Field(i)
		if want[f.Name] != f.Type.String() {
			t.Fatal("unexpected field", f.Name, f.Type)
		}
	}
}

// webhookRecordIsAtomic checks a delivery's row, marks and counts commit
// together: a failure after the insert leaves nothing, so the redelivery
// applies; and two concurrent deliveries of one ID give one outcome and
// one duplicate. count reads a table's rows for team.
func webhookRecordIsAtomic(t *testing.T, s *Store, team string, count func(table string) int, next func() string) {
	t.Helper()
	ctx := context.Background()
	s.SetWebhooksProven(true)
	now := time.Now().UTC().Truncate(time.Second)
	d := issueUpdate("sha256:"+newID(""), team, now)
	s.webhookAfterInsert = func() error { return errors.New("injected") }
	if _, err := s.WebhookRecord(ctx, d); err == nil {
		t.Fatal("the injected failure was not returned")
	}
	s.webhookAfterInsert = nil
	if count("webhook_deliveries") != 0 || count("webhook_counts") != 0 || count("webhook_teams") != 0 || next() == layout(now) {
		t.Fatal("a failed record left state behind")
	}
	if r, err := s.WebhookRecord(ctx, d); err != nil || r.Outcome != "applied" {
		t.Fatal("the redelivery:", r, err)
	}
	if count("webhook_deliveries") != 1 || next() != layout(now) {
		t.Fatal("the redelivery did not apply")
	}
	d2 := issueUpdate("sha256:"+newID(""), team, now)
	outcomes := make(chan string, 2)
	var wg sync.WaitGroup
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r, err := s.WebhookRecord(ctx, d2)
			if err != nil {
				outcomes <- "error: " + err.Error()
				return
			}
			outcomes <- r.Outcome
		}()
	}
	wg.Wait()
	close(outcomes)
	got := []string{}
	for o := range outcomes {
		got = append(got, o)
	}
	sort.Strings(got)
	if !reflect.DeepEqual(got, []string{"applied", "duplicate"}) {
		t.Fatal("concurrent:", got)
	}
	if count("webhook_deliveries") != 2 {
		t.Fatal("rows:", count("webhook_deliveries"))
	}
}

func TestWebhookRecordIsAtomic(t *testing.T) {
	h := newWebhookHarness(t)
	h.cursor(h.pid, "intake-s1", sfTeam, "")
	count := func(table string) int {
		var n int
		if err := h.s.db.QueryRow(`SELECT COUNT(*) FROM `+table+` WHERE team_id=?`, sfTeam).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	next := func() string { n, _ := h.cursorState(h.pid, "intake-s1", sfTeam); return n }
	webhookRecordIsAtomic(t, h.s, sfTeam, count, next)
}

// The store keeps updatedFrom's key names for the probe's evidence, but
// checks them itself rather than trusting the route: only names of 1-64
// letters, at most 64 of them; never a value. size is the body's length.
func TestWebhookKeysStoredAsNamesOnly(t *testing.T) {
	h := newWebhookHarness(t)
	d := issueUpdate("sha256:keys", sfTeam, time.Now().UTC())
	d.Size = 4096
	d.UpdatedKeys = []string{"labelIds", "title value", "x" + strings.Repeat("y", 64), "key2", "", "a,b", "delegateId"}
	for i := 0; i < 70; i++ {
		d.UpdatedKeys = append(d.UpdatedKeys, "k"+strings.Repeat("z", i%5+1))
	}
	h.record(d)
	var keys string
	var size int
	if err := h.s.db.QueryRow(`SELECT updated_keys,size FROM webhook_deliveries WHERE delivery_id='sha256:keys'`).Scan(&keys, &size); err != nil {
		t.Fatal(err)
	}
	list := strings.Split(keys, ",")
	if size != 4096 || len(list) != 64 || list[0] != "labelIds" || list[1] != "delegateId" {
		t.Fatal(size, len(list), list[:2])
	}
	for _, k := range list {
		if len(k) == 0 || len(k) > 64 || strings.ContainsAny(k, " ,0123456789") {
			t.Fatal("kept", k)
		}
	}
}
