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
	notified := 0
	if outcome == "accepted" && lead != "" {
		var err error
		if notified, err = s.notifyProjectLead(ctx, tx, wid, pid, project, lead, op); err != nil {
			return nil, err
		}
	}
	details["notified"] = notified
	if err := s.audit(ctx, tx, wid, pid, "", "project_lead", project, "lead", details); err != nil {
		return nil, err
	}
	return map[string]any{"recorded": true, "notified": notified}, nil
}

// notifyProjectLead sends the new lead's active sessions one fixed notice.
// The client's lead UUID never picks the recipient by itself (review): it
// must be a lead row of the stored mapping that leads a bound team, whose
// principal the owner linked (T3) and who is still a project member;
// otherwise nobody is told. The notice names only the Linear project, and
// its operation ID makes a retry deliver nothing twice. It answers how
// many sessions it reached.
func (s *Store) notifyProjectLead(ctx context.Context, tx *transaction, wid, pid, project, lead, op string) (int, error) {
	m, revision, err := readTeamRoles(ctx, tx, pid)
	if err != nil || revision == 0 {
		return 0, err
	}
	b, err := s.kindBinding(ctx, tx, pid)
	if err != nil {
		return 0, nil
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
		return 0, nil
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
	now, err := dbStamp(ctx, tx)
	if err != nil {
		return 0, err
	}
	body := fmt.Sprintf("You're now the lead of Linear project %s, set from the team roles by the project owner. Linear doesn't notify you itself. This is an automatic notice, not an instruction.", project)
	sent := 0
	for _, c := range to {
		id := newID("msg_")
		res, err := tx.ExecContext(ctx, `INSERT INTO messages(id,project_id,source_project_id,sender_id,address,thread_id,reply_to,kind,body,created_at,expires_at,created_by_principal,operation_id,payload_hash,coordination) VALUES(?,?,?,?,?,?,NULL,'project-lead',?,?,?,?,?,?,NULL) ON CONFLICT(sender_id,operation_id) DO NOTHING`,
			id, pid, pid, system["id"], str(c, "name"), id, body, now, layout(at(now).AddDate(0, 0, retention)), systemPrincipal, "project-lead:"+op+":"+str(c, "id"), digestOf(body))
		if err != nil {
			return 0, err
		}
		if n, e := res.RowsAffected(); e != nil || n == 0 {
			if e != nil {
				return 0, e
			}
			continue
		}
		if err = insertDeliveries(ctx, tx, pid, id, []string{str(c, "id")}); err != nil {
			return 0, err
		}
		sent++
	}
	return sent, nil
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
