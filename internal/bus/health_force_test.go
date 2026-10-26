package bus

import (
	"context"
	"strings"
	"testing"
	"time"
)

// force makes one forced run of project and reports facts for it.
func (h *healthHarness) force(project string, forceNew bool, facts map[string]any) map[string]any {
	h.t.Helper()
	h.sys("health_force", map[string]any{"linear_project": project, "force_new": forceNew})
	return h.sys("health_report", map[string]any{"linear_project": project, "calls": 3, "facts": facts, "forced": true, "force_new": forceNew})
}

func (h *healthHarness) audited(action string) int {
	h.t.Helper()
	var n int
	if e := h.s.db.QueryRow(`SELECT COUNT(*) FROM history WHERE entity='project_health' AND action=?`, action).Scan(&n); e != nil {
		h.t.Fatal(e)
	}
	return n
}

func (h *healthHarness) budgetUsed() int {
	h.t.Helper()
	return h.sys("watch_charge", map[string]any{"calls": 0})["budget_used"].(int)
}

// A forced run is handed out only where health is enabled and posts, only
// for a bound project, and only when the hour's budget pays it in full;
// it is charged a whole run and audited.
func TestHealthForceRefusals(t *testing.T) {
	h := newHealthHarness(t)
	if _, e := h.sysErr("health_force", map[string]any{"linear_project": projectOne}); e == nil || !strings.Contains(e.Error(), "not enabled") {
		t.Fatal("forced without health enabled:", e)
	}
	h.enable(projectOne, 0, nil)
	if _, e := h.sysErr("health_force", map[string]any{"linear_project": projectOne}); e == nil || !strings.Contains(e.Error(), "post") {
		t.Fatal("forced without post:", e)
	}
	h.enable(projectOne, 1, map[string]any{"enabled": true, "post": true})
	if _, e := h.sysErr("health_force", map[string]any{"linear_project": projectOne, "force_new": "yes"}); e == nil {
		t.Fatal("a non-boolean force_new was taken")
	}
	if _, e := h.sysErr("health_report", map[string]any{"linear_project": projectOne, "calls": 0, "facts": blockedFacts(5), "force_new": true}); e == nil {
		t.Fatal("force_new was taken without forced")
	}
	before := h.budgetUsed()
	if r := h.sys("health_force", map[string]any{"linear_project": projectOne}); r["linear_project"] != projectOne || r["stalled_days"] != 5 {
		t.Fatal(r)
	}
	if used := h.budgetUsed(); used != before+HealthMaxCalls || h.audited("run_forced") != 1 {
		t.Fatal("not charged or audited:", before, used)
	}
	if _, e := h.s.db.Exec(`UPDATE watch_lease SET budget_used=budget_limit-?`, HealthMaxCalls); e != nil {
		t.Fatal(e)
	}
	if _, e := h.sysErr("health_force", map[string]any{"linear_project": projectOne}); e == nil || !strings.Contains(e.Error(), "budget") {
		t.Fatal("forced past the budget:", e)
	}
	h.call("taskboard_set_v2", "a", taskboardArgsV2(1, projectTwo))
	if _, e := h.s.db.Exec(`UPDATE watch_lease SET budget_used=0`); e != nil {
		t.Fatal(e)
	}
	if _, e := h.sysErr("health_force", map[string]any{"linear_project": projectOne}); e == nil {
		t.Fatal("forced for a project no longer bound")
	}
	if _, e := h.sysErr("health_force", map[string]any{"linear_project": projectOne, "lease_token": "x"}); e == nil {
		t.Fatal("forced without the lease")
	}
}

func (h *healthHarness) slot(project string) string {
	h.t.Helper()
	var next string
	if e := h.s.db.QueryRow(`SELECT next_health_at FROM project_health WHERE linear_project_id=?`, project).Scan(&next); e != nil {
		h.t.Fatal(e)
	}
	return next
}

// farSlot moves every slot far ahead, so a run that sets one shows.
func (h *healthHarness) farSlot() {
	h.t.Helper()
	if _, e := h.s.db.Exec(`UPDATE project_health SET next_health_at='2999-01-01T00:00:00.000000000Z'`); e != nil {
		h.t.Fatal(e)
	}
}

func (h *healthHarness) auditedBy(action string) string {
	h.t.Helper()
	var who string
	if e := h.s.db.QueryRow(`SELECT changed_by_principal FROM history WHERE entity='project_health' AND action=? ORDER BY changed_at DESC LIMIT 1`, action).Scan(&who); e != nil {
		h.t.Fatal(action, e)
	}
	return who
}

// R91-M2/M3/L2: without --force-new a forced run records no observation
// and never moves the slot, failed or not. It acts only on a health
// pending since an earlier day, as the scheduled run would, and then posts
// under the scheduled run's own label.
func TestAPlainForcedRunRecordsNoObservation(t *testing.T) {
	h := newHealthHarness(t)
	h.enable(projectOne, 0, map[string]any{"enabled": true, "post": true})
	h.farSlot()
	slot := h.slot(projectOne)
	r := h.force(projectOne, false, withUpdates(blockedFacts(5)))
	if r["acted"] != "" || r["post"] != nil || r["pending_health"] != "" || !strings.Contains(r["note"].(string), "Needs a confirming run") || h.slot(projectOne) != slot || h.notices() != 0 {
		t.Fatal(r)
	}
	h.sys("health_force", map[string]any{"linear_project": projectOne})
	if r := h.sys("health_report", map[string]any{"linear_project": projectOne, "calls": 1, "error": "too_many_issues", "forced": true}); r["error"] != "too_many_issues" || h.slot(projectOne) != slot {
		t.Fatal("a failed forced run moved the slot:", r, h.slot(projectOne))
	}
	// A scheduled run sees it first; a forced run the same day doesn't
	// confirm it.
	if r := h.run(projectOne, withUpdates(blockedFacts(5))); r["pending_health"] != "offTrack" {
		t.Fatal(r)
	}
	h.farSlot()
	slot = h.slot(projectOne)
	if r := h.force(projectOne, false, withUpdates(blockedFacts(5))); r["acted"] != "" || r["post"] != nil || !strings.Contains(r["note"].(string), "less than a day ago") || r["pending_health"] != "offTrack" || h.slot(projectOne) != slot {
		t.Fatal(r)
	}
	// Pending since yesterday: the forced run is the second daily run.
	if _, e := h.s.db.Exec(`UPDATE project_health SET pending_since=?`, time.Now().UTC().Add(-21*time.Hour).Format("2006-01-02T15:04:05.000000000Z")); e != nil {
		t.Fatal(e)
	}
	r = h.force(projectOne, false, withUpdates(blockedFacts(5)))
	post, _ := r["post"].(map[string]any)
	if r["acted"] != "changed" || post == nil || !strings.Contains(post["body"].(string), "the same on two daily runs") || h.audited("hysteresis_bypassed") != 0 || h.slot(projectOne) != slot {
		t.Fatal(r)
	}
	var act string
	if e := h.s.db.QueryRow(`SELECT act FROM health_posts WHERE linear_project_id=?`, projectOne).Scan(&act); e != nil || act != "changed" {
		t.Fatal(act, e)
	}
	h.sys("health_post_settle", map[string]any{"linear_project": projectOne, "day": post["day"], "outcome": "accepted", "update_id": updateOne})
	if _, e := h.s.OperatorForcedHealthPost(context.Background(), projectOne, updateOne); e == nil {
		t.Fatal("a confirmed post may be deleted as a forced one")
	}
	if r := h.force(projectOne, false, withUpdates(blockedFacts(5))); r["acted"] != "" || !strings.Contains(r["note"].(string), "Unchanged") {
		t.Fatal(r)
	}
	// A scheduled run's report carries no note.
	if r := h.run(projectOne, withUpdates(blockedFacts(5))); r["note"] != nil || r["held"] != nil {
		t.Fatal(r)
	}
}

// --force-new acts on a single run, audited as the operator, and takes the
// day only from a post no longer on Linear (R91-M4): never a pending,
// accepted or scheduled one.
func TestHealthForceNewActsOnOneRunAndReplacesTheDay(t *testing.T) {
	h := newHealthHarness(t)
	h.enable(projectOne, 0, map[string]any{"enabled": true, "post": true})
	h.farSlot()
	slot := h.slot(projectOne)
	r := h.force(projectOne, true, withUpdates(blockedFacts(5)))
	post, _ := r["post"].(map[string]any)
	if r["acted"] != "forced" || post == nil || !strings.Contains(post["body"].(string), "a run the server operator forced") || h.audited("hysteresis_bypassed") != 1 || h.slot(projectOne) != slot {
		t.Fatal(r)
	}
	for _, action := range []string{"run_forced", "hysteresis_bypassed"} {
		if who := h.auditedBy(action); who != OperatorPrincipal {
			t.Fatal(action, "audited as", who)
		}
	}
	if r := h.force(projectOne, true, withUpdates(blockedFacts(5))); r["post"] != nil || !strings.Contains(r["held"].(string), "not confirmed yet") || h.notices() != 1 {
		t.Fatal("a pending post was replaced:", r)
	}
	h.sys("health_post_settle", map[string]any{"linear_project": projectOne, "day": post["day"], "outcome": "accepted", "update_id": updateOne})
	if r := h.force(projectOne, true, withUpdates(blockedFacts(5))); r["post"] != nil || !strings.Contains(r["held"].(string), updateOne+", may still be on Linear") || h.audited("post_replaced") != 0 {
		t.Fatal("replaced a forced post still on Linear:", r)
	}
	ref, e := h.s.OperatorForcedHealthPost(context.Background(), projectOne, updateOne)
	if e != nil {
		t.Fatal(e)
	}
	if e = h.s.OperatorForcedHealthPostDeleted(context.Background(), ref); e != nil {
		t.Fatal(e)
	}
	r = h.force(projectOne, true, withUpdates(blockedFacts(5)))
	if r["acted"] != "forced" || r["post"] == nil || h.audited("post_replaced") != 1 || h.auditedBy("post_replaced") != OperatorPrincipal {
		t.Fatal(r)
	}
	if row := h.postRow(projectOne); row["state"] != "pending" || row["update_id"] != "" {
		t.Fatal(row)
	}
	// A refused post is not on Linear: it is replaced.
	h.sys("health_post_settle", map[string]any{"linear_project": projectOne, "day": post["day"], "outcome": "refused", "error": "app_scope"})
	if r := h.force(projectOne, true, withUpdates(blockedFacts(5))); r["post"] == nil || h.audited("post_replaced") != 2 {
		t.Fatal("a refused post was not replaced:", r)
	}
	h.sys("health_post_settle", map[string]any{"linear_project": projectOne, "day": post["day"], "outcome": "accepted", "update_id": updateTwo})
	recent := update("e1000000-0000-4000-8000-000000000003", time.Now().Add(-time.Hour), false)
	if r := h.force(projectOne, true, withUpdates(blockedFacts(5), recent)); r["post"] != nil || !strings.Contains(r["held"].(string), "A person posted") {
		t.Fatal("posted over a person's update:", r)
	}
	if r := h.force(projectOne, true, withUpdates(map[string]any{"total": 0, "completed": 0, "canceled": 0, "started": 0, "stalled": []any{}, "stalled_count": 0, "unverified": 0, "blocked": []any{}, "blocked_count": 0, "target_date": "", "start_date": "2026-01-01", "start_from": "startDate"})); r["acted"] != "" || !strings.Contains(r["note"].(string), "Nothing to judge") {
		t.Fatal(r)
	}
	// With --force-new a health it confirms still posts as forced: the
	// operator asked for it, and deletes it.
	h.enable(projectTwo, 0, map[string]any{"enabled": true, "post": true})
	h.run(projectTwo, withUpdates(blockedFacts(5)))
	r = h.force(projectTwo, true, withUpdates(blockedFacts(5)))
	if r["acted"] != "forced" || r["post"] == nil {
		t.Fatal(r)
	}
	// The scheduled run's accepted post is never replaced.
	if _, e := h.s.db.Exec(`UPDATE health_posts SET act='changed',state='accepted',update_id='e1000000-0000-4000-8000-000000000004' WHERE linear_project_id=?`, projectTwo); e != nil {
		t.Fatal(e)
	}
	if r := h.force(projectTwo, true, withUpdates(blockedFacts(5))); r["post"] != nil || !strings.Contains(r["held"].(string), "may still be on Linear") {
		t.Fatal("replaced the scheduled post:", r)
	}
}

// R91-M1: a pending post settles to the app's update nearest its send,
// and never to one another post of the project already holds.
func TestHealthPostSettlesToItsOwnUpdate(t *testing.T) {
	h := newHealthHarness(t)
	h.enable(projectOne, 0, map[string]any{"enabled": true, "post": true})
	// Yesterday's post, still on Linear.
	if _, e := h.s.db.Exec(`INSERT INTO health_posts(project_id,linear_project_id,day,health,was,act,state,sent_at,update_id) VALUES(?,?,'2000-01-01','offTrack','','changed','accepted','2000-01-01T00:00:00.000000000Z',?)`, h.pid, projectOne, updateTwo); e != nil {
		t.Fatal(e)
	}
	post := h.force(projectOne, true, withUpdates(blockedFacts(5)))["post"].(map[string]any)
	h.sys("health_post_settle", map[string]any{"linear_project": projectOne, "day": post["day"], "outcome": "uncertain", "error": "unavailable"})
	now := time.Now()
	// updateTwo, nearest the send, is the other post's; updateOne is next
	// nearest; the last one listed is further.
	h.force(projectOne, false, withUpdates(blockedFacts(5),
		update(updateTwo, now, true),
		update(updateOne, now.Add(30*time.Second), true),
		update("e1000000-0000-4000-8000-000000000005", now.Add(4*time.Minute), true)))
	if row := h.postRow(projectOne); row["state"] != "accepted" || row["update_id"] != updateOne {
		t.Fatal(row)
	}
}

// The operator deletes only a post a forced run made and Linear accepted,
// once; the delete is recorded, charged and audited.
func TestOperatorDeletesOnlyAForcedPost(t *testing.T) {
	h := newHealthHarness(t)
	ctx := context.Background()
	if _, e := h.s.OperatorHealthProject(ctx, projectOne); e == nil {
		t.Fatal("found a project without health enabled")
	}
	h.enable(projectOne, 0, map[string]any{"enabled": true, "post": true})
	h.enable(projectTwo, 0, map[string]any{"enabled": true, "post": true})
	if pid, e := h.s.OperatorHealthProject(ctx, strings.ToUpper(projectOne)); e != nil || pid != h.pid {
		t.Fatal(pid, e)
	}
	// An ordinary post is not the operator's.
	h.run(projectTwo, withUpdates(blockedFacts(5)))
	ordinary := h.run(projectTwo, withUpdates(blockedFacts(5)))["post"].(map[string]any)
	h.sys("health_post_settle", map[string]any{"linear_project": projectTwo, "day": ordinary["day"], "outcome": "accepted", "update_id": updateTwo})
	if _, e := h.s.OperatorForcedHealthPost(ctx, projectTwo, updateTwo); e == nil {
		t.Fatal("an ordinary post may be deleted")
	}
	forced := h.force(projectOne, true, withUpdates(blockedFacts(5)))["post"].(map[string]any)
	if _, e := h.s.OperatorForcedHealthPost(ctx, projectOne, updateOne); e == nil {
		t.Fatal("a pending post may be deleted")
	}
	h.sys("health_post_settle", map[string]any{"linear_project": projectOne, "day": forced["day"], "outcome": "accepted", "update_id": updateOne})
	if _, e := h.s.OperatorForcedHealthPost(ctx, projectTwo, updateOne); e == nil {
		t.Fatal("found the post on another Linear project")
	}
	if _, e := h.s.OperatorForcedHealthPost(ctx, projectOne, "not-a-uuid"); e == nil {
		t.Fatal("took an unreadable update ID")
	}
	if _, e := h.s.db.Exec(`UPDATE health_posts SET state='failed' WHERE linear_project_id=?`, projectOne); e != nil {
		t.Fatal(e)
	}
	if _, e := h.s.OperatorForcedHealthPost(ctx, projectOne, updateOne); e == nil {
		t.Fatal("a post not accepted may be deleted")
	}
	if _, e := h.s.db.Exec(`UPDATE health_posts SET state='accepted' WHERE linear_project_id=?`, projectOne); e != nil {
		t.Fatal(e)
	}
	ref, e := h.s.OperatorForcedHealthPost(ctx, projectOne, strings.ToUpper(updateOne))
	if e != nil || ref.ProjectID != h.pid || ref.UpdateID != updateOne || ref.Day != forced["day"] {
		t.Fatal(ref, e)
	}
	before := h.budgetUsed()
	if e = h.s.OperatorForcedHealthPostDeleted(ctx, ref); e != nil {
		t.Fatal(e)
	}
	if used := h.budgetUsed(); used != before+HealthPostDeleteCalls || h.audited("post_deleted") != 1 {
		t.Fatal("not charged or audited:", before, used)
	}
	if row := h.postRow(projectOne); row["state"] != "accepted" || row["error"] != "deleted" {
		t.Fatal(row)
	}
	if _, e := h.s.OperatorForcedHealthPost(ctx, projectOne, updateOne); e == nil || !strings.Contains(e.Error(), "already recorded") {
		t.Fatal("deleted twice:", e)
	}
	if e = h.s.OperatorForcedHealthPostDeleted(ctx, ref); e == nil {
		t.Fatal("recorded twice")
	}
}
