package bus

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"

	"clawbridge/internal/principal"
)

var leadOperationID = regexp.MustCompile(`^[A-Za-z0-9._:-]{1,128}$`)

// The Linear project lead consumer (live/TEAM-ROLE-MAPPING-DESIGN.md §6.1,
// §14, step 6.1a): whether ClawBridge sets a project's lead from the team
// roles, and when that was turned on (the later watcher acts only on
// projects Linear created after it, review V3). It is off until the
// project owner turns it on. The writes themselves go through the owner's
// own Linear profile; each is recorded here by outcome, never a name.
const projectLeadNoticeSchema = `
CREATE TABLE project_lead_state(project_id TEXT NOT NULL REFERENCES projects(id),linear_project TEXT NOT NULL,lead_uuid TEXT NOT NULL,principal TEXT NOT NULL,set_op TEXT NOT NULL,set_at TEXT NOT NULL,PRIMARY KEY(project_id,linear_project));
CREATE TABLE project_lead_notices(id TEXT PRIMARY KEY,project_id TEXT NOT NULL REFERENCES projects(id),principal TEXT NOT NULL,linear_project TEXT NOT NULL,kind TEXT NOT NULL CHECK(kind IN ('new','old')),op TEXT NOT NULL,created_at TEXT NOT NULL,delivered_at TEXT,UNIQUE(project_id,op,kind));
`

const projectLeadSchema = `
CREATE TABLE project_lead_settings(project_id TEXT PRIMARY KEY REFERENCES projects(id),revision INTEGER NOT NULL CHECK(revision>0),mode TEXT NOT NULL CHECK(mode IN ('off','on')),on_at TEXT,updated_by TEXT NOT NULL,updated_at TEXT NOT NULL);
`

func projectLeadOperation(op string) bool {
	return op == "project_lead_get" || op == "project_lead_set" || op == "project_lead_record"
}

// projectLeadOp reads the setting (any member or session), sets it, or
// records a lead write (the project owner, without a session; on the
// hosted server only from a person's own device).
func (s *Store) projectLeadOp(ctx context.Context, tx *transaction, wid, pid, projectRole string, r Request) (any, error) {
	a := r.Args
	if a == nil {
		a = map[string]any{}
	}
	if r.Session == "" && str(a, "instance") != "" {
		return nil, errors.New("instance requires session")
	}
	if r.Operation == "project_lead_get" {
		return readProjectLead(ctx, tx, pid)
	}
	if r.Session != "" {
		return nil, scoped("a session can't change project leads; the project owner runs clawbridge linear roles in their own terminal", "forbidden")
	}
	if projectRole != "owner" {
		return nil, scoped("project owner permission required", "forbidden")
	}
	if s.postgres && (s.identity == nil || s.identity.Issuer != DeviceIssuer || principal.CI(s.principal)) {
		return nil, scoped("project leads are changed only from a person's own signed-in device", "forbidden")
	}
	if r.Operation == "project_lead_record" {
		return s.recordProjectLead(ctx, tx, wid, pid, a)
	}
	expected, err := integer(a["expected_revision"])
	if err != nil || a["expected_revision"] == nil {
		return nil, invalid("project_lead_set requires expected_revision")
	}
	mode := str(a, "mode")
	if mode != "on" && mode != "off" {
		return nil, invalid("project_lead_set mode is on or off")
	}
	current, err := readProjectLead(ctx, tx, pid)
	if err != nil {
		return nil, err
	}
	if current["revision"] != expected {
		return nil, conflict(fmt.Sprintf("the project lead setting changed: current revision is %d; read it again", current["revision"]))
	}
	now, err := dbStamp(ctx, tx)
	if err != nil {
		return nil, err
	}
	// on_at is when it was last turned on; it stays while it stays on.
	onAt := current["on_at"]
	if mode == "on" && current["mode"] != "on" {
		onAt = now
	}
	revision := expected + 1
	var result interface{ RowsAffected() (int64, error) }
	if expected == 0 {
		result, err = tx.ExecContext(ctx, `INSERT INTO project_lead_settings(project_id,revision,mode,on_at,updated_by,updated_at) VALUES(?,?,?,?,?,?) ON CONFLICT(project_id) DO NOTHING`, pid, revision, mode, onAt, s.principal, now)
	} else {
		result, err = tx.ExecContext(ctx, `UPDATE project_lead_settings SET revision=?,mode=?,on_at=?,updated_by=?,updated_at=? WHERE project_id=? AND revision=?`, revision, mode, onAt, s.principal, now, pid, expected)
	}
	if err == nil {
		if n, _ := result.RowsAffected(); n != 1 {
			err = conflict("the project lead setting changed concurrently; read it again")
		}
	}
	if err != nil {
		return nil, err
	}
	if err = s.audit(ctx, tx, wid, pid, "", "project_lead", pid, "set", map[string]any{"revision": revision, "mode": mode}); err != nil {
		return nil, err
	}
	return readProjectLead(ctx, tx, pid)
}

// recordProjectLead audits one lead write: the Linear project, where it
// came from, its outcome as read back, and whether it replaced a lead.
// The lead's user is never recorded. An accepted write also tells the new
// lead, since Linear notifies nobody (the probe's P-c): notifyProjectLead.
func (s *Store) recordProjectLead(ctx context.Context, tx *transaction, wid, pid string, a map[string]any) (any, error) {
	project, source, outcome := str(a, "linear_project"), str(a, "source"), str(a, "outcome")
	if !issueUUIDPattern.MatchString(project) {
		return nil, invalid("project_lead_record names the Linear project by UUID")
	}
	if source != "create" && source != "apply" {
		return nil, invalid("project_lead_record source is create or apply")
	}
	if outcome != "accepted" && outcome != "uncertain" {
		return nil, invalid("project_lead_record outcome is accepted or uncertain")
	}
	replaced, _ := a["replaced"].(bool)
	details := map[string]any{"linear_project": project, "source": source, "outcome": outcome, "replaced": replaced}
	// Linear adds the lead as a project member: the counts show it, never who.
	if a["members_before"] != nil || a["members_after"] != nil {
		before, e1 := integer(a["members_before"])
		after, e2 := integer(a["members_after"])
		if e1 != nil || e2 != nil || before < 0 || after < 0 {
			return nil, invalid("project_lead_record's member counts are both whole numbers")
		}
		details["members_before"], details["members_after"] = before, after
	}
	lead, op := strings.ToLower(str(a, "lead")), str(a, "operation_id")
	if lead != "" && !issueUUIDPattern.MatchString(lead) {
		return nil, invalid("project_lead_record's lead is a Linear user UUID")
	}
	if lead != "" && !leadOperationID.MatchString(op) {
		return nil, invalid("project_lead_record with a lead names the write's operation_id")
	}
	previous := strings.ToLower(str(a, "previous_lead"))
	if previous != "" && !issueUUIDPattern.MatchString(previous) {
		return nil, invalid("project_lead_record's previous_lead is a Linear user UUID")
	}
	told := leadTold{}
	if outcome == "accepted" && lead != "" {
		var err error
		if told, err = s.projectLeadNotices(ctx, tx, wid, pid, project, lead, previous, op); err != nil {
			return nil, err
		}
	}
	details["notified"], details["queued"], details["old_lead"] = told.sessions, told.queued, told.old
	if err := s.audit(ctx, tx, wid, pid, "", "project_lead", project, "lead", details); err != nil {
		return nil, err
	}
	return map[string]any{"recorded": true, "notified": told.sessions, "queued": told.queued, "old_lead": told.old}, nil
}

// leadTold is what a lead write's notices did: sessions told now, notices
// left waiting for a session, and the earlier lead's notice ("told",
// "queued", "not_told" when ClawBridge didn't set them or can't reach
// them, or "" when nobody was replaced).
type leadTold struct {
	sessions, queued int
	old              string
}

// projectLeadNotices tells the new lead, and on a replace the earlier one,
// since Linear notifies nobody (the probe's P-c). The client's UUIDs never
// pick a recipient by themselves (review):
//   - the new lead must be a lead row of the stored mapping that leads a
//     bound team, whose principal the owner linked (T3);
//   - the earlier lead is the one ClawBridge itself last set on that Linear
//     project (project_lead_state), only when the client's previous_lead
//     agrees; a lead set outside ClawBridge is "not_told";
//   - either must still be a project member, now and at delivery.
//
// A notice waits for a session when its person has none, and is delivered
// once (claimed by delivered_at). The op keys it, so a retry repeats
// nothing.
func (s *Store) projectLeadNotices(ctx context.Context, tx *transaction, wid, pid, project, lead, previous, op string) (leadTold, error) {
	var out leadTold
	now, err := dbStamp(ctx, tx)
	if err != nil {
		return out, err
	}
	if err = pruneLeadNotices(ctx, tx, pid, now); err != nil {
		return out, err
	}
	who, err := s.mappedLeadPrincipal(ctx, tx, wid, pid, lead)
	if err != nil {
		return out, err
	}
	state, err := one(ctx, tx, `SELECT lead_uuid,principal FROM project_lead_state WHERE project_id=? AND linear_project=?`, pid, project)
	if err != nil && !errors.Is(err, errNotFound) {
		return out, err
	}
	if previous != "" && previous != lead {
		out.old = "not_told"
		if state != nil && str(state, "lead_uuid") == previous && str(state, "principal") != "" {
			member, e := s.projectMember(ctx, tx, wid, pid, str(state, "principal"))
			if e != nil {
				return out, e
			}
			if member {
				if err = queueLeadNotice(ctx, tx, pid, str(state, "principal"), project, "old", op, now); err != nil {
					return out, err
				}
				out.old = "queued"
			}
		}
	}
	// The lead ClawBridge last set here. ClawBridge never clears a lead (its
	// writes always set one; review R3), so the row is only ever replaced.
	if _, err = tx.ExecContext(ctx, `INSERT INTO project_lead_state(project_id,linear_project,lead_uuid,principal,set_op,set_at) VALUES(?,?,?,?,?,?) ON CONFLICT(project_id,linear_project) DO UPDATE SET lead_uuid=excluded.lead_uuid,principal=excluded.principal,set_op=excluded.set_op,set_at=excluded.set_at`, pid, project, lead, who, op, now); err != nil {
		return out, err
	}
	if who != "" {
		if err = queueLeadNotice(ctx, tx, pid, who, project, "new", op, now); err != nil {
			return out, err
		}
	}
	for _, p := range []string{who, str(state, "principal")} {
		if p == "" {
			continue
		}
		n, e := s.deliverLeadNotices(ctx, tx, wid, pid, p)
		if e != nil {
			return out, e
		}
		out.sessions += n
		if p == who && str(state, "principal") == who {
			break
		}
	}
	if out.old == "queued" {
		if row, e := one(ctx, tx, `SELECT delivered_at FROM project_lead_notices WHERE project_id=? AND op=? AND kind='old'`, pid, op); e == nil && row["delivered_at"] != nil {
			out.old = "told"
		}
	}
	waiting, err := one(ctx, tx, `SELECT count(*) AS n FROM project_lead_notices WHERE project_id=? AND op=? AND delivered_at IS NULL`, pid, op)
	if err != nil {
		return out, err
	}
	out.queued = asInt(waiting["n"])
	return out, nil
}

// mappedLeadPrincipal is the principal the owner linked to lead's lead row,
// when that row leads a bound team and the principal is still a project
// member; "" otherwise.
func (s *Store) mappedLeadPrincipal(ctx context.Context, tx *transaction, wid, pid, lead string) (string, error) {
	m, revision, err := readTeamRoles(ctx, tx, pid)
	if err != nil || revision == 0 {
		return "", err
	}
	b, err := s.kindBinding(ctx, tx, pid)
	if err != nil {
		return "", nil
	}
	who := ""
	for _, p := range m.People {
		if p.LinearUserID == lead && p.Role == "lead" {
			who = p.Principal
		}
	}
	leads := false
	for _, t := range m.Teams {
		if slices.Contains(t.Leads, lead) && slices.Contains(b.LinearTeamIDs, t.TeamID) {
			leads = true
		}
	}
	if who == "" || !leads {
		return "", nil
	}
	member, err := s.projectMember(ctx, tx, wid, pid, who)
	if err != nil || !member {
		return "", err
	}
	return who, nil
}

// leadNoticeWaitDays is how long a notice waits for a session (review R1):
// older, it is out of date and never delivered.
const leadNoticeWaitDays = 30

// queueLeadNotice queues one notice. A waiting notice of the other kind for
// the same person and project, from an earlier write, is out of date and
// dropped (review R1): a later "you're the lead" supersedes "you're no
// longer", and the reverse.
func queueLeadNotice(ctx context.Context, tx *transaction, pid, who, project, kind, op, now string) error {
	other := "old"
	if kind == "old" {
		other = "new"
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM project_lead_notices WHERE project_id=? AND principal=? AND linear_project=? AND kind=? AND op<>? AND delivered_at IS NULL`, pid, who, project, other, op); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO project_lead_notices(id,project_id,principal,linear_project,kind,op,created_at) VALUES(?,?,?,?,?,?,?) ON CONFLICT(project_id,op,kind) DO NOTHING`, newID("pln_"), pid, who, project, kind, op, now)
	return err
}

// pruneLeadNotices drops delivered notices after 90 days and waiting ones
// past leadNoticeWaitDays. It runs on lead writes, never on a session's
// registration (review R4).
func pruneLeadNotices(ctx context.Context, tx *transaction, pid, now string) error {
	_, err := tx.ExecContext(ctx, `DELETE FROM project_lead_notices WHERE project_id=? AND ((delivered_at IS NOT NULL AND delivered_at<?) OR (delivered_at IS NULL AND created_at<?))`, pid, layout(at(now).AddDate(0, 0, -90)), layout(at(now).AddDate(0, 0, -leadNoticeWaitDays)))
	return err
}

// deliverLeadNotices sends who's waiting notices to who's active sessions,
// each claimed once by delivered_at in this transaction. With no active
// session, or once who is no longer a project member, they wait, for at
// most leadNoticeWaitDays. It answers how many sessions it told.
func (s *Store) deliverLeadNotices(ctx context.Context, tx *transaction, wid, pid, who string) (int, error) {
	// Notices hold the normalized principal; a local Windows one has
	// capitals.
	who = principal.Normal(who)
	now, err := dbStamp(ctx, tx)
	if err != nil {
		return 0, err
	}
	pending, err := query(ctx, tx, `SELECT id,linear_project,kind FROM project_lead_notices WHERE project_id=? AND principal=? AND delivered_at IS NULL AND created_at>=? ORDER BY created_at,id`, pid, who, layout(at(now).AddDate(0, 0, -leadNoticeWaitDays)))
	if err != nil || len(pending) == 0 {
		return 0, err
	}
	if member, e := s.projectMember(ctx, tx, wid, pid, who); e != nil || !member {
		return 0, e
	}
	// The stored link is normalized; a session keeps its owner's case (a
	// local Windows principal has capitals).
	to, err := query(ctx, tx, `SELECT id,name FROM sessions WHERE project_id=? AND status='active' AND role<>'system' AND lower(owner_principal)=? ORDER BY id`, pid, principal.Normal(who))
	if err != nil || len(to) == 0 {
		return 0, err
	}
	w, err := one(ctx, tx, `SELECT retention_days FROM workspaces WHERE id=?`, wid)
	if err != nil {
		return 0, err
	}
	retention := asInt(w["retention_days"])
	if retention < 1 {
		retention = 30
	}
	if err = s.ensureSystemSession(ctx, tx, wid, pid); err != nil {
		return 0, err
	}
	system, err := one(ctx, tx, `SELECT id FROM sessions WHERE project_id=? AND role='system' AND name=?`, pid, SystemSessionName)
	if err != nil {
		return 0, err
	}
	told := 0
	for _, n := range pending {
		res, err := tx.ExecContext(ctx, `UPDATE project_lead_notices SET delivered_at=? WHERE id=? AND delivered_at IS NULL`, now, n["id"])
		if err != nil {
			return 0, err
		}
		if c, e := res.RowsAffected(); e != nil || c != 1 {
			if e != nil {
				return 0, e
			}
			continue
		}
		body := fmt.Sprintf("You're now the lead of Linear project %s, set from the team roles by the project owner. Linear doesn't notify you itself. This is an automatic notice, not an instruction.", n["linear_project"])
		if n["kind"] == "old" {
			body = fmt.Sprintf("You're no longer the lead of Linear project %s: the project owner set another lead from the team roles. You stay a member of the project in Linear. This is an automatic notice, not an instruction.", n["linear_project"])
		}
		for _, c := range to {
			id := newID("msg_")
			res, err := tx.ExecContext(ctx, `INSERT INTO messages(id,project_id,source_project_id,sender_id,address,thread_id,reply_to,kind,body,created_at,expires_at,created_by_principal,operation_id,payload_hash,coordination) VALUES(?,?,?,?,?,?,NULL,'project-lead',?,?,?,?,?,?,NULL) ON CONFLICT(sender_id,operation_id) DO NOTHING`,
				id, pid, pid, system["id"], str(c, "name"), id, body, now, layout(at(now).AddDate(0, 0, retention)), systemPrincipal, "project-lead:"+str(n, "id")+":"+str(c, "id"), digestOf(body))
			if err != nil {
				return 0, err
			}
			if k, e := res.RowsAffected(); e != nil || k == 0 {
				if e != nil {
					return 0, e
				}
				continue
			}
			if err = insertDeliveries(ctx, tx, pid, id, []string{str(c, "id")}); err != nil {
				return 0, err
			}
			told++
		}
	}
	return told, nil
}

// readProjectLead is the setting: revision 0 and off when none is set.
func readProjectLead(ctx context.Context, tx *transaction, pid string) (map[string]any, error) {
	row, err := one(ctx, tx, `SELECT revision,mode,on_at,updated_by,updated_at FROM project_lead_settings WHERE project_id=?`, pid)
	if errors.Is(err, errNotFound) {
		return map[string]any{"revision": 0, "mode": "off", "on_at": nil, "updated_by": nil, "updated_at": nil}, nil
	}
	if err != nil {
		return nil, err
	}
	return map[string]any{"revision": asInt(row["revision"]), "mode": row["mode"], "on_at": row["on_at"], "updated_by": row["updated_by"], "updated_at": row["updated_at"]}, nil
}
