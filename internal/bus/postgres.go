package bus

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"

	"clawbridge/internal/principal"
)

// normalPrincipal matches the server's verified-token form (lowercase), so a
// stored grant compares equal to the principal a request is executed as.
func normalPrincipal(p string) string { return principal.Normal(p) }
func validPrincipal(p string) bool    { return principal.Valid(p) }

// OpenPostgres opens only PostgreSQL. bootstrapOwner is a trusted operator setting,
// never a request field. ExecuteAs must receive a verified identity from the server.
func OpenPostgres(ctx context.Context, dsn, bootstrapOwner string) (*Store, error) {
	bootstrapOwner = normalPrincipal(bootstrapOwner)
	if dsn == "" || !validPrincipal(bootstrapOwner) {
		return nil, errors.New("Postgres DSN and bootstrap owner email are required")
	}
	return openPostgres(ctx, dsn, bootstrapOwner)
}

// postgresSchemaVersion is the hosted schema version this binary writes.
const postgresSchemaVersion = 33

// OpenPostgresOperator opens the hosted authority for a host operator's
// maintenance command. It has no bootstrap owner, so no request run through
// it can initialize a workspace or unbind as the server owner.
// Unlike OpenPostgres it never migrates: it refuses unless the schema is
// exactly this binary's, so an operator image newer than the running server
// cannot upgrade the schema under it.
func OpenPostgresOperator(ctx context.Context, dsn string) (*Store, error) {
	if dsn == "" {
		return nil, errors.New("Postgres DSN is required")
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		return nil, errors.New("invalid Postgres configuration")
	}
	check, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	var version int
	if err = db.QueryRowContext(check, `SELECT COALESCE(MAX(version),0) FROM clawbridge_schema`).Scan(&version); err != nil {
		db.Close()
		// Only a missing schema table means this is not the server's
		// database; authentication, TLS and network failures are an
		// unreachable Postgres, whatever the schema.
		var state interface{ SQLState() string }
		if errors.As(err, &state) && state.SQLState() == "42P01" {
			return nil, errors.New("cannot read the ClawBridge schema version; is this the server's database?")
		}
		return nil, errors.New("Postgres unavailable; check the connection URL, credentials and network")
	}
	if version != postgresSchemaVersion {
		db.Close()
		return nil, fmt.Errorf("Postgres schema is version %d but this binary is version %d; run the operator command from the server's own release", version, postgresSchemaVersion)
	}
	return &Store{db: db, postgres: true}, nil
}

func openPostgres(ctx context.Context, dsn, bootstrapOwner string) (*Store, error) {
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		return nil, errors.New("invalid Postgres configuration")
	}
	db.SetMaxOpenConns(16)
	db.SetMaxIdleConns(4)
	s := &Store{db: db, postgres: true, bootstrapOwner: bootstrapOwner}
	startup, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	if err = s.migratePostgres(startup); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}
func (s *Store) migratePostgres(ctx context.Context) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return errors.New("Postgres unavailable")
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(882712310)`); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS clawbridge_schema(version INTEGER PRIMARY KEY)`); err != nil {
		return err
	}
	var version int
	err = tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(version),0) FROM clawbridge_schema`).Scan(&version)
	if err != nil {
		return err
	}
	if version > postgresSchemaVersion {
		return errors.New("Postgres schema is newer than this binary")
	}
	if version == 0 {
		if _, err = tx.ExecContext(ctx, postgresSchema); err != nil {
			return err
		}
	}
	if version < 2 {
		if _, err = tx.ExecContext(ctx, `ALTER TABLE messages ADD COLUMN coordination TEXT; ALTER TABLE sessions ADD COLUMN notification_policy TEXT NOT NULL DEFAULT 'accept' CHECK(notification_policy IN ('accept','hold','refuse')); INSERT INTO clawbridge_schema(version) VALUES(2)`); err != nil {
			return err
		}
	}
	if version < 3 {
		if _, err = tx.ExecContext(ctx, taskboardSchema+`INSERT INTO clawbridge_schema(version) VALUES(3)`); err != nil {
			return err
		}
	}
	if version < 4 {
		if _, err = tx.ExecContext(ctx, taskboardSchemaV4+`INSERT INTO clawbridge_schema(version) VALUES(4)`); err != nil {
			return err
		}
	}
	if version < 5 {
		if _, err = tx.ExecContext(ctx, assignmentSchemaV5+`INSERT INTO clawbridge_schema(version) VALUES(5)`); err != nil {
			return err
		}
	}
	if version < 6 {
		// Binding events are append-only, like history.
		if _, err = tx.ExecContext(ctx, identitySchemaV6+principalEventsAppendOnlyV6+`INSERT INTO clawbridge_schema(version) VALUES(6)`); err != nil {
			return err
		}
	}
	if version < 7 {
		if _, err = tx.ExecContext(ctx, commentSchemaV7+`INSERT INTO clawbridge_schema(version) VALUES(7)`); err != nil {
			return err
		}
	}
	if version < 8 {
		if _, err = tx.ExecContext(ctx, labelPolicySchemaV8+`INSERT INTO clawbridge_schema(version) VALUES(8)`); err != nil {
			return err
		}
	}
	if version < 9 {
		if _, err = tx.ExecContext(ctx, appIdentitySchemaV9+`INSERT INTO clawbridge_schema(version) VALUES(9)`); err != nil {
			return err
		}
	}
	if version < 10 {
		if _, err = tx.ExecContext(ctx, writeLedgerSchemaV10+`INSERT INTO clawbridge_schema(version) VALUES(10)`); err != nil {
			return err
		}
	}
	if version < 11 {
		if _, err = tx.ExecContext(ctx, markerEngineerSchemaV11+`INSERT INTO clawbridge_schema(version) VALUES(11)`); err != nil {
			return err
		}
	}
	if version < 12 {
		if _, err = tx.ExecContext(ctx, intakeSchemaV12+`INSERT INTO clawbridge_schema(version) VALUES(12)`); err != nil {
			return err
		}
	}
	if version < 13 {
		if _, err = tx.ExecContext(ctx, docSeedSchemaV13+`INSERT INTO clawbridge_schema(version) VALUES(13)`); err != nil {
			return err
		}
	}
	if version < 14 {
		if _, err = tx.ExecContext(ctx, statusPolicySchemaV14+`INSERT INTO clawbridge_schema(version) VALUES(14)`); err != nil {
			return err
		}
	}
	if version < 15 {
		if _, err = tx.ExecContext(ctx, healthSchemaV15+`INSERT INTO clawbridge_schema(version) VALUES(15)`); err != nil {
			return err
		}
	}
	if version < 16 {
		if _, err = tx.ExecContext(ctx, docPolicySchemaV16+`INSERT INTO clawbridge_schema(version) VALUES(16)`); err != nil {
			return err
		}
	}
	if version < 17 {
		if _, err = tx.ExecContext(ctx, dispatcherSchemaV17+`INSERT INTO clawbridge_schema(version) VALUES(17)`); err != nil {
			return err
		}
	}
	if version < 18 {
		if _, err = tx.ExecContext(ctx, authSchemaV18+`INSERT INTO clawbridge_schema(version) VALUES(18)`); err != nil {
			return err
		}
	}
	if version < 19 {
		if _, err = tx.ExecContext(ctx, healthPostSchemaV19+`INSERT INTO clawbridge_schema(version) VALUES(19)`); err != nil {
			return err
		}
	}
	if version < 20 {
		if _, err = tx.ExecContext(ctx, presenceSchemaV20+`INSERT INTO clawbridge_schema(version) VALUES(20)`); err != nil {
			return err
		}
	}
	if version < 21 {
		if _, err = tx.ExecContext(ctx, projectFlowSchemaV21+`INSERT INTO clawbridge_schema(version) VALUES(21)`); err != nil {
			return err
		}
	}
	if version < 22 {
		if _, err = tx.ExecContext(ctx, projectFlowSchemaV22+`INSERT INTO clawbridge_schema(version) VALUES(22)`); err != nil {
			return err
		}
	}
	if version < 23 {
		if _, err = tx.ExecContext(ctx, dispatcherModelSchemaV23+`INSERT INTO clawbridge_schema(version) VALUES(23)`); err != nil {
			return err
		}
	}
	if version < 24 {
		if _, err = tx.ExecContext(ctx, dispatcherCasesSchemaV24+`INSERT INTO clawbridge_schema(version) VALUES(24)`); err != nil {
			return err
		}
	}
	if version < 25 {
		if _, err = tx.ExecContext(ctx, dispatcherChatSchemaV25+`INSERT INTO clawbridge_schema(version) VALUES(25)`); err != nil {
			return err
		}
	}
	if version < 26 {
		if _, err = tx.ExecContext(ctx, contextSchemaV26+`INSERT INTO clawbridge_schema(version) VALUES(26)`); err != nil {
			return err
		}
	}
	if version < 27 {
		if _, err = tx.ExecContext(ctx, contextLevelsSchemaV27+`INSERT INTO clawbridge_schema(version) VALUES(27)`); err != nil {
			return err
		}
	}
	if version < 28 {
		if _, err = tx.ExecContext(ctx, dispatcherAdviseSchemaV28+`INSERT INTO clawbridge_schema(version) VALUES(28)`); err != nil {
			return err
		}
	}
	if version < 29 {
		if _, err = tx.ExecContext(ctx, deviceEventsSchemaV29+`INSERT INTO clawbridge_schema(version) VALUES(29)`); err != nil {
			return err
		}
	}
	if version < 30 {
		if _, err = tx.ExecContext(ctx, watchBudgetSchemaV30+`INSERT INTO clawbridge_schema(version) VALUES(30)`); err != nil {
			return err
		}
	}
	if version < 31 {
		if _, err = tx.ExecContext(ctx, inviteIssuedDeviceSchemaV31+`INSERT INTO clawbridge_schema(version) VALUES(31)`); err != nil {
			return err
		}
	}
	if version < 32 {
		if _, err = tx.ExecContext(ctx, deviceDormancySchemaV32+`INSERT INTO clawbridge_schema(version) VALUES(32)`); err != nil {
			return err
		}
	}
	if version < 33 {
		if _, err = tx.ExecContext(ctx, linearNamesSchemaV33+`INSERT INTO clawbridge_schema(version) VALUES(33)`); err != nil {
			return err
		}
	}
	return tx.Commit()
}

const postgresSchema = `
CREATE TABLE workspaces(id TEXT PRIMARY KEY,name TEXT NOT NULL UNIQUE,mode TEXT NOT NULL CHECK(mode='server'),retention_days INTEGER NOT NULL CHECK(retention_days BETWEEN 1 AND 3650),owner_principal TEXT NOT NULL,created_at TEXT NOT NULL);
CREATE TABLE members(workspace_id TEXT NOT NULL REFERENCES workspaces(id),principal TEXT NOT NULL,display_name TEXT NOT NULL DEFAULT '',added_by TEXT NOT NULL,added_at TEXT NOT NULL,PRIMARY KEY(workspace_id,principal));
CREATE TABLE projects(id TEXT PRIMARY KEY,workspace_id TEXT NOT NULL REFERENCES workspaces(id),slug TEXT NOT NULL,name TEXT NOT NULL,git_remote TEXT NOT NULL DEFAULT '',created_at TEXT NOT NULL,UNIQUE(workspace_id,slug),UNIQUE(workspace_id,id));
CREATE TABLE project_roles(project_id TEXT NOT NULL REFERENCES projects(id),principal TEXT NOT NULL,role TEXT NOT NULL CHECK(role IN ('owner','lead','member','reader')),granted_by TEXT NOT NULL,granted_at TEXT NOT NULL,PRIMARY KEY(project_id,principal));
CREATE TABLE sessions(sequence BIGINT GENERATED ALWAYS AS IDENTITY UNIQUE,id TEXT PRIMARY KEY,project_id TEXT NOT NULL REFERENCES projects(id),name TEXT NOT NULL,instance TEXT NOT NULL,owner_principal TEXT NOT NULL,role TEXT NOT NULL,agent_kind TEXT NOT NULL,model TEXT NOT NULL,host TEXT NOT NULL,account TEXT NOT NULL,transport TEXT NOT NULL,notes TEXT NOT NULL,status TEXT NOT NULL CHECK(status IN ('active','retired','moved')),successor TEXT, last_seen TEXT NOT NULL,created_at TEXT NOT NULL,updated_at TEXT NOT NULL,UNIQUE(project_id,name),UNIQUE(project_id,id),FOREIGN KEY(project_id,successor) REFERENCES sessions(project_id,id));
CREATE TABLE messages(sequence BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,id TEXT NOT NULL UNIQUE,project_id TEXT NOT NULL REFERENCES projects(id),source_project_id TEXT NOT NULL,sender_id TEXT NOT NULL,address TEXT NOT NULL,thread_id TEXT NOT NULL,reply_to TEXT,kind TEXT NOT NULL,body TEXT NOT NULL,created_at TEXT NOT NULL,expires_at TEXT NOT NULL,created_by_principal TEXT NOT NULL,operation_id TEXT NOT NULL,payload_hash TEXT NOT NULL,purged_at TEXT,FOREIGN KEY(project_id,sender_id) REFERENCES sessions(project_id,id),FOREIGN KEY(source_project_id) REFERENCES projects(id),UNIQUE(sender_id,operation_id),UNIQUE(project_id,id),FOREIGN KEY(project_id,thread_id) REFERENCES messages(project_id,id) DEFERRABLE INITIALLY DEFERRED,FOREIGN KEY(project_id,reply_to) REFERENCES messages(project_id,id));
CREATE TABLE deliveries(message_id TEXT NOT NULL,project_id TEXT NOT NULL,recipient_id TEXT NOT NULL,delivered_at TEXT,read_at TEXT,PRIMARY KEY(message_id,project_id,recipient_id),FOREIGN KEY(project_id,message_id) REFERENCES messages(project_id,id),FOREIGN KEY(project_id,recipient_id) REFERENCES sessions(project_id,id));
CREATE TABLE history(sequence BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,id TEXT NOT NULL UNIQUE,workspace_id TEXT NOT NULL REFERENCES workspaces(id),project_id TEXT REFERENCES projects(id),entity TEXT NOT NULL,entity_key TEXT NOT NULL,action TEXT NOT NULL,details TEXT NOT NULL,changed_at TEXT NOT NULL,changed_by_principal TEXT NOT NULL,changed_by_session TEXT);
CREATE INDEX inbox_recipient ON deliveries(project_id,recipient_id,message_id);
CREATE INDEX project_messages ON messages(project_id,sequence);
CREATE INDEX principal_rate ON messages(created_by_principal,created_at);
CREATE INDEX project_history ON history(workspace_id,project_id,sequence);
CREATE FUNCTION clawbridge_immutable_history() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'history is append-only'; END $$;
CREATE TRIGGER immutable_history BEFORE UPDATE OR DELETE OR TRUNCATE ON history FOR EACH STATEMENT EXECUTE FUNCTION clawbridge_immutable_history();
INSERT INTO clawbridge_schema(version) VALUES(1);
`
