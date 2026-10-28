package bus

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode"
)

// The server drain (live/ZERO-LOSS-DEPLOY-DESIGN.md §2). One row says the
// servers are in maintenance until a time; each serving process reports
// itself and whether it has drained. Every time is the database's clock,
// never a process's, so two hosts' skew can't make an instance look dead
// or a drain look over.

// MaintenanceChannel is the Postgres channel a maintenance start or end
// notifies; each serving process re-reads the row on it.
const MaintenanceChannel = "cb_maintenance"

const (
	// MaintenanceDefaultTTL and MaintenanceMaxTTL bound a maintenance: past
	// its end it is over on its own, so a forgotten end can't keep the
	// servers shut.
	MaintenanceDefaultTTL = 15 * time.Minute
	MaintenanceMaxTTL     = 60 * time.Minute
	// maintenanceReasonMax bounds the operator's reason.
	maintenanceReasonMax = 200
	// InstanceLiveWindow is how recently an instance must have reported to
	// count as live; each reports every 5 s.
	InstanceLiveWindow = 15 * time.Second
	// instancePrune drops an instance row that hasn't reported for a day.
	instancePrune = 24 * time.Hour
)

// maintenanceSchema is schema maintenanceSchemaVersion, hosted only: SQLite
// has no servers to drain, so its schema only takes the number.
//
// The operator's maintenance command and the serving processes may run
// different versions (the new image drains the old servers), so any later
// change to these tables must be additive and nullable: never rename, drop
// or retype a column, and never add a NOT NULL one without a default.
// TestPostgresMaintenanceColumnsPinned holds the column set.
const maintenanceSchema = `
CREATE TABLE server_maintenance(id INTEGER PRIMARY KEY CHECK(id=1),started_at TIMESTAMPTZ NOT NULL,ends_at TIMESTAMPTZ NOT NULL,reason TEXT NOT NULL,started_by TEXT NOT NULL);
CREATE TABLE server_maintenance_instances(instance TEXT PRIMARY KEY,started_at TIMESTAMPTZ NOT NULL,seen_at TIMESTAMPTZ NOT NULL,drained_for TIMESTAMPTZ,drained_at TIMESTAMPTZ);
`

// maintenanceSchemaVersion is the first schema with the drain's tables.
const maintenanceSchemaVersion = 48

// OpenPostgresMaintenance opens the hosted authority for operator
// maintenance. Unlike OpenPostgresOperator it accepts any schema that has
// the drain's tables, older or newer than this binary's: the deploy runs it
// from the new image before the swap, against the running server's schema.
// It touches only the drain's tables and never migrates.
func OpenPostgresMaintenance(ctx context.Context, dsn string) (*Store, error) {
	return openPostgresOperator(ctx, dsn, func(version int) error {
		if version < maintenanceSchemaVersion {
			return fmt.Errorf("Postgres schema is version %d, older than the server drain (%d): the running server can't drain, so skip maintenance for this deploy", version, maintenanceSchemaVersion)
		}
		return nil
	})
}

// Maintenance is the drain state as the database's clock reads it.
type Maintenance struct {
	Active    bool      `json:"active"`
	StartedAt time.Time `json:"started_at,omitzero"`
	EndsAt    time.Time `json:"ends_at,omitzero"`
	Reason    string    `json:"reason,omitempty"`
	StartedBy string    `json:"started_by,omitempty"`
	// Now is the database's clock at the read.
	Now time.Time `json:"-"`
}

// Remaining is how long the maintenance has left at the read, 0 when none.
func (m Maintenance) Remaining() time.Duration {
	if !m.Active {
		return 0
	}
	return max(m.EndsAt.Sub(m.Now), 0)
}

// MaintenanceInstance is one serving process as the drain sees it.
type MaintenanceInstance struct {
	Instance string `json:"instance"`
	// Live is a report within InstanceLiveWindow.
	Live bool `json:"live"`
	// Drained is a drain reported for the current maintenance.
	Drained bool `json:"drained"`
}

// ErrMaintenanceInput is an invalid reason or TTL.
var ErrMaintenanceInput = errors.New("invalid maintenance request")

func (s *Store) requireHosted() error {
	if !s.postgres {
		return errors.New("maintenance is only available on the hosted authority")
	}
	return nil
}

// MaintenanceState reads the drain state. A row past its end is not active.
func (s *Store) MaintenanceState(ctx context.Context) (Maintenance, error) {
	if err := s.requireHosted(); err != nil {
		return Maintenance{}, err
	}
	var m Maintenance
	var started, ends sql.NullTime
	var reason, by sql.NullString
	err := s.db.QueryRowContext(ctx, `SELECT now(),m.started_at,m.ends_at,m.reason,m.started_by FROM (SELECT 1) one LEFT JOIN server_maintenance m ON m.id=1`).Scan(&m.Now, &started, &ends, &reason, &by)
	if err != nil {
		return Maintenance{}, err
	}
	if started.Valid && ends.Valid && m.Now.Before(ends.Time) {
		m.Active, m.StartedAt, m.EndsAt, m.Reason, m.StartedBy = true, started.Time, ends.Time, reason.String, by.String
	}
	return m, nil
}

// validMaintenanceReason is 1–200 characters with no control character.
func validMaintenanceReason(reason string) bool {
	if strings.TrimSpace(reason) == "" || len([]rune(reason)) > maintenanceReasonMax {
		return false
	}
	return strings.IndexFunc(reason, unicode.IsControl) < 0
}

// StartMaintenance puts the servers in maintenance for ttl. Starting again
// while one is active extends it and keeps its start, so the drains already
// reported for it still count.
func (s *Store) StartMaintenance(ctx context.Context, reason, by string, ttl time.Duration) (Maintenance, error) {
	if err := s.requireHosted(); err != nil {
		return Maintenance{}, err
	}
	if !validMaintenanceReason(reason) || !validMaintenanceReason(by) || ttl <= 0 || ttl > MaintenanceMaxTTL {
		return Maintenance{}, ErrMaintenanceInput
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Maintenance{}, err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `INSERT INTO server_maintenance(id,started_at,ends_at,reason,started_by) VALUES(1,now(),now()+make_interval(secs=>$1),$2,$3)
		ON CONFLICT(id) DO UPDATE SET ends_at=EXCLUDED.ends_at,reason=EXCLUDED.reason,started_by=EXCLUDED.started_by,
		started_at=CASE WHEN server_maintenance.ends_at>now() THEN server_maintenance.started_at ELSE EXCLUDED.started_at END`, ttl.Seconds(), reason, by); err != nil {
		return Maintenance{}, err
	}
	if _, err = tx.ExecContext(ctx, `SELECT pg_notify($1,'start')`, MaintenanceChannel); err != nil {
		return Maintenance{}, err
	}
	if err = tx.Commit(); err != nil {
		return Maintenance{}, err
	}
	return s.MaintenanceState(ctx)
}

// EndMaintenance ends maintenance; ended is false when there was none
// active.
func (s *Store) EndMaintenance(ctx context.Context) (bool, error) {
	return s.clearMaintenance(ctx, `DELETE FROM server_maintenance WHERE id=1 RETURNING ends_at>now()`)
}

// ClearMaintenanceAtStart is a serving process's start: it ends the
// maintenance, which began before it, since the new binary is up (the
// single-instance deploy's undo), and prunes instance rows unseen for a
// day. Cleared is whether an active maintenance ended.
func (s *Store) ClearMaintenanceAtStart(ctx context.Context) (bool, error) {
	cleared, err := s.clearMaintenance(ctx, `DELETE FROM server_maintenance WHERE id=1 RETURNING ends_at>now()`)
	if err != nil {
		return false, err
	}
	_, err = s.db.ExecContext(ctx, `DELETE FROM server_maintenance_instances WHERE seen_at<now()-make_interval(secs=>$1)`, instancePrune.Seconds())
	return cleared, err
}

func (s *Store) clearMaintenance(ctx context.Context, del string) (bool, error) {
	if err := s.requireHosted(); err != nil {
		return false, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	active := false
	if err = tx.QueryRowContext(ctx, del).Scan(&active); errors.Is(err, sql.ErrNoRows) {
		return false, nil
	} else if err != nil {
		return false, err
	}
	if _, err = tx.ExecContext(ctx, `SELECT pg_notify($1,'end')`, MaintenanceChannel); err != nil {
		return false, err
	}
	return active, tx.Commit()
}

// ReportServerInstance records that instance is serving now, and, with a
// drainedFor time, that it has drained for the maintenance that started
// then. A zero drainedFor clears its drain.
func (s *Store) ReportServerInstance(ctx context.Context, instance string, drainedFor time.Time) error {
	if err := s.requireHosted(); err != nil {
		return err
	}
	drained := sql.NullTime{Time: drainedFor, Valid: !drainedFor.IsZero()}
	_, err := s.db.ExecContext(ctx, `INSERT INTO server_maintenance_instances(instance,started_at,seen_at,drained_for,drained_at) VALUES($1,now(),now(),$2,CASE WHEN $2::timestamptz IS NULL THEN NULL ELSE now() END)
		ON CONFLICT(instance) DO UPDATE SET seen_at=now(),drained_for=EXCLUDED.drained_for,
		drained_at=CASE WHEN EXCLUDED.drained_for IS NULL THEN NULL WHEN server_maintenance_instances.drained_for IS NOT DISTINCT FROM EXCLUDED.drained_for THEN server_maintenance_instances.drained_at ELSE now() END`, instance, drained)
	return err
}

// MaintenanceInstances lists the instances that reported within a day,
// live first.
func (s *Store) MaintenanceInstances(ctx context.Context) ([]MaintenanceInstance, error) {
	if err := s.requireHosted(); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT i.instance,i.seen_at>now()-make_interval(secs=>$1),COALESCE(m.ends_at>now() AND i.drained_for=m.started_at,false)
		FROM server_maintenance_instances i LEFT JOIN server_maintenance m ON m.id=1 ORDER BY 2 DESC,i.instance`, InstanceLiveWindow.Seconds())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []MaintenanceInstance{}
	for rows.Next() {
		var i MaintenanceInstance
		if err = rows.Scan(&i.Instance, &i.Live, &i.Drained); err != nil {
			return nil, err
		}
		out = append(out, i)
	}
	return out, rows.Err()
}

// ServedInMaintenance reports whether a draining server still answers r:
// only a pure read, whose transaction Postgres keeps read-only. Its
// heartbeat and lease renewal, written after the read, are skipped while
// the store is quiet (SetQuiet).
func ServedInMaintenance(r Request) bool { return pureRead(r) }

// SetQuiet makes the store skip the writes a pure read makes after it
// commits (a heartbeat's last_seen, a lease renewal) while quiet answers
// true: a draining server writes nothing for a read.
func (s *Store) SetQuiet(quiet func() bool) { s.quiet = quiet }

func (s *Store) isQuiet() bool { return s.quiet != nil && s.quiet() }
