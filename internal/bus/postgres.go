package bus

import (
	"context"
	"database/sql"
	"errors"
	"net/mail"
	"strings"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
)

// normalPrincipal matches the server's verified-token form (lowercase), so a
// stored grant compares equal to the principal a request is executed as.
func normalPrincipal(p string) string { return strings.ToLower(strings.TrimSpace(p)) }
func validPrincipal(p string) bool {
	v, e := mail.ParseAddress(p)
	return e == nil && v.Address == p && strings.Contains(p, "@") && len(p) <= 320 && !strings.ContainsAny(p, "\r\n")
}

// OpenPostgres opens only PostgreSQL. bootstrapOwner is a trusted operator setting,
// never a request field. ExecuteAs must receive a verified identity from the server.
func OpenPostgres(ctx context.Context, dsn, bootstrapOwner string) (*Store, error) {
	bootstrapOwner = normalPrincipal(bootstrapOwner)
	if dsn == "" || !validPrincipal(bootstrapOwner) {
		return nil, errors.New("Postgres DSN and bootstrap owner email are required")
	}
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
	if version > 4 {
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
