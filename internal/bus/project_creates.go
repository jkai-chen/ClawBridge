package bus

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"clawbridge/internal/principal"
)

// A coordinator's Linear project creates (live/FR5-PLAN.md r1): the
// project's designated coordinator session creates a project in its lane's
// default team, through its person's own Linear profile. The server keeps
// the record: a reservation before any Linear call, settled with the
// project's ID after it, so a crash or a client that skips the settle
// still leaves the create on record, and the owner is told. Nothing here
// counts or caps creates (owner, Q-E). The record is the Linear write
// ledger's (linear_writes.go), kind project_create: its subject is empty,
// its result the Linear project's ID, its detail the kind's label ID.
const projectCreateKind = "project_create"

// projectCreateShown caps the overview's recent creates.
const projectCreateShown = 20

func projectCreateOperation(op string) bool {
	return op == "project_create_reserve" || op == "project_create_settle" || op == "project_create_list" || op == "project_create_ack"
}

// isProjectCoordinator reports whether actor is the project's active
// designated coordinator session. The dispatcher's system session is not:
// a project create is a person's own write.
func isProjectCoordinator(ctx context.Context, tx *transaction, pid string, actor map[string]any) (bool, error) {
	if actor == nil || actor["status"] != "active" || actor["role"] == "system" {
		return false, nil
	}
	p, err := one(ctx, tx, `SELECT coordinator_session FROM projects WHERE id=?`, pid)
	if err != nil {
		return false, err
	}
	return str(p, "coordinator_session") != "" && p["coordinator_session"] == actor["id"], nil
}

// isLinearCoordinator reports whether actor holds the coordinator's Linear
// rights (issue moves, project team adds, project creates) in the project.
// It is true when either:
//  1. isProjectCoordinator is true; or
//  2. the dispatcher is enabled, the project's coordinator_session is the
//     system dispatcher session, and actor, an active non-system session,
//     is some domain's coordinator session (same ID and same owner
//     principal as the domain records).
//
// While the dispatcher is on, no human session is the project coordinator,
// so each domain's coordinator session keeps these rights, inside its own
// lane: every call site still applies its own lane and team checks.
// Assignment powers (coordinatorMay, intake) stay with the project's
// designated coordinator (coordinator_session) alone, checked at those
// sites; while the dispatcher is on, that is the dispatcher. Project status
// is the designated coordinator's, unbounded, and a domain coordinator's,
// bounded by lane at the claim (projectStatusSetter).
// An unreadable domains document is an error, never a grant.
func isLinearCoordinator(ctx context.Context, tx *transaction, pid string, actor map[string]any) (bool, error) {
	if ok, err := isProjectCoordinator(ctx, tx, pid, actor); ok || err != nil {
		return ok, err
	}
	if actor == nil || actor["status"] != "active" || actor["role"] == "system" || str(actor, "id") == "" {
		return false, nil
	}
	// The dispatcher's seat first, so a project whose dispatcher is off never
	// reads (or trips on) the dispatcher document.
	seat, err := query(ctx, tx, `SELECT s.id FROM projects p JOIN sessions s ON s.id=p.coordinator_session WHERE p.id=? AND s.project_id=p.id AND s.role='system' AND s.name=?`, pid, DispatcherSessionName)
	if err != nil {
		return false, err
	}
	if len(seat) != 1 {
		return false, nil
	}
	domains, meta, err := readDispatcher(ctx, tx, pid)
	if err != nil {
		return false, err
	}
	if meta["enabled"] != true {
		return false, nil
	}
	for _, d := range domains.Domains {
		if ref := d.CoordinatorSession; ref != nil && ref.ID == actor["id"] && ref.Principal != "" && ref.Principal == actor["owner_principal"] {
			return true, nil
		}
	}
	return false, nil
}

func (s *Store) projectCreateOp(ctx context.Context, tx *transaction, w, p map[string]any, projectRole string, actor map[string]any, r Request) (any, error) {
	a := r.Args
	if a == nil {
		a = map[string]any{}
	}
	if r.Session == "" && str(a, "instance") != "" {
		return nil, errors.New("instance requires session")
	}
	wid, pid := w["id"].(string), p["id"].(string)
	nowRaw, err := dbStamp(ctx, tx)
	if err != nil {
		return nil, err
	}
	now := at(nowRaw)
	if err = pruneLinearWrites(ctx, tx, pid, now); err != nil {
		return nil, err
	}
	switch r.Operation {
	case "project_create_list":
		if err = s.tellStaleProjectCreates(ctx, tx, w, pid, now); err != nil {
			return nil, err
		}
		return projectCreatesView(ctx, tx, pid, now)
	case "project_create_ack":
		return s.ackProjectCreate(ctx, tx, wid, pid, projectRole, r, now)
	}
	if actor == nil {
		return nil, scoped("a project create is reserved and settled by the coordinator session that makes it", "forbidden")
	}
	if projectRole == "reader" {
		return nil, scoped("project member permission required", "forbidden")
	}
	op := str(a, "operation_id")
	if !commentOperationID.MatchString(op) {
		return nil, invalid("operation_id must be 1–128 letters, digits, '.', '_', ':' or '-'")
	}
	var out any
	if r.Operation == "project_create_reserve" {
		out, err = s.reserveProjectCreate(ctx, tx, wid, pid, actor, a, op, now)
	} else {
		out, err = s.settleProjectCreate(ctx, tx, w, pid, actor, a, op, now)
	}
	if err != nil {
		return nil, err
	}
	// Any coordinator call is a read that may find a stale reservation.
	if err = s.tellStaleProjectCreates(ctx, tx, w, pid, now); err != nil {
		return nil, err
	}
	return out, nil
}

// reserveProjectCreate records a create before any Linear call: the
// active designated coordinator only, in its lane's default team (the
// binding's default team without lanes), with a seeded kind of that team.
// A repeat with the same operation_id returns the same reservation.
func (s *Store) reserveProjectCreate(ctx context.Context, tx *transaction, wid, pid string, actor, a map[string]any, op string, now time.Time) (any, error) {
	ok, err := isLinearCoordinator(ctx, tx, pid, actor)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, scoped("only the project's designated coordinator session or, while the dispatcher is on, a domain's coordinator session may create a Linear project; the project owner creates one from their own terminal", "forbidden")
	}
	team, err := lowerUUID(a, "team_id", true)
	if err != nil {
		return nil, err
	}
	kind := str(a, "kind")
	name := str(a, "name_sha256")
	if !sha256Pattern.MatchString(name) {
		return nil, invalid("name_sha256 must be a lower-case SHA-256 hex digest")
	}
	b, linked, err := storedBinding(ctx, tx, pid)
	if err != nil {
		return nil, err
	}
	if !linked {
		return nil, invalid("the task board isn't linked; a project is created in a bound team")
	}
	home, lane := b.LinearDefaultTeamID, ""
	if len(b.Lanes) > 0 {
		if lane, _, _, err = s.sessionLane(ctx, tx, pid, actor); err != nil {
			return nil, err
		}
		l, exists := b.Lane(lane)
		if !exists {
			return nil, scoped("outside_lane: the coordinator session has no lane, so it creates no project; the project owner pins it with linear lanes pin", "forbidden")
		}
		home = l.DefaultTeamID
	}
	if team != home {
		return nil, scoped(fmt.Sprintf("outside_lane: a coordinator creates projects only in its lane's default team %s", home), "forbidden")
	}
	setting, err := readProjectKind(ctx, tx, pid)
	if err != nil {
		return nil, err
	}
	values, _ := setting["values"].([]ProjectKindValue)
	if len(values) == 0 {
		return nil, invalid("the project has no project kinds; the project owner sets them (linear kind set) and seeds their labels before a coordinator creates a project")
	}
	if setting["team_id"] != team {
		return nil, invalid("the project kinds belong to another team; a coordinator's project needs a kind of the team it is created in")
	}
	i := slices.IndexFunc(values, func(v ProjectKindValue) bool { return v.Name == kind })
	if i < 0 {
		return nil, invalid("kind must be one of the project's kinds (linear kind show)")
	}
	label := values[i].LabelID
	if label == "" || setting["group_label_id"] == nil {
		return nil, invalid("the kind's label isn't seeded in Linear yet; the project owner seeds it (linear kind seed)")
	}
	// The plan is the team, the kind and the name's digest: a repeat must
	// match them all. A not_sent one reached nothing, so it reopens.
	row, replay, err := reserveLinearWrite(ctx, tx, pid, linearWrite{Kind: projectCreateKind, Operation: op, Session: str(actor, "id"), Principal: s.principal, ByRole: "coordinator", Lane: lane, Team: team, Plan: team + "\n" + kind + "\n" + name, Detail: map[string]any{"kind_label_id": label}}, now)
	if err != nil {
		return nil, err
	}
	if replay {
		return reservationView(row, kind, label, true), nil
	}
	// History by IDs only: no name, no session name.
	if err = s.audit(ctx, tx, wid, pid, "", "project_create", op, "reserve", map[string]any{"team_id": team}); err != nil {
		return nil, err
	}
	return reservationView(row, kind, label, false), nil
}

// settleProjectCreate closes the reservation the same session made:
// created, with the Linear project's ID, which tells the owner; or
// not_sent, when nothing reached Linear. A settle repeats safely and never
// changes the project ID.
func (s *Store) settleProjectCreate(ctx context.Context, tx *transaction, w map[string]any, pid string, actor, a map[string]any, op string, now time.Time) (any, error) {
	row, err := linearWriteOwn(ctx, tx, pid, op, projectCreateKind, str(actor, "id"), s.principal)
	if err != nil {
		return nil, err
	}
	kinds, err := projectKindNames(ctx, tx, pid)
	if err != nil {
		return nil, err
	}
	kind := kinds[str(detailOf(row), "kind_label_id")]
	switch str(a, "outcome") {
	case "created":
		project, err := lowerUUID(a, "linear_project_id", true)
		if err != nil {
			return nil, err
		}
		row, replay, err := settleLinearWrite(ctx, tx, pid, row, "accepted", project, nil, now)
		if err != nil || replay {
			if err != nil {
				return nil, err
			}
			return reservationView(row, kind, "", true), nil
		}
		if err = s.audit(ctx, tx, w["id"].(string), pid, "", "project_create", op, "created", map[string]any{"team_id": row["team_id"], "linear_project_id": project}); err != nil {
			return nil, err
		}
		body := fmt.Sprintf("Coordinator session %s created Linear project %s in team %s (kind %s), operation %s. No lead was set. Archive or delete stays yours, in Linear.", str(actor, "name"), project, str(row, "team_id"), kind, op)
		told, err := s.tellProjectOwners(ctx, tx, w, pid, "project-created", body, now)
		if err != nil {
			return nil, err
		}
		out := reservationView(row, kind, "", false)
		out["owner_sessions_told"] = told
		return out, nil
	case "not_sent":
		if a["linear_project_id"] != nil {
			return nil, invalid("a not_sent outcome has no linear_project_id")
		}
		row, replay, err := settleLinearWrite(ctx, tx, pid, row, "not_sent", "", nil, now)
		if err != nil || replay {
			if err != nil {
				return nil, err
			}
			return reservationView(row, kind, "", true), nil
		}
		if err = s.audit(ctx, tx, w["id"].(string), pid, "", "project_create", op, "not_sent", map[string]any{"team_id": row["team_id"]}); err != nil {
			return nil, err
		}
		return reservationView(row, kind, "", false), nil
	}
	return nil, invalid("outcome must be created or not_sent")
}

// ackProjectCreate is the owner's acknowledgement of an unsettled or
// uncertain reservation, or of a team add Linear answered with other
// fields changed: it stops showing, and is pruned with the settled ones.
// A clean accepted write has nothing to acknowledge.
func (s *Store) ackProjectCreate(ctx context.Context, tx *transaction, wid, pid, projectRole string, r Request, now time.Time) (any, error) {
	if r.Session != "" {
		return nil, scoped("a session can't acknowledge a Linear write; the project owner, or the project lead who confirmed it, does, in their own terminal", "forbidden")
	}
	if projectRole == "reader" {
		return nil, scoped("project owner permission required, or the project lead who confirmed the write", "forbidden")
	}
	if s.postgres && (s.identity == nil || s.identity.Issuer != DeviceIssuer || principal.CI(s.principal)) {
		return nil, scoped("a Linear write is acknowledged only from a person's own signed-in device", "forbidden")
	}
	op := str(r.Args, "operation_id")
	open := `project_id=? AND operation_id=? AND (state IN ('reserved','uncertain') OR (kind IN (?,?) AND state='accepted' AND detail LIKE ?)) AND acknowledged_at IS NULL`
	found, err := query(ctx, tx, `SELECT * FROM linear_write_reservations WHERE `+open, pid, op, projectTeamAddKind, projectPlanKind, `%"changed":[%`)
	if err != nil {
		return nil, err
	}
	if len(found) != 1 {
		// A member who isn't a lead learns nothing of other writes: the
		// refusal is the one before FR13 let a team's lead acknowledge.
		if projectRole != "owner" && projectRole != "lead" {
			return nil, scoped("project owner permission required, or the project lead who confirmed the write", "forbidden")
		}
		return nil, &serviceError{code: "not_found", cause: errors.New("the project has no unacknowledged, unsettled Linear write, or team add Linear answered with other changes, with that operation_id"), public: true}
	}
	row := found[0]
	// The project owner acknowledges any write; a project lead only one
	// they confirmed themselves, as a lead. A coordinator's write is the
	// owner's to acknowledge. A project plan (FR13) a team's lead confirmed,
	// in their terminal or from a session they own, is also that lead's to
	// acknowledge, in their own terminal (a session never acknowledges).
	if projectRole != "owner" && !ownAck(row, projectRole, s.principal) {
		if projectRole != "lead" {
			return nil, scoped("project owner permission required, or the project lead who confirmed the write", "forbidden")
		}
		return nil, scoped("only the project owner, or the project lead who confirmed this write, acknowledges it", "forbidden")
	}
	// A family's operation acknowledges its children too, those still
	// open; a child's own operation ID acknowledges that child alone.
	ops := []string{op}
	if str(row, "kind") == issueMoveKind {
		kids, _ := detailOf(row)["children"].([]any)
		for _, kid := range kids {
			child, _ := kid.(string)
			if child == "" {
				continue
			}
			c, err := query(ctx, tx, `SELECT operation_id FROM linear_write_reservations WHERE `+open+` AND kind=?`, pid, childOperation(op, child), projectTeamAddKind, projectPlanKind, `%"changed":[%`, issueMoveChildKind)
			if err != nil {
				return nil, err
			}
			if len(c) == 1 {
				ops = append(ops, str(c[0], "operation_id"))
			}
		}
	}
	for _, o := range ops {
		if _, err = tx.ExecContext(ctx, `UPDATE linear_write_reservations SET acknowledged_at=? WHERE project_id=? AND operation_id=? AND acknowledged_at IS NULL`, layout(now), pid, o); err != nil {
			return nil, err
		}
	}
	if err = s.audit(ctx, tx, wid, pid, "", "project_create", op, "acknowledged", map[string]any{"operation_ids": ops, "by": projectRole}); err != nil {
		return nil, err
	}
	view, err := projectCreatesView(ctx, tx, pid, now)
	if err != nil {
		return nil, err
	}
	view["acknowledged"] = ops
	return view, nil
}

// ownAck reports whether a caller who isn't the owner may acknowledge row
// as its confirmer: a project lead their own lead (or FR13 project_lead)
// row, and a team's lead their own project plan.
func ownAck(row map[string]any, projectRole, who string) bool {
	if str(row, "principal") != who {
		return false
	}
	switch str(row, "by_role") {
	case "lead":
		return projectRole == "lead"
	case "project_lead":
		return projectRole == "lead" && str(row, "kind") == projectPlanKind
	case "team_lead", "team_lead_session":
		return str(row, "kind") == projectPlanKind
	}
	return false
}

// tellStaleProjectCreates tells the owner's sessions, once, of each
// Linear write, of any kind, unsettled for linearWriteStale and not
// acknowledged. The claim (stale_notified_at) is set in the same
// transaction as the message, so it never repeats; with no owner session
// to tell, it waits.
func (s *Store) tellStaleProjectCreates(ctx context.Context, tx *transaction, w map[string]any, pid string, now time.Time) error {
	rows, err := staleLinearWrites(ctx, tx, pid, now.Add(-linearWriteStale))
	if err != nil {
		return err
	}
	kinds, err := projectKindNames(ctx, tx, pid)
	if err != nil {
		return err
	}
	for _, row := range rows {
		if row["stale_notified_at"] != nil {
			continue
		}
		op := str(row, "operation_id")
		body := fmt.Sprintf("A coordinator Linear write may have happened, unconfirmed: %s operation %s, reserved in team %s at %s, never settled. Check Linear; clawbridge linear writes --ack %s once you have.", str(row, "kind"), op, str(row, "team_id"), str(row, "reserved_at"), op)
		if str(row, "state") == "uncertain" {
			body = fmt.Sprintf("A coordinator Linear write may have happened, uncertain: %s operation %s in team %s; its read-back couldn't tell whether Linear took it. Check Linear; clawbridge linear writes --ack %s once you have.", str(row, "kind"), op, str(row, "team_id"), op)
		}
		if str(row, "kind") == projectCreateKind {
			body = fmt.Sprintf("A coordinator create may have happened, unconfirmed: operation %s reserved a Linear project in team %s (kind %s) at %s and never settled. Check Linear; clawbridge linear writes --ack %s once you have.", op, str(row, "team_id"), kinds[str(detailOf(row), "kind_label_id")], str(row, "reserved_at"), op)
		}
		told, err := s.tellProjectOwners(ctx, tx, w, pid, "project-create-unconfirmed", body, now)
		if err != nil {
			return err
		}
		if told == 0 {
			continue
		}
		if _, err = tx.ExecContext(ctx, `UPDATE linear_write_reservations SET stale_notified_at=? WHERE project_id=? AND operation_id=? AND stale_notified_at IS NULL`, layout(now), pid, op); err != nil {
			return err
		}
	}
	return nil
}

// tellProjectOwners queues one message from the system session to each
// active session of the project's owners, and returns how many it reached.
// A local store has no project roles: its workspace owner owns all.
func (s *Store) tellProjectOwners(ctx context.Context, tx *transaction, w map[string]any, pid, kind, body string, now time.Time) (int, error) {
	owns, args := `owner_principal=?`, []any{pid, str(w, "owner_principal")}
	if tx.postgres {
		owns = `(owner_principal=? OR EXISTS(SELECT 1 FROM project_roles r WHERE r.project_id=sessions.project_id AND r.principal=sessions.owner_principal AND r.role='owner'))`
	}
	to, err := query(ctx, tx, `SELECT id,name FROM sessions WHERE project_id=? AND status='active' AND role<>'system' AND `+owns+` ORDER BY id`, args...)
	if err != nil || len(to) == 0 {
		return 0, err
	}
	wid := w["id"].(string)
	if err = s.ensureSystemSession(ctx, tx, wid, pid); err != nil {
		return 0, err
	}
	system, err := one(ctx, tx, `SELECT id FROM sessions WHERE project_id=? AND role='system' AND name=?`, pid, SystemSessionName)
	if err != nil {
		return 0, err
	}
	retention := asInt(w["retention_days"])
	if retention < 1 {
		retention = 30
	}
	stamp := layout(now)
	for _, c := range to {
		id := newID("msg_")
		if _, err = tx.ExecContext(ctx, `INSERT INTO messages(id,project_id,source_project_id,sender_id,address,thread_id,reply_to,kind,body,created_at,expires_at,created_by_principal,operation_id,payload_hash,coordination) VALUES(?,?,?,?,?,?,NULL,?,?,?,?,?,?,?,NULL)`,
			id, pid, pid, system["id"], str(c, "name"), id, kind, body, stamp, layout(now.AddDate(0, 0, retention)), systemPrincipal, newID("pcreate_"), digestOf(body)); err != nil {
			return 0, err
		}
		if err = insertDeliveries(ctx, tx, pid, id, []string{str(c, "id")}); err != nil {
			return 0, err
		}
	}
	return len(to), nil
}

// projectKindNames maps the project's kind label IDs to their names, for
// what the owner reads; the ledger keeps only the label's ID.
func projectKindNames(ctx context.Context, tx *transaction, pid string) (map[string]string, error) {
	setting, err := readProjectKind(ctx, tx, pid)
	if err != nil {
		return nil, err
	}
	out := map[string]string{}
	values, _ := setting["values"].([]ProjectKindValue)
	for _, v := range values {
		if v.LabelID != "" {
			out[v.LabelID] = v.Name
		}
	}
	return out, nil
}

// projectCreateState is a create's state as its callers read it: settled
// accepted is created.
func projectCreateState(row map[string]any) string {
	if state := str(row, "state"); state != "accepted" {
		return state
	}
	return "created"
}

func reservationView(row map[string]any, kind, label string, replay bool) map[string]any {
	out := map[string]any{"operation_id": row["operation_id"], "team_id": row["team_id"], "kind": kind, "state": projectCreateState(row), "linear_project_id": row["result"], "reserved_at": row["reserved_at"], "settled_at": row["settled_at"], "replay": replay}
	if label != "" {
		out["kind_label_id"] = label
	}
	return out
}

// projectCreatesView lists a project's coordinator creates, the recent
// ones settled as created, and its unconfirmed Linear writes of every
// kind (unsettled past linearWriteStale, or settled uncertain; not
// acknowledged), each with its kind (write) and state. Session names and principals stay out; the IDs are
// enough to look in Linear.
func projectCreatesView(ctx context.Context, tx *transaction, pid string, now time.Time) (map[string]any, error) {
	kinds, err := projectKindNames(ctx, tx, pid)
	if err != nil {
		return nil, err
	}
	rows, err := query(ctx, tx, `SELECT operation_id,team_id,result,detail,settled_at FROM linear_write_reservations WHERE project_id=? AND kind=? AND state='accepted' ORDER BY settled_at DESC,operation_id LIMIT ?`, pid, projectCreateKind, projectCreateShown)
	if err != nil {
		return nil, err
	}
	created := []map[string]any{}
	for _, row := range rows {
		created = append(created, map[string]any{"operation_id": row["operation_id"], "team_id": row["team_id"], "kind": kinds[str(detailOf(row), "kind_label_id")], "linear_project_id": row["result"], "settled_at": row["settled_at"]})
	}
	stale, err := staleLinearWrites(ctx, tx, pid, now.Add(-linearWriteStale))
	if err != nil {
		return nil, err
	}
	unconfirmed := []map[string]any{}
	for _, row := range stale {
		u := map[string]any{"operation_id": row["operation_id"], "write": row["kind"], "state": row["state"], "team_id": row["team_id"], "reserved_at": row["reserved_at"], "stale_notified_at": row["stale_notified_at"]}
		if row["subject"] != "" {
			u["subject"] = row["subject"]
		}
		if row["kind"] == projectCreateKind {
			u["kind"] = kinds[str(detailOf(row), "kind_label_id")]
		}
		unconfirmed = append(unconfirmed, u)
	}
	return map[string]any{"created": created, "unconfirmed": unconfirmed, "keep_days": linearWriteKeepDays}, nil
}
