package bus

import (
	"context"
	"database/sql"
	"errors"
	"regexp"
)

// Schema 10: the hosted server's operation ledger for the writes it sends
// to Linear as the app (v0.2.8). Records are opaque JSON kept by the
// caller; each call is one short transaction, and no Linear call is ever
// made while one is open.
const writeLedgerSchemaV10 = `
CREATE TABLE linear_write_ledger(namespace TEXT NOT NULL,operation_id TEXT NOT NULL,record TEXT NOT NULL,updated_at TEXT NOT NULL,PRIMARY KEY(namespace,operation_id));
CREATE TABLE linear_write_unsettled(namespace TEXT NOT NULL,operation_id TEXT NOT NULL,PRIMARY KEY(namespace,operation_id));
`

var (
	ledgerNamespace   = regexp.MustCompile(`^[0-9a-f]{64}$`)
	ledgerOperationID = regexp.MustCompile(`^[A-Za-z0-9._-]{1,128}$`)
)

func validLedgerKey(namespace, operationID string) error {
	if !ledgerNamespace.MatchString(namespace) || !ledgerOperationID.MatchString(operationID) {
		return invalid("invalid operation ledger key")
	}
	return nil
}

// ledgerTx runs fn in one short transaction.
func (s *Store) ledgerTx(ctx context.Context, fn func(*transaction) error) error {
	if !s.postgres {
		return errors.New("the shared operation ledger needs a hosted server")
	}
	raw, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	tx := &transaction{Tx: raw, postgres: true}
	defer tx.Rollback()
	if err = fn(tx); err != nil {
		return err
	}
	return tx.Commit()
}

// LedgerInsert records record for an operation that has none and reports
// created; for one that has a record, it returns that record instead. The
// row names its ClawBridge project and whose write it is (schema 49).
func (s *Store) LedgerInsert(ctx context.Context, namespace, operationID, record, project, who string) (string, bool, error) {
	if err := validLedgerKey(namespace, operationID); err != nil {
		return "", false, err
	}
	if err := validLedgerScope(project, who); err != nil {
		return "", false, err
	}
	existing, created := "", false
	err := s.ledgerTx(ctx, func(tx *transaction) error {
		r, err := tx.ExecContext(ctx, `INSERT INTO linear_write_ledger(namespace,operation_id,record,updated_at,project_id,principal,pending) VALUES(?,?,?,?,?,?,?) ON CONFLICT(namespace,operation_id) DO NOTHING`, namespace, operationID, record, stamp(), project, who, recordPending(record))
		if err != nil {
			return err
		}
		if n, _ := r.RowsAffected(); n == 1 {
			created = true
			return nil
		}
		return tx.QueryRowContext(ctx, `SELECT record FROM linear_write_ledger WHERE namespace=? AND operation_id=?`, namespace, operationID).Scan(&existing)
	})
	return existing, created, err
}

// LedgerGet returns an operation's record, if any.
func (s *Store) LedgerGet(ctx context.Context, namespace, operationID string) (string, bool, error) {
	if err := validLedgerKey(namespace, operationID); err != nil {
		return "", false, err
	}
	record, found := "", false
	err := s.ledgerTx(ctx, func(tx *transaction) error {
		err := tx.QueryRowContext(ctx, `SELECT record FROM linear_write_ledger WHERE namespace=? AND operation_id=?`, namespace, operationID).Scan(&record)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		found = err == nil
		return err
	})
	return record, found, err
}

// ledgerReplace settles a record by compare-and-swap on the record alone.
// It never reads acknowledged_at: a record a marker's reread acknowledged
// still takes the late outcome of the server that sent it, and applied
// wins (schema 53).
const ledgerReplace = `UPDATE linear_write_ledger SET record=?,updated_at=?,pending=? WHERE namespace=? AND operation_id=? AND record=?`

// LedgerReplace swaps an operation's record from from to to, only if it is
// still from: the caller that wrote from is the only one that settles it.
// The row stays pending only while the new record is (schema 49).
func (s *Store) LedgerReplace(ctx context.Context, namespace, operationID, from, to string) error {
	if err := validLedgerKey(namespace, operationID); err != nil {
		return err
	}
	return s.ledgerTx(ctx, func(tx *transaction) error {
		r, err := tx.ExecContext(ctx, ledgerReplace, to, stamp(), recordPending(to), namespace, operationID, from)
		if err != nil {
			return err
		}
		if n, _ := r.RowsAffected(); n != 1 {
			return conflict("the operation's record changed")
		}
		return nil
	})
}

// LedgerDelete removes an operation's record if it is still record.
func (s *Store) LedgerDelete(ctx context.Context, namespace, operationID, record string) error {
	if err := validLedgerKey(namespace, operationID); err != nil {
		return err
	}
	return s.ledgerTx(ctx, func(tx *transaction) error {
		r, err := tx.ExecContext(ctx, `DELETE FROM linear_write_ledger WHERE namespace=? AND operation_id=? AND record=?`, namespace, operationID, record)
		if err != nil {
			return err
		}
		if n, _ := r.RowsAffected(); n != 1 {
			return conflict("the operation's record changed")
		}
		return nil
	})
}

// LedgerMarkUnsettled notes a comment whose bus settle is owed; unmark
// drops the note.
func (s *Store) LedgerMarkUnsettled(ctx context.Context, namespace, operationID string, unsettled bool) error {
	if err := validLedgerKey(namespace, operationID); err != nil {
		return err
	}
	return s.ledgerTx(ctx, func(tx *transaction) error {
		q := `INSERT INTO linear_write_unsettled(namespace,operation_id) VALUES(?,?) ON CONFLICT(namespace,operation_id) DO NOTHING`
		if !unsettled {
			q = `DELETE FROM linear_write_unsettled WHERE namespace=? AND operation_id=?`
		}
		_, err := tx.ExecContext(ctx, q, namespace, operationID)
		return err
	})
}

// LedgerUnsettled lists up to limit comments whose bus settle is owed.
func (s *Store) LedgerUnsettled(ctx context.Context, namespace string, limit int) ([]string, error) {
	if !ledgerNamespace.MatchString(namespace) || limit < 1 || limit > 1000 {
		return nil, invalid("invalid operation ledger selection")
	}
	out := []string{}
	err := s.ledgerTx(ctx, func(tx *transaction) error {
		rows, err := tx.QueryContext(ctx, `SELECT operation_id FROM linear_write_unsettled WHERE namespace=? ORDER BY operation_id LIMIT ?`, namespace, limit)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var op string
			if err = rows.Scan(&op); err != nil {
				return err
			}
			out = append(out, op)
		}
		return rows.Err()
	})
	return out, err
}
