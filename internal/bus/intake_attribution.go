package bus

import (
	"context"
	"strings"
	"time"

	"clawbridge/internal/principal"
)

// Intake attribution (live/INTAKE-ATTRIBUTION-PLAN.md, reviewer A1–A5): on
// a hosted server every agent write runs as the ClawBridge app, so Linear's
// history names the app for a label a person added through ClawBridge. The
// server records who asked for each accepted app write that added a label or
// posted a comment; intake credits a signal to a trusted person only when
// that record is the one write in the signal's window and the person made
// it from their own signed-in device, with no session and no agent marker.
const intakeAttributionSchema = `
CREATE TABLE app_write_attributions(project_id TEXT NOT NULL REFERENCES projects(id),issue_uuid TEXT NOT NULL,kind TEXT NOT NULL CHECK(kind IN ('label','comment')),target_id TEXT NOT NULL,principal TEXT NOT NULL,session TEXT NOT NULL DEFAULT '',issuer TEXT NOT NULL,agent_marker TEXT NOT NULL DEFAULT '',creditable INTEGER NOT NULL CHECK(creditable IN (0,1)),operation_id TEXT NOT NULL DEFAULT '',written_at TEXT NOT NULL);
CREATE INDEX app_write_attributions_lookup ON app_write_attributions(project_id,issue_uuid,kind,target_id,written_at);
CREATE INDEX app_write_attributions_age ON app_write_attributions(project_id,written_at);
ALTER TABLE intake_trusted ADD COLUMN principal TEXT;
`

// attributionRetention is how long an attribution is kept: intake only
// looks back over its poll windows.
const attributionRetention = 7 * 24 * time.Hour

// Attribution windows (A5): Linear's createdAt follows the send, so a
// history entry or notification matches a write from 10 s before it was
// recorded to 120 s after.
const (
	AttributionBefore = 10 * time.Second
	AttributionAfter  = 120 * time.Second
)

// MarkerUnsent stands for a client that sent no agent_marker argument: an
// older client, whose write is never creditable.
const MarkerUnsent = "unsent"

// AppWrite is one accepted app write, as the server records it. Workspace
// and Project are as the request named them.
type AppWrite struct {
	Workspace, Project string
	Issue              string
	// Labels are the label IDs the write added; Comment the comment it
	// posted.
	Labels      []string
	Comment     string
	Identity    Identity
	Session     string
	AgentMarker string
	OperationID string
}

// creditable is A2: a person's own signed-in device, not CI, with no
// session and no agent marker.
func (w AppWrite) creditable() bool {
	return w.Identity.Issuer == DeviceIssuer && !principal.CI(w.Identity.Principal) && w.Session == "" && w.AgentMarker == ""
}

// RecordAppWrite records who asked for an accepted app write (A1: every
// one, sessioned too, so a session's write is never invisible). It is the
// server's own record, made after the receipt was verified, and no client
// operation reaches it (A3). A write to a project with no task board
// binding records nothing: intake watches no such project.
func (s *Store) RecordAppWrite(ctx context.Context, w AppWrite) error {
	issue := strings.ToLower(w.Issue)
	if !issueUUIDPattern.MatchString(issue) || (len(w.Labels) == 0 && w.Comment == "") {
		return nil
	}
	pid, err := s.WatchProjectID(ctx, w.Workspace, w.Project)
	if err != nil || pid == "" {
		return err
	}
	tx, err := s.beginIntake(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	now, err := dbStamp(ctx, tx)
	if err != nil {
		return err
	}
	creditable := 0
	if w.creditable() {
		creditable = 1
	}
	insert := func(kind, target string) error {
		_, e := tx.ExecContext(ctx, `INSERT INTO app_write_attributions(project_id,issue_uuid,kind,target_id,principal,session,issuer,agent_marker,creditable,operation_id,written_at) VALUES(?,?,?,?,?,?,?,?,?,?,?)`, pid, issue, kind, strings.ToLower(target), normalPrincipal(w.Identity.Principal), w.Session, w.Identity.Issuer, w.AgentMarker, creditable, w.OperationID, now)
		return e
	}
	seen := map[string]bool{}
	for _, label := range w.Labels {
		label = strings.ToLower(label)
		if label == "" || seen[label] {
			continue
		}
		seen[label] = true
		if err = insert("label", label); err != nil {
			return err
		}
	}
	if w.Comment != "" {
		if err = insert("comment", w.Comment); err != nil {
			return err
		}
	}
	if _, err = tx.ExecContext(ctx, `DELETE FROM app_write_attributions WHERE project_id=? AND written_at<?`, pid, layout(at(now).Add(-attributionRetention))); err != nil {
		return err
	}
	return tx.Commit()
}

// AttributionCredit is what intake makes of the app writes in a signal's
// window. Result is "" when there are none, "credited" with the trusted
// Linear user (Actor) and the principal who asked, or a skip reason:
// attribution_ambiguous for several writes or a session's or agent's write
// among them, attributed_untrusted for one write that no trusted, linked
// project member made.
type AttributionCredit struct {
	Result    string
	Actor     string
	Principal string
}

// IntakeAttribution reads the app writes of kind and target on issue
// recorded between from and to, and credits them (A1, A4). Credit needs
// exactly one write, creditable; its principal still a member of the
// project, and linked to exactly one Linear user still on the trusted list.
func (s *Store) IntakeAttribution(ctx context.Context, pid, issue, kind, target string, from, to time.Time) (AttributionCredit, error) {
	if kind != "label" && kind != "comment" {
		return AttributionCredit{}, invalid("kind must be label or comment")
	}
	tx, err := s.beginIntake(ctx)
	if err != nil {
		return AttributionCredit{}, err
	}
	defer tx.Rollback()
	rows, err := query(ctx, tx, `SELECT principal,creditable FROM app_write_attributions WHERE project_id=? AND issue_uuid=? AND kind=? AND target_id=? AND written_at>=? AND written_at<=? ORDER BY written_at`, pid, strings.ToLower(issue), kind, strings.ToLower(target), layout(from), layout(to))
	if err != nil {
		return AttributionCredit{}, err
	}
	switch {
	case len(rows) == 0:
		return AttributionCredit{}, nil
	case len(rows) > 1:
		return AttributionCredit{Result: "attribution_ambiguous"}, nil
	}
	who := str(rows[0], "principal")
	if asInt(rows[0]["creditable"]) != 1 {
		// One write, a session's or an agent's: never a person's.
		return AttributionCredit{Result: "attributed_untrusted", Principal: who}, nil
	}
	untrusted := AttributionCredit{Result: "attributed_untrusted", Principal: who}
	if member, err := projectMember(ctx, tx, pid, who); err != nil || !member {
		return untrusted, err
	}
	linked, err := query(ctx, tx, `SELECT linear_user_id FROM intake_trusted WHERE project_id=? AND principal=?`, pid, who)
	if err != nil || len(linked) != 1 {
		return untrusted, err
	}
	return AttributionCredit{Result: "credited", Actor: str(linked[0], "linear_user_id"), Principal: who}, nil
}

// projectMember is whether p holds the project now: its workspace's owner,
// or, on a hosted server, a principal with a project role. A local
// workspace has its owner alone.
func projectMember(ctx context.Context, tx *transaction, pid, p string) (bool, error) {
	q, args := `SELECT 1 FROM projects pr JOIN workspaces w ON w.id=pr.workspace_id WHERE pr.id=? AND w.owner_principal=?`, []any{pid, p}
	if tx.postgres {
		q, args = q+` UNION ALL SELECT 1 FROM project_roles WHERE project_id=? AND principal=?`, append(args, pid, p)
	}
	rows, err := query(ctx, tx, q, args...)
	if err != nil {
		return false, err
	}
	return len(rows) > 0, nil
}

// trustLink is the principal a trust add links (Q2): --principal, which
// must be a project member, or else link_email when it is an email that
// exactly matches one (either case). "" when nothing links.
func trustLink(ctx context.Context, tx *transaction, pid string, a map[string]any) (string, error) {
	if explicit := str(a, "principal"); explicit != "" {
		p := normalPrincipal(explicit)
		member, err := projectMember(ctx, tx, pid, p)
		if err != nil {
			return "", err
		}
		if !member {
			return "", invalid("--principal must be a member of this ClawBridge project")
		}
		return p, nil
	}
	email := str(a, "link_email")
	if email == "" {
		return "", nil
	}
	p := normalPrincipal(email)
	if !validPrincipal(p) || !strings.EqualFold(strings.TrimSpace(email), p) {
		return "", nil
	}
	member, err := projectMember(ctx, tx, pid, p)
	if err != nil || !member {
		return "", err
	}
	return p, nil
}
