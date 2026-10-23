package bus

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"time"
)

// identitySchemaV6 binds each hosted principal (an email) to the Google
// account (issuer and subject) that first used it, records binding events,
// and adds the per-workspace hosted-domain allow-list (empty means off).
// After an unbind, previous_subject names the released account, which may not
// bind again unless the unbind set allow_previous.
const identitySchemaV6 = `
CREATE TABLE principal_subjects(principal TEXT PRIMARY KEY,issuer TEXT NOT NULL,subject TEXT,bound_at TEXT NOT NULL,bound_by TEXT NOT NULL,previous_subject TEXT,allow_previous INTEGER NOT NULL DEFAULT 0);
CREATE TABLE principal_events(id TEXT PRIMARY KEY,principal TEXT NOT NULL,action TEXT NOT NULL CHECK(action IN ('principal_bound','principal_subject_mismatch','principal_previous_refused','principal_unbound','principal_unbound_allow_previous')),subject_hash TEXT NOT NULL,changed_at TEXT NOT NULL,changed_by TEXT NOT NULL);
CREATE INDEX principal_event_history ON principal_events(principal,changed_at);
ALTER TABLE workspaces ADD COLUMN allowed_domains TEXT NOT NULL DEFAULT '[]';
`

// principalEventsAppendOnlyV6 guards binding history on Postgres with its own
// message, so a refused change names the table it was refused on.
const principalEventsAppendOnlyV6 = `
CREATE FUNCTION clawbridge_immutable_principal_events() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'principal_events is append-only; binding history cannot be updated or deleted'; END $$;
CREATE TRIGGER immutable_principal_events BEFORE UPDATE OR DELETE OR TRUNCATE ON principal_events FOR EACH STATEMENT EXECUTE FUNCTION clawbridge_immutable_principal_events();
`

// GoogleIssuer is the one spelling of Google's ID token issuer a binding
// stores; tokens may carry either accounts.google.com form.
const GoogleIssuer = "https://accounts.google.com"

// OperatorPrincipal records a change made on the host with direct database
// access rather than by a signed-in principal.
const OperatorPrincipal = "operator:local"

// refusalAuditInterval bounds the refusal events one account can add for a
// principal: at most one of each kind per hour.
const refusalAuditInterval = time.Hour

// Identity is a verified hosted caller: the email principal grants are keyed
// on, and the account that proved it.
type Identity struct {
	Principal string
	Issuer    string
	Subject   string
}

// errNeedsBind leaves a request transaction once the request proved the
// principal's access but its account is not the one bound to the principal.
var errNeedsBind = errors.New("principal binding required")

// subjectHash names an account in the audit log without storing the subject
// of an account that was refused.
func subjectHash(issuer, subject string) string {
	h := sha256.Sum256([]byte(issuer + "\x00" + subject))
	return hex.EncodeToString(h[:])
}

// RequireVerifiedAccount makes ExecuteAs refuse, so a hosted request can only
// run through ExecuteAsIdentity and its account binding. The server sets it.
func (s *Store) RequireVerifiedAccount() { s.requireAccount = true }

// ExecuteAsIdentity runs a hosted request for a verified identity. A request
// that passes its access checks also needs the principal bound to this
// account: when it is not, the request is rolled back, the account is bound
// (first sight, or after an unbind) or refused in a transaction of its own,
// and a bound request runs once more. A caller without access is refused by
// the request itself and writes no binding or event.
func (s *Store) ExecuteAsIdentity(ctx context.Context, id Identity, r Request) (Result, error) {
	if !s.postgres {
		return Result{}, errors.New("ExecuteAsIdentity is only available on the hosted authority")
	}
	id.Principal = normalPrincipal(id.Principal)
	if !validPrincipal(id.Principal) {
		return Result{}, errors.New("verified email principal is required")
	}
	if id.Issuer == "" || id.Subject == "" {
		return Result{}, errors.New("verified account subject is required")
	}
	scoped := *s
	scoped.principal = id.Principal
	scoped.identity = &id
	result, err := scoped.executeRequest(ctx, r)
	if errors.Is(err, errNeedsBind) {
		if err = s.bindSubject(ctx, id); err == nil {
			result, err = scoped.executeRequest(ctx, r)
			if errors.Is(err, errNeedsBind) {
				err = &serviceError{code: "forbidden", cause: errors.New("the account binding for " + id.Principal + " changed during this request; sign in again"), public: true}
			}
		}
	}
	return result, classify(err)
}

// requireBound is called inside a request once it has proved the caller's
// access. It reads without a lock: a bound account, the common case, costs one
// primary-key read. Any other state leaves the transaction to bindSubject.
func (s *Store) requireBound(ctx context.Context, tx *transaction) error {
	if s.identity == nil {
		return nil
	}
	var bound int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM principal_subjects WHERE principal=? AND issuer=? AND subject=?`, s.principal, s.identity.Issuer, s.identity.Subject).Scan(&bound); err != nil {
		return err
	}
	if bound != 1 {
		return errNeedsBind
	}
	return nil
}

// bindSubject binds an unbound principal to id's account or refuses it, under
// the principal's subject lock and in its own transaction, so a refusal is
// still audited. Only a request that proved its access reaches it.
func (s *Store) bindSubject(ctx context.Context, id Identity) error {
	if id.Issuer == "" || id.Subject == "" {
		return errors.New("verified account subject is required")
	}
	raw, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return errors.New("authority unavailable; no write queued")
	}
	tx := &transaction{Tx: raw, postgres: s.postgres}
	defer tx.Rollback()
	if err = lockSubject(ctx, tx, id.Principal); err != nil {
		return err
	}
	var issuer string
	var subject, previous sql.NullString
	var allowPrevious bool
	err = tx.QueryRowContext(ctx, `SELECT issuer,subject,previous_subject,allow_previous<>0 FROM principal_subjects WHERE principal=?`, id.Principal).Scan(&issuer, &subject, &previous, &allowPrevious)
	hash := subjectHash(id.Issuer, id.Subject)
	now := stamp()
	switch {
	case errors.Is(err, sql.ErrNoRows):
		if _, err = tx.ExecContext(ctx, `INSERT INTO principal_subjects(principal,issuer,subject,bound_at,bound_by) VALUES(?,?,?,?,'first_sight')`, id.Principal, id.Issuer, id.Subject, now); err != nil {
			return err
		}
		err = principalEvent(ctx, tx, id.Principal, "principal_bound", hash, id.Principal)
	case err != nil:
		return err
	case !subject.Valid && issuer == id.Issuer && previous.Valid && previous.String == id.Subject && !allowPrevious:
		// The account the server owner released may still hold a valid
		// token; it binds again only after an unbind with allow_previous.
		return refuse(ctx, tx, id.Principal, "principal_previous_refused", hash, "this Google account was released from "+id.Principal+" by the server owner; ask the server owner to rebind it")
	case !subject.Valid:
		if _, err = tx.ExecContext(ctx, `UPDATE principal_subjects SET issuer=?,subject=?,bound_at=?,bound_by='first_sight',allow_previous=0 WHERE principal=?`, id.Issuer, id.Subject, now, id.Principal); err != nil {
			return err
		}
		err = principalEvent(ctx, tx, id.Principal, "principal_bound", hash, id.Principal)
	case issuer == id.Issuer && subject.String == id.Subject:
		return nil
	default:
		return refuse(ctx, tx, id.Principal, "principal_subject_mismatch", hash, "this Google account is not the one bound to "+id.Principal+"; ask the server owner to rebind it")
	}
	if err != nil {
		return err
	}
	if err = tx.Commit(); err != nil {
		return errors.New("authority commit failed; no write queued")
	}
	return nil
}

// lockSubject serializes binding changes for one principal on Postgres.
func lockSubject(ctx context.Context, tx *transaction, principal string) error {
	if !tx.postgres {
		return nil
	}
	if _, err := tx.ExecContext(ctx, `SET LOCAL statement_timeout='5s'`); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `SET LOCAL lock_timeout='5s'`); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtextextended(?,0))`, "subject:"+principal)
	return err
}

// refuse audits a refused account, at most once per account and kind each
// refusalAuditInterval, commits, and returns the public refusal.
func refuse(ctx context.Context, tx *transaction, principal, action, hash, message string) error {
	var recent int
	since := time.Now().UTC().Add(-refusalAuditInterval).Format(timestampLayout)
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM principal_events WHERE principal=? AND action=? AND subject_hash=? AND changed_at>?`, principal, action, hash, since).Scan(&recent); err != nil {
		return err
	}
	if recent == 0 {
		if err := principalEvent(ctx, tx, principal, action, hash, principal); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return errors.New("authority commit failed; no write queued")
	}
	return &serviceError{code: "forbidden", cause: errors.New(message), public: true}
}

func principalEvent(ctx context.Context, tx *transaction, principal, action, hash, by string) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO principal_events(id,principal,action,subject_hash,changed_at,changed_by) VALUES(?,?,?,?,?,?)`, newID("pev_"), principal, action, hash, stamp(), by)
	return err
}

// unbindPrincipal lets the server's bootstrap owner release a principal from
// its account, for an address that legitimately moved to a new account.
func (s *Store) unbindPrincipal(ctx context.Context, tx *transaction, a map[string]any) (any, error) {
	// Only a hosted store has a bootstrap owner; a local one never unbinds.
	if s.bootstrapOwner == "" || s.principal != s.bootstrapOwner {
		return nil, &serviceError{code: "forbidden", cause: errors.New("only the server owner may unbind a principal"), public: true}
	}
	if err := s.requireBound(ctx, tx); err != nil {
		return nil, err
	}
	principal := normalPrincipal(str(a, "principal"))
	if !validPrincipal(principal) {
		return nil, invalid("principal must be an email")
	}
	allowPrevious, _ := a["allow_previous"].(bool)
	return unbindSubject(ctx, tx, principal, allowPrevious, s.principal)
}

// OperatorUnbindPrincipal is the host operator's way out when the server owner
// cannot sign in, for example because the owner's own address moved to a new
// Google account. It needs direct database access, not a signed-in principal,
// and is audited as OperatorPrincipal.
func (s *Store) OperatorUnbindPrincipal(ctx context.Context, principal string, allowPrevious bool) (map[string]any, error) {
	principal = normalPrincipal(principal)
	if !validPrincipal(principal) {
		return nil, errors.New("principal must be an email")
	}
	raw, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, errors.New("authority unavailable")
	}
	tx := &transaction{Tx: raw, postgres: s.postgres}
	defer tx.Rollback()
	out, err := unbindSubject(ctx, tx, principal, allowPrevious, OperatorPrincipal)
	if err != nil {
		return nil, err
	}
	if err = tx.Commit(); err != nil {
		return nil, errors.New("authority commit failed")
	}
	return out, nil
}

// unbindSubject releases principal from its account under the subject lock.
// The released account is kept as previous_subject and refused at the next
// sign-in unless allowPrevious, which an unbind made by mistake needs; given on
// an already unbound principal, allowPrevious only lifts that refusal.
func unbindSubject(ctx context.Context, tx *transaction, principal string, allowPrevious bool, by string) (map[string]any, error) {
	if err := lockSubject(ctx, tx, principal); err != nil {
		return nil, err
	}
	var issuer string
	var subject, previous sql.NullString
	var allowed bool
	err := tx.QueryRowContext(ctx, `SELECT issuer,subject,previous_subject,allow_previous<>0 FROM principal_subjects WHERE principal=?`, principal).Scan(&issuer, &subject, &previous, &allowed)
	if errors.Is(err, sql.ErrNoRows) {
		return map[string]any{"principal": principal, "unbound": false, "reason": "not bound"}, nil
	}
	if err != nil {
		return nil, err
	}
	action := "principal_unbound"
	if allowPrevious {
		action = "principal_unbound_allow_previous"
	}
	released := subject
	if !subject.Valid {
		if !allowPrevious || allowed || !previous.Valid {
			return map[string]any{"principal": principal, "unbound": false, "reason": "not bound"}, nil
		}
		released = previous
	}
	flag := 0
	if allowPrevious {
		flag = 1
	}
	if _, err = tx.ExecContext(ctx, `UPDATE principal_subjects SET previous_subject=?,subject=NULL,allow_previous=?,bound_at=?,bound_by=? WHERE principal=?`, released.String, flag, stamp(), by, principal); err != nil {
		return nil, err
	}
	if err = principalEvent(ctx, tx, principal, action, subjectHash(issuer, released.String), by); err != nil {
		return nil, err
	}
	next := "the next sign-in with this address binds its account, except the released one; have the user sign in now"
	if allowPrevious {
		next = "the next sign-in with this address binds its account, the released one included; have the user sign in now"
	}
	return map[string]any{"principal": principal, "unbound": true, "allow_previous": allowPrevious, "next": next}, nil
}
