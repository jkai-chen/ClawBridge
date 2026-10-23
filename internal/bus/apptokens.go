package bus

import (
	"context"
	"database/sql"
	"errors"
	"regexp"
	"time"
)

// Schema 9: the hosted server's Linear app identity (v0.2.7). The server
// alone holds the app token, sealed at rest; clients never receive it. The
// same DDL serves SQLite and PostgreSQL, though only a hosted server uses it.
const appIdentitySchemaV9 = `
CREATE TABLE linear_app_tokens(app_client_id TEXT NOT NULL,linear_org_id TEXT NOT NULL,token_sha256 TEXT NOT NULL,scopes_digest TEXT NOT NULL,ciphertext TEXT NOT NULL,nonce TEXT NOT NULL,minted_at TEXT NOT NULL,expires_at TEXT NOT NULL,state TEXT NOT NULL CHECK(state IN ('minted','live','replaced','discarded','revoked')),PRIMARY KEY(app_client_id,token_sha256));
CREATE UNIQUE INDEX linear_app_token_live ON linear_app_tokens(app_client_id,linear_org_id) WHERE state='live';
CREATE INDEX linear_app_token_minted ON linear_app_tokens(app_client_id,minted_at);
`

// AppToken is one sealed Linear app token. Ciphertext and Nonce are the
// AES-GCM seal; the plaintext token is never stored or returned.
type AppToken struct {
	AppClientID  string
	LinearOrgID  string
	TokenSHA256  string
	ScopesDigest string
	Ciphertext   string
	Nonce        string
	MintedAt     time.Time
	ExpiresAt    time.Time
}

var appClientPattern = regexp.MustCompile(`^[A-Za-z0-9._-]{1,128}$`)

func validAppToken(t AppToken) bool {
	return appClientPattern.MatchString(t.AppClientID) && issueUUIDPattern.MatchString(t.LinearOrgID) && sha256Pattern.MatchString(t.TokenSHA256) && sha256Pattern.MatchString(t.ScopesDigest) && t.Ciphertext != "" && t.Nonce != "" && len(t.Ciphertext) <= 32<<10 && !t.MintedAt.IsZero() && t.ExpiresAt.After(t.MintedAt)
}

// appTx runs fn in a short transaction under the app's advisory lock
// (Postgres). No caller may make a Linear call inside fn.
func (s *Store) appTx(ctx context.Context, app string, fn func(*transaction) error) error {
	if !s.postgres {
		return errors.New("the Linear app identity needs a hosted server")
	}
	raw, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	tx := &transaction{Tx: raw, postgres: true}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `SET LOCAL lock_timeout='5s'`); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtextextended(?,0))`, "linear-app:"+app); err != nil {
		return err
	}
	if err = fn(tx); err != nil {
		return err
	}
	return tx.Commit()
}

func scanAppToken(row *sql.Row) (AppToken, error) {
	var t AppToken
	var minted, expires string
	if err := row.Scan(&t.AppClientID, &t.LinearOrgID, &t.TokenSHA256, &t.ScopesDigest, &t.Ciphertext, &t.Nonce, &minted, &expires); err != nil {
		return AppToken{}, err
	}
	t.MintedAt, t.ExpiresAt = at(minted), at(expires)
	return t, nil
}

// LiveAppToken returns the live sealed token for app and org, if any. An
// empty org matches the app's only live token; an app live in two
// organisations is refused, since a server serves one.
func (s *Store) LiveAppToken(ctx context.Context, app, org string) (AppToken, bool, error) {
	var out AppToken
	found := false
	err := s.appTx(ctx, app, func(tx *transaction) error {
		q, args := `SELECT app_client_id,linear_org_id,token_sha256,scopes_digest,ciphertext,nonce,minted_at,expires_at FROM linear_app_tokens WHERE app_client_id=? AND state='live'`, []any{app}
		if org != "" {
			q, args = q+` AND linear_org_id=?`, append(args, org)
		} else {
			var n int
			if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM linear_app_tokens WHERE app_client_id=? AND state='live'`, app).Scan(&n); err != nil {
				return err
			}
			if n > 1 {
				return invalid("the Linear app is live in more than one organisation")
			}
		}
		t, err := scanAppToken(tx.QueryRowContext(ctx, q, args...))
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		out, found = t, err == nil
		return err
	})
	return out, found, err
}

// PublishAppToken makes t the live token for its app and org, replacing
// the live one only when that one's digest is replaces (compare-and-swap):
// "" replaces nothing. When another token is live instead, t is recorded as
// discarded, so it still counts toward the mint limit, and the live one is
// returned for the caller to use.
func (s *Store) PublishAppToken(ctx context.Context, t AppToken, replaces string) (AppToken, bool, error) {
	if !validAppToken(t) || (replaces != "" && !sha256Pattern.MatchString(replaces)) {
		return AppToken{}, false, invalid("invalid app token record")
	}
	current, published := t, false
	err := s.appTx(ctx, t.AppClientID, func(tx *transaction) error {
		live, err := scanAppToken(tx.QueryRowContext(ctx, `SELECT app_client_id,linear_org_id,token_sha256,scopes_digest,ciphertext,nonce,minted_at,expires_at FROM linear_app_tokens WHERE app_client_id=? AND linear_org_id=? AND state='live'`, t.AppClientID, t.LinearOrgID))
		hasLive := err == nil
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		state := "live"
		if hasLive && live.TokenSHA256 != replaces {
			state, current = "discarded", live
		} else if hasLive {
			if _, err = tx.ExecContext(ctx, `UPDATE linear_app_tokens SET state='replaced' WHERE app_client_id=? AND token_sha256=?`, live.AppClientID, live.TokenSHA256); err != nil {
				return err
			}
		}
		// The token was counted when it was minted; that row becomes this one.
		if _, err = tx.ExecContext(ctx, `DELETE FROM linear_app_tokens WHERE app_client_id=? AND token_sha256=? AND state='minted'`, t.AppClientID, t.TokenSHA256); err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO linear_app_tokens(app_client_id,linear_org_id,token_sha256,scopes_digest,ciphertext,nonce,minted_at,expires_at,state) VALUES(?,?,?,?,?,?,?,?,?)`, t.AppClientID, t.LinearOrgID, t.TokenSHA256, t.ScopesDigest, t.Ciphertext, t.Nonce, layout(t.MintedAt), layout(t.ExpiresAt), state)
		published = state == "live"
		return err
	})
	if err != nil {
		return AppToken{}, false, err
	}
	return current, published, nil
}

// RecordMintedAppToken counts a token the moment Linear issued it, before
// anything else can fail: a row in state minted, with no organisation and no
// ciphertext. Publishing the token later replaces the row; a token that never
// gets that far still counts toward the mint limit until it expires.
func (s *Store) RecordMintedAppToken(ctx context.Context, app, sha string, minted, expires time.Time) error {
	if !appClientPattern.MatchString(app) || !sha256Pattern.MatchString(sha) || minted.IsZero() || !expires.After(minted) {
		return invalid("invalid minted app token record")
	}
	return s.appTx(ctx, app, func(tx *transaction) error {
		_, err := tx.ExecContext(ctx, `INSERT INTO linear_app_tokens(app_client_id,linear_org_id,token_sha256,scopes_digest,ciphertext,nonce,minted_at,expires_at,state) VALUES(?,'',?,'','','',?,?,'minted')`, app, sha, layout(minted), layout(expires))
		return err
	})
}

// AppTokensMinted counts the app's tokens minted since since and not
// revoked: Linear allows a bounded number of live tokens per app.
func (s *Store) AppTokensMinted(ctx context.Context, app string, since time.Time) (int, error) {
	n := 0
	err := s.appTx(ctx, app, func(tx *transaction) error {
		return tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM linear_app_tokens WHERE app_client_id=? AND minted_at>=? AND state<>'revoked'`, app, layout(since)).Scan(&n)
	})
	return n, err
}

// RevokedAppToken records that Linear revoked the token with digest sha.
func (s *Store) RevokedAppToken(ctx context.Context, app, sha string) error {
	if !sha256Pattern.MatchString(sha) {
		return invalid("invalid app token digest")
	}
	return s.appTx(ctx, app, func(tx *transaction) error {
		_, err := tx.ExecContext(ctx, `UPDATE linear_app_tokens SET state='revoked' WHERE app_client_id=? AND token_sha256=?`, app, sha)
		return err
	})
}
