package bus

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"time"

	"clawbridge/internal/principal"
)

// The text a trusted person vouched for (FR57). At the signal the queue
// row keeps the SHA-256 of the issue's title and description. The server's
// own reads of the issue, as the app (the intake pass), send the current
// hashes here: if either differs, the row is marked and the coordinator is
// told once. A client's issue read never compares: it shows the stored mark.
//
// The mark lives in the row's reason column in two forms, both prefixed
// text_ (isTextMark): text_changed (changed, and the change notice went
// out) and text_changed_unsent (changed, but no coordinator could be told
// yet). Another path writes reason too: intakeNotice sets no_recipient on
// a row it could tell nobody about. So a mark that could not be announced
// is kept as unsent, never claimed as told, and the row's notified_at says
// whether the original notice ever went out; IntakeTextRetry delivers both
// once a coordinator exists. Clearing a mark restores no_recipient when
// the original notice is still owed.
const (
	intakeTextChanged = "text_changed"
	intakeTextUnsent  = "text_changed_unsent"
	// intakeTextMaybeUnsent is intakeTextUnsent for a flag set at queueing
	// (textFlag.maybe): its notice, when it goes out, says the text may have
	// changed, not that it did.
	intakeTextMaybeUnsent = "text_maybe_unsent"
	// sqlTextMark is the predicate for a text mark, on reason.
	sqlTextMark = `substr(reason,1,5)='text_'`
	// sqlClearTextMark is the reason a cleared mark leaves.
	sqlClearTextMark = `CASE WHEN reason IN ('` + intakeTextUnsent + `','` + intakeTextMaybeUnsent + `') AND notified_at IS NULL THEN 'no_recipient' WHEN ` + sqlTextMark + ` THEN '' ELSE reason END`
)

func isTextMark(reason string) bool { return strings.HasPrefix(reason, "text_") }

// IntakeTextStatus is what a text check found. Tracked is false for an
// issue with no live queue row, or a row that kept no hashes.
type IntakeTextStatus struct {
	Tracked bool `json:"tracked"`
	Changed bool `json:"changed"`
	// SignalAt is the issue's updated_at as the trusted signal saw it.
	SignalAt string `json:"signal_at,omitempty"`
}

// IntakeTextCheck compares an issue's current title and description
// hashes with those the trusted signal recorded, for the server's own
// intake pass. An empty hash is an unknown field and is not compared.
func (s *Store) IntakeTextCheck(ctx context.Context, pid, issueUUID, titleSHA256, descriptionSHA256 string) (IntakeTextStatus, error) {
	tx, p, err := s.systemTx(ctx, pid)
	if err != nil {
		return IntakeTextStatus{}, err
	}
	defer tx.Rollback()
	w, err := one(ctx, tx, `SELECT * FROM workspaces WHERE id=?`, str(p, "workspace_id"))
	if err != nil {
		return IntakeTextStatus{}, err
	}
	st, err := s.intakeTextCheck(ctx, tx, w, pid, issueUUID, titleSHA256, descriptionSHA256)
	if err != nil {
		return IntakeTextStatus{}, err
	}
	return st, tx.Commit()
}

// intakeTextRow reads the queue row a text check or status is about, and
// whether the queue holds that issue to its signal's text at all.
func intakeTextRow(ctx context.Context, tx *transaction, pid, issue string) (map[string]any, bool, error) {
	rows, err := query(ctx, tx, `SELECT issue_identifier,signals,trigger_actor,trigger_cap,notified_at,state,reason,queued_title_sha256,queued_description_sha256,queued_updated_at FROM intake_queue WHERE project_id=? AND issue_uuid=?`, pid, issue)
	if err != nil || len(rows) == 0 {
		return nil, false, err
	}
	row := rows[0]
	if state := str(row, "state"); state == "dismissed" || state == "closed" || (str(row, "queued_title_sha256") == "" && str(row, "queued_description_sha256") == "") {
		return nil, false, nil
	}
	return row, true, nil
}

// intakeTextStatus is the stored verdict for a client's issue read. It
// writes nothing.
func intakeTextStatus(ctx context.Context, tx *transaction, pid, issue string) (map[string]any, error) {
	row, tracked, err := intakeTextRow(ctx, tx, pid, issue)
	if err != nil || !tracked {
		return map[string]any{"tracked": false, "changed": false}, err
	}
	return map[string]any{"tracked": true, "changed": isTextMark(str(row, "reason")), "signal_at": str(row, "queued_updated_at")}, nil
}

// intakeTextCheck compares the app's hashes with the row's. Only the
// server's own reads come here (the intake pass).
func (s *Store) intakeTextCheck(ctx context.Context, tx *transaction, w map[string]any, pid, issue, title, description string) (IntakeTextStatus, error) {
	row, tracked, err := intakeTextRow(ctx, tx, pid, issue)
	if err != nil || !tracked {
		return IntakeTextStatus{}, err
	}
	queuedTitle, queuedDescription := str(row, "queued_title_sha256"), str(row, "queued_description_sha256")
	st := IntakeTextStatus{Tracked: true, SignalAt: str(row, "queued_updated_at")}
	if isTextMark(str(row, "reason")) {
		st.Changed = true
		return st, nil
	}
	differs := func(queued, current string) bool { return queued != "" && current != "" && queued != current }
	if !differs(queuedTitle, title) && !differs(queuedDescription, description) {
		return st, nil
	}
	st.Changed = true
	return st, s.markText(ctx, tx, w, pid, issue, row, textFlag{})
}

// textFlag says what a flag stands on. A flag from a hash difference is
// definite. One set at queueing (maybe) is only that the issue changed after
// the trusted signal, which need not be the text; signalAt is the signal's
// time, and unknown to the retry of an unsent one.
type textFlag struct {
	maybe    bool
	signalAt string
}

// The future offer and auto intake modes must treat any text_ mark
// (isTextMark) as BLOCKING: a flagged issue is never offered or
// auto-assigned until it is vouched for again. Only notify exists today,
// so nothing enforces this yet.
//
// markText flags a row whose text changed, and tells the coordinator once.
// The flag is text_changed only when the notice can go out now; otherwise
// it is text_changed_unsent, for IntakeTextRetry. The claim is its own
// write, so concurrent checks tell the coordinator once.
func (s *Store) markText(ctx context.Context, tx *transaction, w map[string]any, pid, issue string, row map[string]any, flag textFlag) error {
	targets, mode, err := s.textNoticeTargets(ctx, tx, pid, issue, str(row, "trigger_cap"))
	if err != nil {
		return err
	}
	mark := intakeTextUnsent
	if flag.maybe {
		mark = intakeTextMaybeUnsent
	}
	if len(targets) > 0 && mode {
		mark = intakeTextChanged
	}
	res, err := tx.ExecContext(ctx, `UPDATE intake_queue SET reason=? WHERE project_id=? AND issue_uuid=? AND NOT (`+sqlTextMark+`)`, mark, pid, issue)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 || mark != intakeTextChanged {
		return nil
	}
	return s.deliverTextNotices(ctx, tx, w, pid, issue, row, targets[0], flag)
}

// textNoticeTargets are the coordinator sessions a text notice would go to,
// and whether the project's policy sends notices at this cap at all.
func (s *Store) textNoticeTargets(ctx context.Context, tx *transaction, pid, issue, cap string) ([]map[string]any, bool, error) {
	policy, err := intakePolicy(ctx, tx, pid)
	if err != nil {
		return nil, false, err
	}
	project, labels := issueRouting(ctx, tx, pid, issue)
	targets, err := s.coordinatorTargets(ctx, tx, pid, project, labels)
	return targets, lowerMode(str(policy, "mode"), cap) == "notify", err
}

// deliverTextNotices sends the original intake notice, when it never went
// out (the row has no notified_at), and then the change notice. The row's
// reason has already been claimed.
func (s *Store) deliverTextNotices(ctx context.Context, tx *transaction, w map[string]any, pid, issue string, row map[string]any, target map[string]any, flag textFlag) error {
	nowRaw, err := dbStamp(ctx, tx)
	if err != nil {
		return err
	}
	if row["notified_at"] == nil {
		ev := IntakeEvent{Signal: str(row, "signals"), Actor: str(row, "trigger_actor"), Trusted: str(row, "trigger_cap") == "auto" && str(row, "trigger_actor") != "", Issue: IntakeIssue{UUID: issue, Identifier: str(row, "issue_identifier")}}
		if err = s.intakeNotice(ctx, tx, w, pid, ev, str(row, "trigger_cap"), nowRaw); err != nil {
			return err
		}
	}
	return s.intakeTextNotice(ctx, tx, w, pid, issue, str(row, "issue_identifier"), str(row, "queued_updated_at"), nowRaw, flag)
}

// IntakeTextRetry tells a coordinator about the rows whose text changed
// while there was nobody to tell (text_changed_unsent), now that there may
// be one. It reads no Linear. The server's intake pass runs it first.
func (s *Store) IntakeTextRetry(ctx context.Context, pid string) error {
	tx, p, err := s.systemTx(ctx, pid)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	rows, err := query(ctx, tx, `SELECT issue_uuid,issue_identifier,signals,trigger_actor,trigger_cap,notified_at,queued_updated_at,reason FROM intake_queue WHERE project_id=? AND reason IN (?,?) AND state IN ('queued','offered','assigned') ORDER BY issue_uuid LIMIT 50`, pid, intakeTextUnsent, intakeTextMaybeUnsent)
	if err != nil || len(rows) == 0 {
		return err
	}
	w, err := one(ctx, tx, `SELECT * FROM workspaces WHERE id=?`, str(p, "workspace_id"))
	if err != nil {
		return err
	}
	for _, row := range rows {
		issue := str(row, "issue_uuid")
		targets, mode, e := s.textNoticeTargets(ctx, tx, pid, issue, str(row, "trigger_cap"))
		if e != nil {
			return e
		}
		if len(targets) == 0 || !mode {
			continue
		}
		res, e := tx.ExecContext(ctx, `UPDATE intake_queue SET reason=? WHERE project_id=? AND issue_uuid=? AND reason=?`, intakeTextChanged, pid, issue, str(row, "reason"))
		if e != nil {
			return e
		}
		if n, _ := res.RowsAffected(); n == 0 {
			continue
		}
		if e = s.deliverTextNotices(ctx, tx, w, pid, issue, row, targets[0], textFlag{maybe: str(row, "reason") == intakeTextMaybeUnsent}); e != nil {
			return e
		}
	}
	return tx.Commit()
}

// intakeTextNotice tells the project's coordinator, once per flag, that an
// issue's text changed after the trusted signal, or (flag.maybe) may have.
// Typed fields only, never the text; every time is parsed. With no
// coordinator the flag stands and nobody is told.
func (s *Store) intakeTextNotice(ctx context.Context, tx *transaction, w map[string]any, pid, issue, identifier, updatedAt, nowRaw string, flag textFlag) error {
	project, labels := issueRouting(ctx, tx, pid, issue)
	coordinator, err := s.coordinatorTargets(ctx, tx, pid, project, labels)
	if err != nil || len(coordinator) == 0 {
		return err
	}
	name := issue
	if issueIdentifierPattern.MatchString(identifier) {
		name = identifier
	}
	// The stamps came from Linear: only a parsed time is sent on.
	when := func(raw string) string {
		if t, e := time.Parse(time.RFC3339Nano, raw); e == nil {
			return t.UTC().Format(time.RFC3339)
		}
		return "unknown"
	}
	var b strings.Builder
	if flag.maybe {
		fmt.Fprintf(&b, "Intake: %s's text may have changed (or another field changed) since the trusted signal (signal at %s, issue updated at %s); check the issue history. Until then the text is NOT vouched for. Ask the person who asked to re-add needs-agent or delegate again, or ask a project lead or owner to run clawbridge intake rearm %s in their own terminal, to vouch for it.", name, when(flag.signalAt), when(updatedAt), issue)
	} else {
		fmt.Fprintf(&b, "Intake: %s's title or description changed after the trusted signal (issue updated at %s). The new text is NOT vouched for. Ask the person who asked to re-add needs-agent or delegate again, or ask a project lead or owner to run clawbridge intake rearm %s in their own terminal, to vouch for it.", name, when(updatedAt), issue)
	}
	b.WriteString("\n" + intakeLine)
	return s.insertIntakeMessage(ctx, tx, w, pid, coordinator[0], b.String(), nowRaw)
}

// IntakeSkew is how long after a trusted transition an issue may have been
// updated and still count as the text the person saw: Linear stamps the
// issue's update and the history entry a moment apart.
const IntakeSkew = 5 * time.Second

// eventVouchesText reports whether a signal's issue read is the text its
// trusted actor saw: the actor is trusted and proven, and the issue was not
// updated more than IntakeSkew after the transition. An edit between the
// transition and the poll fails this, so it cannot vouch for itself.
func eventVouchesText(ev IntakeEvent) bool {
	if !ev.Trusted || ev.Actor == "" || ev.Issue.TitleSHA256 == "" {
		return false
	}
	happened, e := time.Parse(time.RFC3339Nano, ev.At)
	if e != nil {
		return false
	}
	updated, e := time.Parse(time.RFC3339Nano, ev.Issue.UpdatedAt)
	return e == nil && !updated.After(happened.Add(IntakeSkew))
}

// eventRevouches reports whether a signal on a live row is the trusted
// person vouching again for its text: a label add (S1) or a delegation (S2)
// within the skew. A mention (S3) is not an act on the issue, so it never
// is.
func eventRevouches(ev IntakeEvent) bool {
	return (ev.Signal == "S1" || ev.Signal == "S2") && eventVouchesText(ev)
}

// intakeNewerThanStamp reports whether a transition happened after the
// issue update the row's text was read with, so it is a new vouch and not
// a replay of the one the row holds.
func intakeNewerThanStamp(ev IntakeEvent, row map[string]any) bool {
	happened, e := time.Parse(time.RFC3339Nano, ev.At)
	if e != nil {
		return false
	}
	stamped, e := time.Parse(time.RFC3339Nano, str(row, "queued_updated_at"))
	return e == nil && happened.After(stamped)
}

// restampText records the event's text as the row's baseline and lifts a
// text-change flag.
func restampText(ctx context.Context, tx *transaction, pid string, ev IntakeEvent) error {
	_, err := tx.ExecContext(ctx, `UPDATE intake_queue SET queued_title_sha256=?,queued_description_sha256=?,queued_updated_at=?,reason=`+sqlClearTextMark+` WHERE project_id=? AND issue_uuid=?`, ev.Issue.TitleSHA256, ev.Issue.DescriptionSHA256, ev.Issue.UpdatedAt, pid, ev.Issue.UUID)
	return err
}

// ownTerminal is the proof intake attribution asks of a person (A2,
// AppWrite.creditable): no session, no agent marker (the client sent
// agent_marker empty; an older client sends none, and none is refused),
// and on the hosted server the person's own signed-in device, not CI. The
// agent marker is the client's own word. A local authority has no device
// identity, so the other checks are all it can make.
func (s *Store) ownTerminal(actor map[string]any, a map[string]any) bool {
	if actor != nil {
		return false
	}
	if marker, ok := a["agent_marker"].(string); !ok || marker != "" {
		return false
	}
	if s.postgres {
		return s.identity != nil && s.identity.Issuer == DeviceIssuer && !principal.CI(s.principal)
	}
	return true
}

var hexSHA256 = regexp.MustCompile(`^[0-9a-f]{64}$`)

// AppText is an issue's text as the server's own app read saw it. Only the
// server sets it, on the context of the request it forwards (WithAppText);
// a client's arguments never carry hashes.
type AppText struct {
	TitleSHA256, DescriptionSHA256, UpdatedAt string
}

type appTextKey struct{}

// WithAppText marks ctx with the app's read for an intake_rearm. A request
// reaches the bus with its HTTP context, which a client cannot mark.
func WithAppText(ctx context.Context, t AppText) context.Context {
	return context.WithValue(ctx, appTextKey{}, t)
}

// vouchText makes the app's read the row's baseline, and lifts a
// text-change flag, when the person confirmed the update time of the issue
// they read and the app's read is of that same update (the FR29 pattern:
// the profile and the app render the text differently, so the hashes are
// the app's and the person's read only proves the issue has not changed
// since). Without the app's read, a title and description hash, or the same
// update time, it changes nothing.
func vouchText(ctx context.Context, tx *transaction, pid, issue string, a map[string]any) (bool, error) {
	t, ok := ctx.Value(appTextKey{}).(AppText)
	if !ok || !hexSHA256.MatchString(t.TitleSHA256) || !hexSHA256.MatchString(t.DescriptionSHA256) {
		return false, nil
	}
	confirmed, e1 := time.Parse(time.RFC3339Nano, str(a, "updated_at"))
	read, e2 := time.Parse(time.RFC3339Nano, t.UpdatedAt)
	if e1 != nil || e2 != nil || len(str(a, "updated_at")) > 64 || !confirmed.Equal(read) {
		return false, nil
	}
	_, err := tx.ExecContext(ctx, `UPDATE intake_queue SET queued_title_sha256=?,queued_description_sha256=?,queued_updated_at=?,reason=`+sqlClearTextMark+` WHERE project_id=? AND issue_uuid=?`, t.TitleSHA256, t.DescriptionSHA256, t.UpdatedAt, pid, issue)
	return err == nil, err
}

// IntakeTextCandidateCap is a safety bound on the rows one pass lists, far
// above a realistic queue. The watcher logs once when it is reached.
const IntakeTextCandidateCap = 5000

// IntakeTextCandidates are the project's live queue rows the intake pass
// still has to hold to their signal's text: queued, offered or assigned,
// with hashes, and not yet flagged. It lists them all (up to the cap) so
// the pass can take the longest-unchecked first.
func (s *Store) IntakeTextCandidates(ctx context.Context, pid string) ([]string, error) {
	tx, _, err := s.systemTx(ctx, pid)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	rows, err := query(ctx, tx, `SELECT issue_uuid FROM intake_queue WHERE project_id=? AND state IN ('queued','offered','assigned') AND NOT (`+sqlTextMark+`) AND (queued_title_sha256<>'' OR queued_description_sha256<>'') ORDER BY issue_uuid LIMIT `+fmt.Sprint(IntakeTextCandidateCap), pid)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(rows))
	for _, row := range rows {
		out = append(out, str(row, "issue_uuid"))
	}
	return out, nil
}
