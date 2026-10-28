package bus

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// The server's message stream (MESSAGE-QUEUE-DESIGN §3.1). A session's
// instance is its secret, and a URL ends up in access logs, so the stream's
// GET never carries it (M3). The client first asks for a stream ticket on
// the normal signed request: random, single-use, valid for a minute, and
// bound to the caller's account, the request's scope, the instance and the
// mode. The GET hands the ticket over in a header, and the server redeems
// it for the instance its reads then run with.

// Schema 36: the stream's tickets, and the consumer each lease was taken by,
// so a stream that replaces another takes over what it held.
const streamSchemaV36 = `ALTER TABLE deliveries ADD COLUMN lease_consumer TEXT;
CREATE TABLE stream_tickets(ticket_hash TEXT PRIMARY KEY,principal TEXT NOT NULL,issuer TEXT NOT NULL,subject TEXT NOT NULL,workspace TEXT NOT NULL,project TEXT NOT NULL,session TEXT NOT NULL,project_id TEXT NOT NULL,session_id TEXT NOT NULL,instance TEXT NOT NULL,mode TEXT NOT NULL,expires_at TEXT NOT NULL);
CREATE INDEX stream_tickets_expiry ON stream_tickets(expires_at);
`

// streamTicketLife is how long a stream ticket can be redeemed.
const streamTicketLife = time.Minute

// Stream modes: consume leases and pushes the session's deliveries; observe
// pushes only its counts.
const (
	StreamConsume = "consume"
	StreamObserve = "observe"
)

// ErrStreamTicket refuses a ticket that is unknown, used, expired, or not
// the caller's for this stream. It never says which.
var ErrStreamTicket = errors.New("the stream ticket is invalid, used or expired; ask for a new one")

// streamTicket issues a stream ticket for the actor at its current instance:
// Execute has already refused any other instance.
func (s *Store) streamTicket(ctx context.Context, tx *transaction, pid string, actor map[string]any, r Request) (any, error) {
	if s.identity == nil || !tx.postgres {
		return nil, invalid("streams need a ClawBridge server")
	}
	mode := str(r.Args, "mode")
	if mode != StreamConsume && mode != StreamObserve {
		return nil, invalid("mode must be consume or observe")
	}
	ticket, err := randomToken("stk_")
	if err != nil {
		return nil, err
	}
	now := hostClock().UTC()
	expires := layout(now.Add(streamTicketLife))
	if _, err = tx.ExecContext(ctx, `DELETE FROM stream_tickets WHERE expires_at<=?`, layout(now)); err != nil {
		return nil, err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO stream_tickets(ticket_hash,principal,issuer,subject,workspace,project,session,project_id,session_id,instance,mode,expires_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?)`,
		tokenDigest(ticket), s.principal, s.identity.Issuer, s.identity.Subject, r.Workspace, r.Project, r.Session, pid, actor["id"], actor["instance"], mode, expires); err != nil {
		return nil, err
	}
	return map[string]any{"ticket": ticket, "expires_at": expires, "mode": mode}, nil
}

// StreamScope is what a stream's GET names: the request scope and the mode.
type StreamScope struct {
	Workspace, Project, Session, Mode string
}

// StreamGrant is what a redeemed ticket was issued for.
type StreamGrant struct {
	Instance, ProjectID, SessionID string
}

// RedeemStreamTicket uses a ticket up and returns what it was issued for.
// The ticket must be unexpired, the same account's, and for the same scope
// and mode; whatever the outcome, it can't be used again.
func (s *Store) RedeemStreamTicket(ctx context.Context, id Identity, ticket string, scope StreamScope) (StreamGrant, error) {
	var g StreamGrant
	if !s.postgres || ticket == "" || len(ticket) > 128 {
		return g, ErrStreamTicket
	}
	var principal, issuer, subject, workspace, project, session, mode, expires string
	err := s.db.QueryRowContext(ctx, `DELETE FROM stream_tickets WHERE ticket_hash=$1 RETURNING principal,issuer,subject,workspace,project,session,project_id,session_id,instance,mode,expires_at`, tokenDigest(ticket)).
		Scan(&principal, &issuer, &subject, &workspace, &project, &session, &g.ProjectID, &g.SessionID, &g.Instance, &mode, &expires)
	if errors.Is(err, sql.ErrNoRows) {
		return StreamGrant{}, ErrStreamTicket
	}
	if err != nil {
		return StreamGrant{}, err
	}
	if expires <= layout(hostClock().UTC()) || principal != normalPrincipal(id.Principal) || issuer != id.Issuer || subject != id.Subject ||
		workspace != scope.Workspace || project != scope.Project || session != scope.Session || mode != scope.Mode {
		return StreamGrant{}, ErrStreamTicket
	}
	return g, nil
}

// ConsumerChannel is the Postgres channel a new consume stream announces
// itself on, with the payload "<project_id>:<session_id>:<consumer>". Every
// replica's older consume stream for that session is replaced.
const ConsumerChannel = "cb_consumer"

// CaseChannel is the Postgres channel a new dispatcher case notifies, with
// the project's ID: the model's cases/next long-polls wait on it instead of
// re-reading every second (MESSAGE-QUEUE-DESIGN §4). It says only "look".
const CaseChannel = "cb_case"

// caseNotify tells the server's waiting cases/next polls that a case
// opened. NOTIFY is transactional, so a rolled-back case wakes no one.
func caseNotify(ctx context.Context, tx *transaction, pid string) error {
	if !tx.postgres {
		return nil
	}
	_, err := tx.ExecContext(ctx, `SELECT pg_notify(?,?)`, CaseChannel, pid)
	return err
}

// AnnounceConsumer tells every replica that consumer is now the session's
// consume stream.
func (s *Store) AnnounceConsumer(ctx context.Context, pid, sid, consumer string) error {
	if !s.postgres {
		return nil
	}
	_, err := s.db.ExecContext(ctx, `SELECT pg_notify($1,$2)`, ConsumerChannel, pid+":"+sid+":"+consumer)
	return err
}

// latestSeq is the newest recipient_seq the session has been given, 0
// before its first delivery.
func latestSeq(ctx context.Context, tx *transaction, pid, sid string) (int64, error) {
	var seq int64
	err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(seq),0) FROM delivery_counters WHERE project_id=? AND recipient_id=?`, pid, sid).Scan(&seq)
	return seq, err
}
