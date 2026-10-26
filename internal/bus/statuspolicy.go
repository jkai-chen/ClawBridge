package bus

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"
)

// Schema 14: the project owner's status policy, which moves an agent's
// issue forward through the team's workflow (live/STATUS-FLOW-DESIGN.md),
// and the claim marker's third part, state, that applies it. The same DDL
// serves SQLite and PostgreSQL.
const statusPolicySchemaV14 = `
CREATE TABLE linear_status_policy(project_id TEXT PRIMARY KEY REFERENCES projects(id),revision INTEGER NOT NULL,policy TEXT NOT NULL,updated_at TEXT NOT NULL,updated_by_principal TEXT NOT NULL,updated_by_session TEXT);
ALTER TABLE assignment_markers ADD COLUMN state_state TEXT NOT NULL DEFAULT 'off' CHECK(state_state IN ('off','pending','in_flight','done','skipped','superseded','failed_out_of_binding'));
ALTER TABLE assignment_markers ADD COLUMN state_policy_revision INTEGER NOT NULL DEFAULT 0;
ALTER TABLE assignment_markers ADD COLUMN state_target TEXT NOT NULL DEFAULT '';
ALTER TABLE assignment_markers ADD COLUMN state_landed TEXT NOT NULL DEFAULT '';
ALTER TABLE assignment_markers ADD COLUMN state_skip_reason TEXT NOT NULL DEFAULT '';
ALTER TABLE assignment_markers ADD COLUMN outcome TEXT NOT NULL DEFAULT '' CHECK(outcome IN ('','done'));
`

const (
	maxStatusTeams    = 20
	maxStatusWorkflow = 200
	maxStatusCloses   = 50
)

// StatusPolicy is a project's status policy, resolved per bound team when
// it is set: each target's state ID, name and type, and the team's whole
// workflow as state ID → type, so a state added later is unknown and moves
// nothing. Closes says who closes each bound Linear project's issues
// (A2): agent, or human, the default for every project not listed.
type StatusPolicy struct {
	Enabled         bool                  `json:"enabled"`
	ClaimFromTriage bool                  `json:"claim_from_triage"`
	Teams           map[string]StatusTeam `json:"teams"`
	Closes          map[string]string     `json:"closes,omitempty"`
}

// AgentCloses reports whether agents close the issues of a Linear project:
// only one the policy names agent. An issue in no project is a person's to
// close.
func (p StatusPolicy) AgentCloses(project string) bool {
	return project != "" && p.Closes[project] == "agent"
}

// StatusTeam is one team's targets and workflow snapshot. Done is the
// completed state a done-release moves to in a project agents close.
type StatusTeam struct {
	Create      *StatusState      `json:"create,omitempty"`
	Claim       *StatusState      `json:"claim,omitempty"`
	ReleaseDone *StatusState      `json:"release_done,omitempty"`
	Done        *StatusState      `json:"done,omitempty"`
	Workflow    map[string]string `json:"workflow"`
}

// StatusState is one resolved workflow state.
type StatusState struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Type string `json:"type"`
}

var statusTypes = map[string]bool{"triage": true, "backlog": true, "unstarted": true, "started": true, "completed": true, "canceled": true}

var lowerUUIDPattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// ParseStatusPolicy decodes a policy strictly and checks its rules: create
// is a backlog or unstarted state, claim and release_done are started
// states, done alone is a completed state, nothing is ever canceled, and
// release_done and done need a claim, since a release moves only from the
// claim state. Closes names canonical project UUIDs, agent or human.
func ParseStatusPolicy(v any) (StatusPolicy, error) {
	raw, err := json.Marshal(v)
	if err != nil || len(raw) > 256<<10 {
		return StatusPolicy{}, invalid("status policy must be a JSON object under 256 KiB")
	}
	var p StatusPolicy
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if dec.Decode(&p) != nil || dec.Decode(new(any)) != io.EOF {
		return StatusPolicy{}, invalid("status policy must be {\"enabled\", \"claim_from_triage\", \"teams\": {team: {create, claim, release_done, done, workflow}}, \"closes\": {project: agent|human}}")
	}
	if p.Teams == nil {
		p.Teams = map[string]StatusTeam{}
	}
	if len(p.Teams) > maxStatusTeams {
		return StatusPolicy{}, invalid(fmt.Sprintf("status policy holds at most %d teams", maxStatusTeams))
	}
	for team, t := range p.Teams {
		if !lowerUUIDPattern.MatchString(team) {
			return StatusPolicy{}, invalid("status policy teams are canonical team UUIDs")
		}
		if len(t.Workflow) == 0 || len(t.Workflow) > maxStatusWorkflow {
			return StatusPolicy{}, invalid(fmt.Sprintf("a team's workflow holds 1–%d states", maxStatusWorkflow))
		}
		for id, kind := range t.Workflow {
			if !lowerUUIDPattern.MatchString(id) || !statusTypes[kind] {
				return StatusPolicy{}, invalid("a workflow maps state UUIDs to triage, backlog, unstarted, started, completed or canceled")
			}
		}
		for _, target := range []struct {
			field string
			state *StatusState
			types []string
		}{{"create", t.Create, []string{"backlog", "unstarted"}}, {"claim", t.Claim, []string{"started"}}, {"release_done", t.ReleaseDone, []string{"started"}}, {"done", t.Done, []string{"completed"}}} {
			if target.state == nil {
				continue
			}
			s := *target.state
			if !lowerUUIDPattern.MatchString(s.ID) || t.Workflow[s.ID] == "" || t.Workflow[s.ID] != s.Type {
				return StatusPolicy{}, invalid(fmt.Sprintf("%s must be a state of the team's workflow, with its type", target.field))
			}
			if err := policyName(s.Name); err != nil {
				return StatusPolicy{}, invalid(err.Error())
			}
			if !containsString(target.types, s.Type) {
				return StatusPolicy{}, invalid(fmt.Sprintf("%s must be a %v state, never canceled", target.field, target.types))
			}
		}
		if t.ReleaseDone != nil && (t.Claim == nil || t.Claim.ID == t.ReleaseDone.ID) {
			return StatusPolicy{}, invalid("release_done moves only from the claim state: set a different claim state too")
		}
		if t.Done != nil && t.Claim == nil {
			return StatusPolicy{}, invalid("done moves only from the claim state: set a claim state too")
		}
	}
	if len(p.Closes) > maxStatusCloses {
		return StatusPolicy{}, invalid(fmt.Sprintf("closes names at most %d projects", maxStatusCloses))
	}
	for project, who := range p.Closes {
		if !lowerUUIDPattern.MatchString(project) || (who != "agent" && who != "human") {
			return StatusPolicy{}, invalid("closes maps canonical project UUIDs to agent or human")
		}
	}
	return p, nil
}

func statusPolicyOperation(op string) bool { return op == "status_policy_set" }

// statusPolicyOp sets the policy: the project owner only, without a session
// (the CLI) or from an active one. Reading it (status_policy_get) is open
// to every member and handled before sessions.
func (s *Store) statusPolicyOp(ctx context.Context, tx *transaction, w, p map[string]any, projectRole string, actor map[string]any, r Request) (any, error) {
	wid, pid := w["id"].(string), p["id"].(string)
	if projectRole != "owner" {
		return nil, errors.New("project owner permission required")
	}
	if actor != nil && actor["status"] != "active" {
		return nil, errors.New("session is retired or moved; it cannot perform this operation")
	}
	stored, current, err := readStatusPolicy(ctx, tx, pid)
	if err != nil {
		return nil, err
	}
	expected, err := integer(r.Args["expected_revision"])
	if err != nil || r.Args["expected_revision"] == nil {
		return nil, invalid("status_policy_set requires expected_revision")
	}
	if current != int64(expected) {
		return nil, conflict(fmt.Sprintf("status policy changed: current revision is %d", current))
	}
	policy, err := ParseStatusPolicy(r.Args["status_policy"])
	if err != nil {
		return nil, err
	}
	via := str(r.Args, "via")
	if via != "" && !closesVia[via] {
		return nil, invalid("via is cli or cli-tty")
	}
	// R89-H1: who closes is never a session's to change, even the owner's
	// own session's: an agent could grant itself the right to close.
	if actor != nil && !closesEqual(stored, policy) {
		return nil, errors.New("a session can never change who closes issues; the project owner runs status-policy set without --session")
	}
	// R89-M1: letting agents close a project is confirmed at a terminal,
	// whichever command writes it.
	if loosened := closesLoosened(stored, policy); len(loosened) > 0 && via != "cli-tty" {
		return nil, invalid("letting agents close issues is confirmed at a terminal, by typing each project's name (" + strings.Join(loosened, ", ") + ")")
	}
	encoded, _ := json.Marshal(policy)
	now, err := dbStamp(ctx, tx)
	if err != nil {
		return nil, err
	}
	var session any
	if actor != nil {
		session = actor["id"]
	}
	revision := int64(expected) + 1
	if expected == 0 {
		_, err = tx.ExecContext(ctx, `INSERT INTO linear_status_policy(project_id,revision,policy,updated_at,updated_by_principal,updated_by_session) VALUES(?,?,?,?,?,?)`, pid, revision, string(encoded), now, s.principal, session)
	} else {
		var result interface{ RowsAffected() (int64, error) }
		result, err = tx.ExecContext(ctx, `UPDATE linear_status_policy SET revision=?,policy=?,updated_at=?,updated_by_principal=?,updated_by_session=? WHERE project_id=? AND revision=?`, revision, string(encoded), now, s.principal, session, pid, expected)
		if err == nil {
			if n, _ := result.RowsAffected(); n != 1 {
				err = conflict("status policy changed concurrently; read it again")
			}
		}
	}
	if err != nil {
		return nil, err
	}
	if err = s.audit(ctx, tx, wid, pid, "", "linear_status_policy", pid, "set", auditVia(map[string]any{"revision": revision, "enabled": policy.Enabled, "teams": len(policy.Teams)}, via)); err != nil {
		return nil, err
	}
	return statusPolicyView(ctx, tx, pid)
}

// readStatusPolicy is the project's policy and its revision; with none,
// an empty, disabled policy at revision 0.
func readStatusPolicy(ctx context.Context, tx *transaction, pid string) (StatusPolicy, int64, error) {
	row, err := one(ctx, tx, `SELECT revision,policy FROM linear_status_policy WHERE project_id=?`, pid)
	if errors.Is(err, errNotFound) {
		return StatusPolicy{Teams: map[string]StatusTeam{}}, 0, nil
	}
	if err != nil {
		return StatusPolicy{}, 0, err
	}
	var stored any
	if json.Unmarshal([]byte(fmt.Sprint(row["policy"])), &stored) != nil {
		return StatusPolicy{}, 0, errors.New("stored status policy is unreadable")
	}
	policy, err := ParseStatusPolicy(stored)
	if err != nil {
		return StatusPolicy{}, 0, errors.New("stored status policy is unreadable")
	}
	return policy, int64(asInt(row["revision"])), nil
}

func statusPolicyView(ctx context.Context, tx *transaction, pid string) (map[string]any, error) {
	policy, revision, err := readStatusPolicy(ctx, tx, pid)
	if err != nil {
		return nil, err
	}
	out := map[string]any{"revision": revision, "enabled": policy.Enabled, "claim_from_triage": policy.ClaimFromTriage, "teams": policy.Teams, "updated_at": nil, "updated_by": nil}
	if len(policy.Closes) > 0 {
		out["closes"] = policy.Closes
	}
	if revision > 0 {
		row, err := one(ctx, tx, `SELECT updated_at,updated_by_principal FROM linear_status_policy WHERE project_id=?`, pid)
		if err != nil {
			return nil, err
		}
		out["updated_at"], out["updated_by"] = row["updated_at"], row["updated_by_principal"]
	}
	return out, nil
}

// statusMoves reports whether an enabled policy has a target of the given
// kind (claim, or release_done: its release_done or done) for any team.
func (p StatusPolicy) statusMoves(kind string) bool {
	if !p.Enabled {
		return false
	}
	for _, t := range p.Teams {
		if kind == "claim" && t.Claim != nil || kind == "release_done" && (t.ReleaseDone != nil || t.Done != nil) {
			return true
		}
	}
	return false
}

// markerStatePart is the state part a new marker row starts with, and the
// policy revision it is frozen at: pending only while the policy moves
// issues for that phase.
func markerStatePart(ctx context.Context, tx *transaction, pid, kind string) (string, int64, error) {
	policy, revision, err := readStatusPolicy(ctx, tx, pid)
	if err != nil {
		return "", 0, err
	}
	if !policy.statusMoves(kind) {
		return "off", 0, nil
	}
	return "pending", revision, nil
}

// statusSkipReasons are the only words a skipped state part records.
var statusSkipReasons = map[string]bool{"from_state": true, "team_unbound": true, "team_off": true, "target_unknown": true, "policy_off": true, "policy_changed": true, "state_unresolved": true}

// stateGo hands out a state move. The policy must still be enabled and at
// the revision the row froze (SF-M1); the applier checks the issue's team,
// the target and the current state against it. A claim move never started
// is superseded once a close exists, and a close move waits until the
// claim's has settled, then carries how it settled.
func (m markerCall) stateGo(ctx context.Context, out func(string) map[string]any, wait func() (any, error)) (any, error) {
	if m.phase == "claim" && m.close != nil && str(m.row, "state_state") == "pending" {
		o := out("none")
		o["state"] = "superseded"
		return o, m.set(ctx, "claim", "state_state", "superseded")
	}
	if m.phase == "close" && m.claim != nil {
		if s := str(m.claim, "state_state"); s == "pending" || s == "in_flight" {
			return wait()
		}
	}
	policy, revision, err := readStatusPolicy(ctx, m.tx, m.pid)
	if err != nil {
		return nil, err
	}
	reason := ""
	switch {
	case !policy.Enabled:
		reason = "policy_off"
	case revision != int64(asInt(m.row["state_policy_revision"])):
		reason = "policy_changed"
	}
	if reason != "" {
		if _, err = m.tx.ExecContext(ctx, `UPDATE assignment_markers SET state_state='skipped',state_skip_reason=?,updated_at=? WHERE assignment_id=? AND phase=?`, reason, layout(m.now), m.assignment, m.phase); err != nil {
			return nil, err
		}
		o := out("none")
		o["state"], o["reason"] = "skipped", reason
		return o, nil
	}
	if err = m.set(ctx, m.phase, "state_state", "in_flight"); err != nil {
		return nil, err
	}
	o := out("move")
	o["kind"] = "claim"
	if m.phase == "close" {
		o["kind"] = "release_done"
		// A1: the claim's settled state part and the revision it froze, so
		// a release that overtook its claim may move from the claim's
		// from-set, under the same policy only.
		if m.claim != nil {
			o["claim_state"] = str(m.claim, "state_state")
			o["claim_policy_revision"] = int64(asInt(m.claim["state_policy_revision"]))
		}
	}
	o["policy"], o["policy_revision"] = policy, revision
	return o, nil
}

// stateSettle records a state move: done with the target and the state that
// landed (SF-L3: whatever landed is final, never sent again), skipped with
// its reason, or a failure retried with backoff. An uncertain move stays in
// flight; its retry reads the issue again first (SF-L5).
func (m markerCall) stateSettle(ctx context.Context, outcome string) (any, error) {
	if str(m.row, "state_state") != "in_flight" {
		return nil, scoped("this marker part was not handed out; ask marker_go first", "conflict")
	}
	now := layout(m.now)
	state, target, landed, reason := "", "", "", ""
	switch outcome {
	case "done":
		var err error
		if target, err = lowerUUID(m.a, "state_target", true); err != nil {
			return nil, err
		}
		if landed, err = lowerUUID(m.a, "state_landed", true); err != nil {
			return nil, err
		}
		state = "done"
	case "skipped":
		reason = str(m.a, "skip_reason")
		if !statusSkipReasons[reason] {
			return nil, invalid("skip_reason must be from_state, team_unbound, team_off, target_unknown, policy_off or policy_changed")
		}
		state = "skipped"
	case "failed", "out_of_binding":
		state = "pending"
	case "uncertain":
		state = "in_flight"
	default:
		return nil, invalid("a state part settles done, skipped, failed, uncertain or out_of_binding")
	}
	attempts := asInt(m.row["attempts"])
	next := now
	if state == "pending" || state == "in_flight" {
		attempts++
		next = layout(m.now.Add(markerBackoff(attempts)))
		if outcome == "out_of_binding" && attempts >= markerMaxOutside {
			state = "failed_out_of_binding"
		}
	}
	failure := str(m.a, "error")
	if len(failure) > 200 {
		failure = failure[:200]
	}
	if _, err := m.tx.ExecContext(ctx, `UPDATE assignment_markers SET state_state=?,state_target=?,state_landed=?,state_skip_reason=?,attempts=?,next_attempt_at=?,last_error=?,updated_at=? WHERE assignment_id=? AND phase=?`, state, target, landed, reason, attempts, next, failure, now, m.assignment, m.phase); err != nil {
		return nil, err
	}
	return map[string]any{"assignment_id": m.assignment, "phase": m.phase, "part": m.part, "state": state, "next_attempt_at": next}, nil
}
