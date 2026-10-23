// Package bus shares messaging policy across local SQLite and hosted PostgreSQL authorities.
package bus

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/url"
	"os"
	"os/user"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

type Request struct {
	Operation string         `json:"operation"`
	Workspace string         `json:"workspace"`
	Project   string         `json:"project"`
	Session   string         `json:"session"`
	Args      map[string]any `json:"args"`
}
type Result struct {
	Source string `json:"source"`
	Data   any    `json:"data"`
}
type Store struct {
	db             *sql.DB
	principal      string
	postgres       bool
	bootstrapOwner string
	// identity is the verified account of a hosted request, set only by
	// ExecuteAsIdentity; requireAccount makes ExecuteAs refuse without one.
	identity       *Identity
	requireAccount bool
	// hostedDomain is the verified Google Workspace domain (hd) of a hosted
	// caller, empty for a consumer account; workspaces may require one.
	hostedDomain string
}

// SQLiteURIPath is the path part of a file: URI that SQLite resolves to path.
// A Windows path such as C:\dir\bus.db must become /C:/dir/bus.db: given as
// is, the drive letter and backslashes make the URI's authority or a relative
// path, and SQLite cannot open the database.
func SQLiteURIPath(goos, path string) string {
	if goos != "windows" {
		return path
	}
	p := strings.ReplaceAll(path, `\`, "/")
	if len(p) >= 2 && p[1] == ':' {
		return "/" + p
	}
	return p
}

func Open(path string) (*Store, error) {
	if path == "" {
		return nil, errors.New("explicit database path is required")
	}
	if path != ":memory:" {
		abs, err := filepath.Abs(path)
		if err != nil {
			return nil, err
		}
		path = abs
		if err = os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			return nil, err
		}
		// Never raw-open an existing SQLite inode: closing that descriptor can
		// release POSIX locks held by another SQLite connection in this process.
		f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0600)
		if err != nil && !errors.Is(err, os.ErrExist) {
			return nil, err
		}
		if err == nil {
			if err = f.Close(); err != nil {
				return nil, err
			}
		}
	}
	dsn := ":memory:?_pragma=foreign_keys(1)&_pragma=busy_timeout(5000)&_txlock=immediate"
	if path != ":memory:" {
		u := url.URL{Scheme: "file", Path: SQLiteURIPath(runtime.GOOS, path)}
		dsn = u.String() + "?_pragma=foreign_keys(1)&_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_txlock=immediate"
	}
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	u, err := user.Current()
	if err != nil {
		db.Close()
		return nil, fmt.Errorf("local OS identity: %w", err)
	}
	s := &Store{db: db, principal: "local:" + u.Uid + ":" + u.Username}
	if err = s.migrate(); err != nil {
		db.Close()
		return nil, fmt.Errorf("initialize SQLite: %w", err)
	}
	return s, nil
}
func (s *Store) Close() error { return s.db.Close() }

func (s *Store) migrate() error {
	tx, err := s.db.BeginTx(context.Background(), nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var version int
	if err = tx.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		return err
	}
	if version > 6 {
		return errors.New("database schema is newer than this ClawBridge binary")
	}
	if version == 0 {
		var tables int
		if err = tx.QueryRow("SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name NOT LIKE 'sqlite_%'").Scan(&tables); err != nil {
			return err
		}
		if tables != 0 {
			return errors.New("database is not an initialized ClawBridge store; refusing to modify unrelated tables")
		}
		if _, err = tx.Exec(schema); err != nil {
			return err
		}
	}
	if version < 2 {
		if _, err = tx.Exec(`ALTER TABLE messages ADD COLUMN coordination TEXT; ALTER TABLE sessions ADD COLUMN notification_policy TEXT NOT NULL DEFAULT 'accept' CHECK(notification_policy IN ('accept','hold','refuse')); PRAGMA user_version=2;`); err != nil {
			return err
		}
	}
	if version < 3 {
		if _, err = tx.Exec(taskboardSchema + `PRAGMA user_version=3;`); err != nil {
			return err
		}
	}
	if version < 4 {
		if _, err = tx.Exec(taskboardSchemaV4 + `PRAGMA user_version=4;`); err != nil {
			return err
		}
	}
	if version < 5 {
		if _, err = tx.Exec(assignmentSchemaV5 + `PRAGMA user_version=5;`); err != nil {
			return err
		}
	}
	if version < 6 {
		if _, err = tx.Exec(identitySchemaV6 + `PRAGMA user_version=6;`); err != nil {
			return err
		}
	}
	return tx.Commit()
}

const schema = `
CREATE TABLE IF NOT EXISTS workspaces(id TEXT PRIMARY KEY,name TEXT NOT NULL UNIQUE,mode TEXT NOT NULL CHECK(mode='local'),retention_days INTEGER NOT NULL,owner_principal TEXT NOT NULL,created_at TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS projects(id TEXT PRIMARY KEY,workspace_id TEXT NOT NULL REFERENCES workspaces(id),slug TEXT NOT NULL,name TEXT NOT NULL,git_remote TEXT NOT NULL DEFAULT '',created_at TEXT NOT NULL,UNIQUE(workspace_id,slug),UNIQUE(workspace_id,id));
CREATE TABLE IF NOT EXISTS sessions(id TEXT PRIMARY KEY,project_id TEXT NOT NULL REFERENCES projects(id),name TEXT NOT NULL,instance TEXT NOT NULL,owner_principal TEXT NOT NULL,role TEXT NOT NULL,agent_kind TEXT NOT NULL,model TEXT NOT NULL,host TEXT NOT NULL,account TEXT NOT NULL,transport TEXT NOT NULL,notes TEXT NOT NULL,status TEXT NOT NULL CHECK(status IN ('active','retired','moved')),successor TEXT REFERENCES sessions(id),last_seen TEXT NOT NULL,created_at TEXT NOT NULL,updated_at TEXT NOT NULL,UNIQUE(project_id,name),UNIQUE(project_id,id));
CREATE TABLE IF NOT EXISTS messages(sequence INTEGER PRIMARY KEY AUTOINCREMENT,id TEXT NOT NULL UNIQUE,project_id TEXT NOT NULL REFERENCES projects(id),source_project_id TEXT NOT NULL,sender_id TEXT NOT NULL,address TEXT NOT NULL,thread_id TEXT NOT NULL REFERENCES messages(id) DEFERRABLE INITIALLY DEFERRED,reply_to TEXT REFERENCES messages(id),kind TEXT NOT NULL,body TEXT NOT NULL,created_at TEXT NOT NULL,expires_at TEXT NOT NULL,created_by_principal TEXT NOT NULL,operation_id TEXT NOT NULL,payload_hash TEXT NOT NULL,FOREIGN KEY(project_id,sender_id) REFERENCES sessions(project_id,id),FOREIGN KEY(source_project_id) REFERENCES projects(id),UNIQUE(sender_id,operation_id),UNIQUE(project_id,id));
CREATE TABLE IF NOT EXISTS deliveries(message_id TEXT NOT NULL,project_id TEXT NOT NULL,recipient_id TEXT NOT NULL,delivered_at TEXT,read_at TEXT,PRIMARY KEY(message_id,project_id,recipient_id),FOREIGN KEY(project_id,message_id) REFERENCES messages(project_id,id),FOREIGN KEY(project_id,recipient_id) REFERENCES sessions(project_id,id));
CREATE TABLE IF NOT EXISTS history(sequence INTEGER PRIMARY KEY AUTOINCREMENT,id TEXT NOT NULL UNIQUE,workspace_id TEXT NOT NULL REFERENCES workspaces(id),project_id TEXT REFERENCES projects(id),entity TEXT NOT NULL,entity_key TEXT NOT NULL,action TEXT NOT NULL,details TEXT NOT NULL,changed_at TEXT NOT NULL,changed_by_principal TEXT NOT NULL,changed_by_session TEXT);
CREATE INDEX IF NOT EXISTS inbox_recipient ON deliveries(project_id,recipient_id,message_id);
CREATE INDEX IF NOT EXISTS project_messages ON messages(project_id,sequence);
CREATE INDEX IF NOT EXISTS project_history ON history(workspace_id,project_id,sequence);
CREATE TRIGGER IF NOT EXISTS immutable_history_update BEFORE UPDATE ON history BEGIN SELECT RAISE(ABORT,'history is append-only'); END;
CREATE TRIGGER IF NOT EXISTS immutable_history_delete BEFORE DELETE ON history BEGIN SELECT RAISE(ABORT,'history is append-only'); END;
PRAGMA user_version=1;
`

func newID(prefix string) string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	return prefix + hex.EncodeToString(b[:])
}

const timestampLayout = "2006-01-02T15:04:05.000000000Z07:00"

func stamp() string                         { return time.Now().UTC().Format(timestampLayout) }
func str(a map[string]any, k string) string { v, _ := a[k].(string); return v }
func asJSON(v any) string                   { b, _ := json.Marshal(v); return string(b) }
func query(ctx context.Context, tx *transaction, q string, args ...any) ([]map[string]any, error) {
	rows, err := tx.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil {
		return nil, err
	}
	out := []map[string]any{}
	for rows.Next() {
		v := make([]any, len(cols))
		p := make([]any, len(cols))
		for i := range v {
			p[i] = &v[i]
		}
		if err = rows.Scan(p...); err != nil {
			return nil, err
		}
		m := map[string]any{}
		for i, c := range cols {
			if b, ok := v[i].([]byte); ok {
				v[i] = string(b)
			}
			m[c] = v[i]
			if c == "coordination" && v[i] == nil {
				delete(m, c)
				continue
			}
			if c == "transport" || c == "details" || c == "coordination" || c == "scope_paths" {
				var decoded any
				if t, ok := v[i].(string); ok && json.Unmarshal([]byte(t), &decoded) == nil {
					if tx.postgres && c == "details" {
						decoded = hostedAuditDetails(decoded)
					}
					m[c] = decoded
				}
			}
		}
		decorateSession(m)
		out = append(out, m)
	}
	return out, rows.Err()
}
func one(ctx context.Context, tx *transaction, q string, args ...any) (map[string]any, error) {
	r, err := query(ctx, tx, q, args...)
	if err != nil {
		return nil, err
	}
	if len(r) != 1 {
		return nil, errNotFound
	}
	return r[0], nil
}
func (s *Store) audit(ctx context.Context, tx *transaction, w, p, session, entity, key, action string, details any) error {
	if tx.actorID != "" {
		session = tx.actorID
	}
	var project, actor any
	if p != "" {
		project = p
	}
	if session != "" {
		actor = session
	}
	if s.postgres {
		details = hostedAuditDetails(details)
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO history(id,workspace_id,project_id,entity,entity_key,action,details,changed_at,changed_by_principal,changed_by_session) VALUES(?,?,?,?,?,?,?,?,?,?)`, newID("evt_"), w, project, entity, key, action, asJSON(details), stamp(), s.principal, actor)
	return err
}

var namePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)

func nameOK(v string) error {
	if !namePattern.MatchString(v) {
		return errors.New("name must be 1–128 letters, digits, dot, underscore or hyphen")
	}
	return nil
}

// sessionIDShaped reports whether a session address is in the ses_ ID
// namespace. Such addresses resolve by exact ID only and are never names.
func sessionIDShaped(v string) bool {
	return len(v) >= 4 && strings.EqualFold(v[:4], "ses_")
}
func sessionNameOK(v string) error {
	if err := nameOK(v); err != nil {
		return err
	}
	if sessionIDShaped(v) {
		return errors.New("session names cannot use the reserved ses_ session-ID prefix")
	}
	return nil
}

// sessionAddress returns the SQL predicate and arguments that resolve a
// session address: an exact ID for ID-shaped addresses, otherwise ID or name.
func sessionAddress(v string) (string, []any) {
	if sessionIDShaped(v) {
		return "id=?", []any{v}
	}
	return "(id=? OR name=?)", []any{v, v}
}
func validateArgs(op string, a map[string]any) error {
	allowed := map[string]string{
		"taskboard_get":           "instance",
		"taskboard_get_v2":        "instance",
		"taskboard_set_v2":        "instance expected_revision mode linear_team_ids linear_default_team_id linear_project_ids linear_workspace_id",
		"taskboard_clear_v2":      "instance expected_revision",
		"taskboard_access":        "instance",
		"taskboard_set":           "instance expected_revision linear_team_ids linear_default_team_id linear_project_id linear_workspace_id",
		"taskboard_clear":         "instance expected_revision",
		"inbox_status":            "instance after",
		"notification_status":     "instance after",
		"set_notification_policy": "instance policy",
		"assignment_propose":      "instance issue_uuid issue_identifier task_role scope_repo scope_paths",
		"assignment_confirm":      "instance target_session revision allow_overlap",
		"assignment_release":      "instance target_session revision",
		"coordinator_set":         "instance target_session",
		"workspace_init":          "mode name retention_days", "project_create": "slug name git_remote", "list_projects": "", "list_members": "", "register_session": "instance resume role agent_kind model host account notes transport", "list_sessions": "instance status role limit after issue scope_path assignment_state", "send": "instance to body kind thread_id operation_id coordination", "reply": "instance message_id body kind operation_id coordination", "inbox": "instance unread_only limit after", "mark_read": "instance ids", "update_session": "instance target_session role model host account notes transport", "retire_session": "instance target_session successor status", "history": "instance entity key limit after", "whoami": "instance", "message_status": "instance message_id", "resolve_project": "git_remote instance", "snapshot": "instance", "project_role_set": "instance principal role", "project_role_revoke": "instance principal", "purge_expired": "instance", "principal_unbind": "principal allow_previous", "workspace_set_domains": "domains",
	}
	keys, ok := allowed[op]
	if !ok {
		return fmt.Errorf("unsupported operation %q", op)
	}
	for k, v := range a {
		if !strings.Contains(" "+keys+" ", " "+k+" ") {
			return fmt.Errorf("unsupported argument %q for %s", k, op)
		}
		switch k {
		case "retention_days", "limit", "after", "expected_revision":
			if _, err := integer(v); err != nil {
				return fmt.Errorf("%s: %w", k, err)
			}
		case "linear_project_id", "linear_workspace_id":
			if v != nil {
				if _, ok := v.(string); !ok {
					return fmt.Errorf("%s must be UUID or null", k)
				}
			}
		case "coordination":
			if _, err := ParseCoordination(v); err != nil {
				return err
			}
		case "transport":
			if _, ok := v.(map[string]any); !ok {
				return errors.New("transport must be an object")
			}
		case "ids", "linear_team_ids", "linear_project_ids", "scope_paths", "domains":
			switch v.(type) {
			case []string, []any:
			default:
				return errors.New("ids must be an array of strings")
			}
		case "unread_only", "resume", "allow_overlap", "allow_previous":
			if _, ok := v.(bool); !ok {
				return fmt.Errorf("%s must be boolean", k)
			}
		default:
			if _, ok := v.(string); !ok {
				return fmt.Errorf("%s must be a string", k)
			}
		}
	}
	if op == "taskboard_set" || op == "taskboard_clear" || op == "taskboard_set_v2" || op == "taskboard_clear_v2" {
		if _, err := taskboardExpected(a); err != nil {
			return err
		}
		if op == "taskboard_set_v2" {
			if _, err := parseTaskboardMappingV2(a); err != nil {
				return err
			}
		}
		if op == "taskboard_set" {
			if _, err := parseTaskboardMapping(a); err != nil {
				return err
			}
		}
	}
	encoded, err := json.Marshal(a)
	if err != nil {
		return errors.New("arguments must be JSON serializable")
	}
	if len(encoded) > 80000 {
		return errors.New("request arguments exceed size limit")
	}
	return nil
}
func integer(v any) (int, error) {
	switch n := v.(type) {
	case int:
		return n, nil
	case int64:
		if n >= 0 && n <= 9007199254740991 {
			return int(n), nil
		}
	case float64:
		if n >= 0 && n <= 9007199254740991 && n == float64(int(n)) {
			return int(n), nil
		}
	case json.Number:
		i, e := n.Int64()
		if e == nil && i >= 0 && i <= 9007199254740991 {
			return int(i), nil
		}
	}
	return 0, errors.New("expected bounded integer")
}
func limit(a map[string]any) (int, error) {
	if v, ok := a["limit"]; ok {
		n, e := integer(v)
		if e != nil || n < 1 || n > 1000 {
			return 0, errors.New("limit must be 1–1000")
		}
		return n, nil
	}
	return 100, nil
}

// rejectNUL refuses a NUL byte anywhere in a request's text before it reaches
// a database: Postgres rejects it as a data exception, and SQLite would store
// a string other tools truncate.
func rejectNUL(r Request) error {
	var walk func(v any) bool
	walk = func(v any) bool {
		switch x := v.(type) {
		case string:
			return strings.ContainsRune(x, 0)
		case []string:
			for _, s := range x {
				if strings.ContainsRune(s, 0) {
					return true
				}
			}
		case []any:
			for _, item := range x {
				if walk(item) {
					return true
				}
			}
		case map[string]any:
			for key, item := range x {
				if strings.ContainsRune(key, 0) || walk(item) {
					return true
				}
			}
		}
		return false
	}
	if walk(r.Operation) || walk(r.Workspace) || walk(r.Project) || walk(r.Session) || walk(r.Args) {
		return invalid("text arguments must not contain NUL bytes")
	}
	return nil
}

func (s *Store) Execute(ctx context.Context, r Request) (Result, error) {
	if s.postgres {
		return Result{}, errors.New("Postgres requires trusted ExecuteAs identity")
	}
	result, err := s.executeRequest(ctx, r)
	if r.Operation == "inbox_status" || r.Operation == "notification_status" || strings.HasPrefix(r.Operation, "taskboard_") {
		err = classify(err)
	}
	return result, err
}
func (s *Store) ExecuteAs(ctx context.Context, principal string, r Request) (Result, error) {
	if !s.postgres {
		return Result{}, errors.New("ExecuteAs is only available on the hosted authority")
	}
	if s.requireAccount {
		return Result{}, errors.New("this authority binds accounts; run hosted requests through ExecuteAsIdentity")
	}
	if !validPrincipal(principal) {
		return Result{}, errors.New("verified email principal is required")
	}
	scoped := *s
	scoped.principal = principal
	result, err := scoped.executeRequest(ctx, r)
	return result, classify(err)
}

// pureRead reports whether a hosted request only reads. Such a request runs in
// a read-only REPEATABLE READ transaction without the principal and workspace
// locks, so reads never queue behind writes; Postgres refuses any write it
// attempts. Reads that name a session heartbeat it (sessions.last_seen), and
// inbox stamps deliveries, so those keep the locks.
func pureRead(r Request) bool {
	switch r.Operation {
	case "snapshot", "inbox_status", "notification_status", "taskboard_get", "taskboard_get_v2", "taskboard_access", "list_projects", "list_members":
		return true
	case "list_sessions", "history", "whoami":
		// Naming a session with its instance makes it a heartbeat read: the
		// read itself is pure and the heartbeat follows (see beat).
		return (r.Session == "") == (str(r.Args, "instance") == "")
	case "resolve_project":
		return r.Session == "" && str(r.Args, "instance") == ""
	}
	return false
}

// heartbeat is the session a pure read names, recorded after the read.
type heartbeat struct{ workspace, project, session string }

// logHeartbeatSkipped reports a heartbeat that failed (not one skipped for a
// busy lock); tests replace it. It never fails the read.
var logHeartbeatSkipped = func(operation string, err error) {
	log.Printf("clawbridge: %s heartbeat skipped: %v", operation, err)
}

// beat records a heartbeat read's last_seen after the read has committed, in
// its own short transaction. It takes the workspace lock only if it is free,
// so the read never queues behind a write: a busy lock skips the beat
// silently, and any failure (a serialization error, a timeout) is logged and
// skipped. The session's next read records it.
func (s *Store) beat(ctx context.Context, r Request, b *heartbeat) (string, bool) {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	raw, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		logHeartbeatSkipped(r.Operation, err)
		return "", false
	}
	tx := &transaction{Tx: raw, postgres: true}
	defer tx.Rollback()
	var acquired bool
	now := stamp()
	if _, err = tx.ExecContext(ctx, `SET LOCAL lock_timeout='1s'`); err == nil {
		err = tx.QueryRowContext(ctx, `SELECT pg_try_advisory_xact_lock(hashtextextended(?,0))`, "workspace:"+b.workspace).Scan(&acquired)
	}
	if err == nil && acquired {
		if _, err = tx.ExecContext(ctx, `UPDATE sessions SET last_seen=? WHERE project_id=? AND id=?`, now, b.project, b.session); err == nil {
			err = tx.Commit()
		}
	}
	if err != nil {
		logHeartbeatSkipped(r.Operation, err)
	}
	return now, err == nil && acquired
}

// logMisclassifiedRead reports a pure read that tried to write; tests
// replace it. Only the operation name is logged, never arguments.
var logMisclassifiedRead = func(operation string) {
	log.Printf("clawbridge: %s ran as a pure read but attempted a write (SQLSTATE 25006); its pureRead classification is wrong", operation)
}

// misclassifiedRead turns a write refused inside a pure read's read-only
// transaction (SQLSTATE 25006) into invalid, and logs it: the classification
// is a bug to surface, not an outage that would send clients to a mirror.
// Anywhere else 25006 means the database itself is read-only (failover, hot
// standby, maintenance), which classify reports as unavailable.
func misclassifiedRead(tx *transaction, r Request, err error) error {
	var state interface{ SQLState() string }
	if tx.pure && errors.As(err, &state) && state.SQLState() == "25006" {
		logMisclassifiedRead(r.Operation)
		return &serviceError{code: "invalid", cause: err}
	}
	return err
}
func (s *Store) executeRequest(ctx context.Context, r Request) (Result, error) {
	if err := rejectNUL(r); err != nil {
		return Result{}, err
	}
	if err := validateArgs(r.Operation, r.Args); err != nil {
		return Result{}, err
	}
	pure := s.postgres && pureRead(r)
	options := &sql.TxOptions{}
	if pure {
		options.Isolation = sql.LevelRepeatableRead
		options.ReadOnly = true
	}
	raw, err := s.db.BeginTx(ctx, options)
	if err != nil {
		return Result{}, errors.New("authority unavailable; no write queued")
	}
	tx := &transaction{Tx: raw, postgres: s.postgres, pure: pure}
	defer tx.Rollback()
	if s.postgres {
		if _, err = tx.ExecContext(ctx, `SET LOCAL statement_timeout='12s'`); err != nil {
			return Result{}, err
		}
		if _, err = tx.ExecContext(ctx, `SET LOCAL lock_timeout='5s'`); err != nil {
			return Result{}, err
		}
	}
	if s.postgres && !tx.pure {
		// Serialize a principal's rate-limit accounting across its workspaces.
		if _, err = tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtextextended(?,0))`, "principal:"+s.principal); err != nil {
			return Result{}, err
		}
	}
	data, err := s.execute(ctx, tx, r)
	if err != nil {
		return Result{}, misclassifiedRead(tx, r, err)
	}
	if err = tx.Commit(); err != nil {
		return Result{}, errors.New("authority commit failed; no write queued")
	}
	if tx.beat != nil {
		if now, ok := s.beat(ctx, r, tx.beat); ok && r.Operation == "whoami" {
			// whoami shows the heartbeat it just recorded; when the beat was
			// skipped it shows the last recorded one.
			if session, _ := data.(map[string]any)["session"].(map[string]any); session != nil {
				session["last_seen"] = now
				decorateSession(session)
			}
		}
	}
	source := "local-authority"
	if s.postgres {
		source = "server"
	}
	if s.postgres {
		redactPeerInstances(data, s.principal)
	}
	return Result{Source: source, Data: data}, nil
}
func (s *Store) execute(ctx context.Context, tx *transaction, r Request) (any, error) {
	a := r.Args
	if r.Operation == "workspace_init" {
		if s.postgres {
			if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(882712311)`); err != nil {
				return nil, err
			}
		}
		out, err := s.initWorkspace(ctx, tx, r)
		if err == nil {
			err = s.requireBound(ctx, tx)
		}
		return out, err
	}
	if r.Operation == "resolve_project" {
		out, err := s.resolveInTransaction(ctx, tx, r)
		if err == nil {
			err = s.requireBound(ctx, tx)
		}
		return out, err
	}
	if r.Operation == "principal_unbind" {
		return s.unbindPrincipal(ctx, tx, a)
	}
	if !s.postgres && (r.Operation == "snapshot" || r.Operation == "project_role_set" || r.Operation == "project_role_revoke" || r.Operation == "purge_expired" || r.Operation == "workspace_set_domains") {
		return nil, errors.New("operation requires hosted authority")
	}
	if r.Workspace == "" {
		return nil, errors.New("workspace is required; run setup --local explicitly")
	}
	w, err := one(ctx, tx, `SELECT * FROM workspaces WHERE id=? OR name=?`, r.Workspace, r.Workspace)
	if err != nil {
		return nil, err
	}
	wid := w["id"].(string)
	tx.workspace = wid
	expectedMode := "local"
	if s.postgres {
		expectedMode = "server"
	}
	if w["mode"] != expectedMode {
		return nil, errors.New("workspace storage mode mismatch; no fallback or queued write")
	}
	if s.postgres {
		if _, err = one(ctx, tx, `SELECT principal FROM members WHERE workspace_id=? AND principal=?`, wid, s.principal); err != nil {
			return nil, scopedError(err, "workspace membership required", "forbidden")
		}
		if !tx.pure {
			if _, err = tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtextextended(?,0))`, "workspace:"+wid); err != nil {
				return nil, err
			}
			// Read the allow-list again under the workspace lock its setter
			// holds, so a write never commits after a restriction it predates.
			var domains string
			if err = tx.QueryRowContext(ctx, `SELECT allowed_domains FROM workspaces WHERE id=?`, wid).Scan(&domains); err != nil {
				return nil, err
			}
			w["allowed_domains"] = domains
		}
		// The workspace owner may always repair or clear the allow-list.
		if r.Operation != "workspace_set_domains" || w["owner_principal"] != s.principal {
			if err = s.domainGate(w["allowed_domains"]); err != nil {
				return nil, err
			}
		}
		// A member's account must be the one bound to the principal; a
		// non-member or a refused domain was refused above and writes no
		// binding.
		if err = s.requireBound(ctx, tx); err != nil {
			return nil, err
		}
		if r.Operation == "workspace_set_domains" {
			return s.setWorkspaceDomains(ctx, tx, w, a)
		}
		if r.Operation == "project_create" && w["owner_principal"] != s.principal {
			return nil, errors.New("workspace owner required to create projects")
		}
	} else if w["owner_principal"] != s.principal {
		return nil, errors.New("local workspace belongs to a different OS user")
	}
	if r.Operation == "project_create" {
		return s.createProject(ctx, tx, wid, a)
	}
	if r.Operation == "list_projects" {
		return s.listProjects(ctx, tx, w)
	}
	if r.Project == "" {
		return nil, errors.New("project is required; select an existing project explicitly")
	}
	p, err := one(ctx, tx, `SELECT * FROM projects WHERE workspace_id=? AND (id=? OR slug=?)`, wid, r.Project, r.Project)
	if err != nil {
		return nil, err
	}
	pid := p["id"].(string)
	projectRole := "owner"
	if s.postgres {
		projectRole, err = s.projectRole(ctx, tx, w, pid)
		if err != nil {
			return nil, err
		}
		switch r.Operation {
		case "register_session", "send", "reply", "update_session", "retire_session", "assignment_propose", "assignment_confirm", "assignment_release", "coordinator_set":
			if projectRole == "reader" {
				return nil, errors.New("project member permission required")
			}
		case "project_role_set", "project_role_revoke", "purge_expired":
			if projectRole != "owner" {
				return nil, errors.New("project owner permission required")
			}
		}
		if r.Operation == "project_role_set" {
			if err = s.touchReadActor(ctx, tx, pid, r); err != nil {
				return nil, err
			}
			return s.setProjectRole(ctx, tx, wid, pid, a)
		}
		if r.Operation == "project_role_revoke" {
			if err = s.touchReadActor(ctx, tx, pid, r); err != nil {
				return nil, err
			}
			return s.revokeProjectRole(ctx, tx, w, wid, pid, a)
		}
		if r.Operation == "list_members" {
			return s.listMembers(ctx, tx, pid)
		}
		if r.Operation == "purge_expired" {
			if err = s.touchReadActor(ctx, tx, pid, r); err != nil {
				return nil, err
			}
			return s.purgeExpired(ctx, tx, wid, pid)
		}
		if r.Operation == "snapshot" {
			return s.snapshot(ctx, tx, w, p, r)
		}
	}
	if r.Operation == "register_session" {
		return s.register(ctx, tx, wid, pid, r)
	}
	if r.Operation == "list_sessions" {
		if err = s.touchReadActor(ctx, tx, pid, r); err != nil {
			return nil, err
		}
		n, e := limit(a)
		if e != nil {
			return nil, e
		}
		status, role := str(a, "status"), str(a, "role")
		if status != "" && status != "active" && status != "retired" && status != "moved" {
			return nil, errors.New("invalid session status")
		}
		after, e := cursor(a)
		if e != nil {
			return nil, e
		}
		return s.listSessions(ctx, tx, pid, status, role, a, after, n)
	}
	if r.Operation == "history" {
		if err = s.touchReadActor(ctx, tx, pid, r); err != nil {
			return nil, err
		}
		n, e := limit(a)
		if e != nil {
			return nil, e
		}
		entity, key := str(a, "entity"), str(a, "key")
		after, e := cursor(a)
		if e != nil {
			return nil, e
		}
		rows, e := query(ctx, tx, `SELECT * FROM history WHERE workspace_id=? AND project_id=? AND (?='' OR entity=?) AND (?='' OR entity_key=?) AND sequence>? ORDER BY sequence LIMIT ?`, wid, pid, entity, entity, key, key, after, n+1)
		if e != nil {
			return nil, e
		}
		return page("events", rows, n, after), nil
	}
	var actor map[string]any
	if r.Session != "" {
		match, args := sessionAddress(r.Session)
		actor, err = one(ctx, tx, `SELECT * FROM sessions WHERE project_id=? AND `+match, append([]any{pid}, args...)...)
		if err != nil {
			return nil, err
		}
		if actor["owner_principal"] != s.principal || str(a, "instance") == "" || actor["instance"] != str(a, "instance") {
			return nil, errors.New("session instance mismatch; pass the instance returned by register")
		}
	}
	if r.Operation == "taskboard_access" || r.Operation == "taskboard_get" || r.Operation == "taskboard_set" || r.Operation == "taskboard_clear" || r.Operation == "taskboard_get_v2" || r.Operation == "taskboard_set_v2" || r.Operation == "taskboard_clear_v2" {
		if r.Session == "" && str(a, "instance") != "" {
			return nil, errors.New("instance requires session")
		}
		if r.Operation == "taskboard_access" {
			access := TaskboardAccess{WorkspaceID: wid, ProjectID: pid, ProjectRole: projectRole, CanWrite: projectRole != "reader"}
			if actor != nil {
				access.SessionID = actor["id"].(string)
				access.CanWrite = access.CanWrite && actor["status"] == "active"
			}
			return accessMap(access), nil
		}
		if r.Operation != "taskboard_get" && r.Operation != "taskboard_get_v2" && r.Operation != "taskboard_access" {
			if projectRole != "owner" && projectRole != "lead" {
				return nil, errors.New("project lead or owner permission required")
			}
			if actor != nil && actor["status"] != "active" {
				return nil, errors.New("active actor permission required")
			}
		}
		if actor != nil {
			tx.actorID = actor["id"].(string)
		}
		return s.taskboard(ctx, tx, wid, pid, r.Operation, a)
	}
	// Metadata polling authorizes the actor above, but must never heartbeat it.
	if r.Operation == "inbox_status" || r.Operation == "notification_status" {
		if actor == nil || actor["status"] != "active" {
			return nil, errors.New("active owned session permission required for inbox status")
		}
		result, e := s.inboxStatus(ctx, tx, wid, pid, actor["id"].(string), a)
		if e == nil && r.Operation == "notification_status" {
			result.(map[string]any)["policy"] = actor["notification_policy"]
		}
		return result, e
	}
	if actor != nil {
		if tx.pure {
			tx.beat = &heartbeat{wid, pid, actor["id"].(string)}
		} else {
			now := stamp()
			if _, err = tx.ExecContext(ctx, `UPDATE sessions SET last_seen=? WHERE project_id=? AND id=?`, now, pid, actor["id"]); err != nil {
				return nil, err
			}
			actor["last_seen"] = now
		}
		decorateSession(actor)
	}
	if r.Operation == "whoami" {
		provenance, mode := "local-os-user-unverified", "local"
		if s.postgres {
			provenance, mode = "google-id-token", "cloud"
		}
		if actor != nil {
			live, e := liveAssignment(ctx, tx, pid, actor["id"].(string))
			if e != nil {
				return nil, e
			}
			actor["assignment"] = assignmentView(live, actor)
		}
		return map[string]any{"workspace": w, "project": p, "session": actor, "principal": s.principal, "identity_provenance": provenance, "verified": s.postgres, "storage_mode": mode, "online": true}, nil
	}
	if actor == nil {
		return nil, errors.New("session is required; register and pass --session and --instance")
	}
	sid := actor["id"].(string)
	tx.actorID = sid
	if assignmentOperation(r.Operation) {
		return s.assignment(ctx, tx, w, p, projectRole, actor, r)
	}
	target := actor
	if selector := str(a, "target_session"); selector != "" {
		if !s.postgres || (projectRole != "owner" && projectRole != "lead") {
			return nil, errors.New("project lead or owner permission required to target another session")
		}
		if actor["status"] != "active" {
			return nil, errors.New("active actor permission required for peer administration")
		}
		match, args := sessionAddress(selector)
		target, err = one(ctx, tx, `SELECT * FROM sessions WHERE project_id=? AND `+match, append([]any{pid}, args...)...)
		if err != nil {
			return nil, err
		}
		if projectRole == "lead" && target["owner_principal"] != s.principal {
			if err = s.leadMayAdminister(ctx, tx, w, pid, target, a); err != nil {
				return nil, err
			}
		}
	}
	if actor["status"] != "active" && r.Operation != "message_status" && r.Operation != "inbox" && r.Operation != "mark_read" && r.Operation != "retire_session" && r.Operation != "send" && r.Operation != "reply" {
		return nil, errors.New("session is retired or moved; it cannot perform this operation")
	}
	switch r.Operation {
	case "set_notification_policy":
		return s.setNotificationPolicy(ctx, tx, wid, pid, actor, a)
	case "send", "reply":
		return s.send(ctx, tx, wid, pid, actor, r)
	case "inbox":
		return s.inbox(ctx, tx, wid, pid, sid, a)
	case "mark_read":
		return s.markRead(ctx, tx, wid, pid, sid, a)
	case "message_status":
		return s.messageStatus(ctx, tx, pid, sid, str(a, "message_id"))
	case "update_session":
		return s.update(ctx, tx, wid, pid, target, a)
	case "retire_session":
		return s.retire(ctx, tx, wid, pid, target, a)
	}
	return nil, errors.New("unsupported operation")
}
func (s *Store) initWorkspace(ctx context.Context, tx *transaction, r Request) (any, error) {
	a := r.Args
	mode := "local"
	if s.postgres {
		mode = "server"
	}
	if str(a, "mode") != mode {
		return nil, fmt.Errorf("explicit mode=%s is required", mode)
	}
	name := r.Workspace
	if name == "" {
		name = str(a, "name")
	}
	if err := nameOK(name); err != nil {
		return nil, err
	}
	retention := 90
	if v, ok := a["retention_days"]; ok {
		retention, _ = integer(v)
	}
	if retention < 1 || retention > 3650 {
		return nil, errors.New("retention_days must be 1–3650")
	}
	existing, err := query(ctx, tx, `SELECT * FROM workspaces WHERE name=?`, name)
	if err != nil {
		return nil, err
	}
	if len(existing) > 0 {
		if existing[0]["owner_principal"] != s.principal || existing[0]["mode"] != mode || existing[0]["retention_days"] != int64(retention) {
			return nil, errors.New("workspace exists with conflicting owner, mode or retention")
		}
		return existing[0], nil
	}
	if s.postgres && s.principal != s.bootstrapOwner {
		return nil, errors.New("only the configured bootstrap owner may initialize a new workspace")
	}
	id := newID("ws_")
	_, err = tx.ExecContext(ctx, `INSERT INTO workspaces(id,name,mode,retention_days,owner_principal,created_at) VALUES(?,?,?,?,?,?)`, id, name, mode, retention, s.principal, stamp())
	if err != nil {
		return nil, err
	}
	if s.postgres {
		if _, err = tx.ExecContext(ctx, `INSERT INTO members(workspace_id,principal,added_by,added_at) VALUES(?,?,?,?)`, id, s.principal, s.principal, stamp()); err != nil {
			return nil, err
		}
	}
	if err = s.audit(ctx, tx, id, "", "", "workspace", id, "initialized", map[string]any{"mode": mode, "retention_days": retention}); err != nil {
		return nil, err
	}
	return one(ctx, tx, `SELECT * FROM workspaces WHERE id=?`, id)
}
func (s *Store) createProject(ctx context.Context, tx *transaction, wid string, a map[string]any) (any, error) {
	slug := str(a, "slug")
	if err := nameOK(slug); err != nil {
		return nil, err
	}
	name := str(a, "name")
	if name == "" {
		name = slug
	}
	if len(name) > 512 {
		return nil, errors.New("project name too long")
	}
	remote, err := NormalizeRemote(str(a, "git_remote"))
	if err != nil {
		return nil, err
	}
	id := newID("prj_")
	_, err = tx.ExecContext(ctx, `INSERT INTO projects(id,workspace_id,slug,name,git_remote,created_at) VALUES(?,?,?,?,?,?)`, id, wid, slug, name, remote, stamp())
	if err != nil {
		return nil, fmt.Errorf("create project (slug must be unique in workspace): %w", err)
	}
	if s.postgres {
		if _, err = tx.ExecContext(ctx, `INSERT INTO project_roles(project_id,principal,role,granted_by,granted_at) VALUES(?,?,?,?,?)`, id, s.principal, "owner", s.principal, stamp()); err != nil {
			return nil, err
		}
	}
	if err = s.audit(ctx, tx, wid, id, "", "project", id, "created", map[string]any{"slug": slug, "git_remote": remote}); err != nil {
		return nil, err
	}
	return one(ctx, tx, `SELECT * FROM projects WHERE id=?`, id)
}

// NormalizeRemote preserves repository path case and normalizes common Git transports.
func NormalizeRemote(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", nil
	}
	var host, path string
	if strings.Contains(raw, "://") {
		u, e := url.Parse(raw)
		if e != nil || u.Hostname() == "" {
			return "", errors.New("invalid git remote URL")
		}
		if u.RawQuery != "" || u.Fragment != "" {
			return "", errors.New("git remote must not contain query or fragment")
		}
		host = strings.ToLower(u.Hostname())
		if u.Port() != "" {
			host += ":" + u.Port()
		}
		path = u.Path
	} else {
		v := raw
		if i := strings.LastIndex(v, "@"); i >= 0 {
			v = v[i+1:]
		}
		if i := strings.Index(v, ":"); i > 0 {
			host = strings.ToLower(v[:i])
			path = v[i+1:]
		} else {
			if i = strings.Index(v, "/"); i > 0 {
				host = strings.ToLower(v[:i])
				path = v[i+1:]
			} else {
				return "", errors.New("git remote must include host and repository path")
			}
		}
	}
	path = strings.TrimSuffix(strings.TrimRight(path, "/"), ".git")
	path = strings.TrimLeft(path, "/")
	if path == "" || host == "" || strings.ContainsAny(host, " \t\n") || strings.ContainsAny(path, "\r\n") {
		return "", errors.New("invalid git remote")
	}
	return host + "/" + path, nil
}
func hashPayload(v any) string {
	h := sha256.Sum256([]byte(asJSON(v)))
	return hex.EncodeToString(h[:])
}

func cursor(a map[string]any) (int64, error) {
	if v, ok := a["after"]; ok {
		n, e := integer(v)
		if e != nil || n < 0 {
			return 0, errors.New("after must be a nonnegative integer sequence")
		}
		return int64(n), nil
	}
	return 0, nil
}
func page(key string, rows []map[string]any, n int, after int64) map[string]any {
	more := len(rows) > n
	if more {
		rows = rows[:n]
	}
	next := after
	if len(rows) > 0 {
		next = rows[len(rows)-1]["sequence"].(int64)
	}
	return map[string]any{key: rows, "next_cursor": next, "has_more": more}
}

const staleAfter = 15 * time.Minute

func decorateSession(m map[string]any) {
	raw, ok := m["last_seen"].(string)
	if !ok {
		return
	}
	status, ok := m["status"].(string)
	if !ok {
		return
	}
	presence := status
	stale := false
	if status == "active" {
		t, e := time.Parse(time.RFC3339Nano, raw)
		if e != nil || time.Since(t) > staleAfter {
			presence = "stale"
			stale = true
		}
	}
	m["presence"] = presence
	m["stale"] = stale
}
func (s *Store) touchReadActor(ctx context.Context, tx *transaction, pid string, r Request) error {
	instance := str(r.Args, "instance")
	if instance == "" && (!s.postgres || r.Session == "") {
		return nil
	}
	if r.Session == "" {
		return errors.New("session is required with instance")
	}
	match, args := sessionAddress(r.Session)
	actor, err := one(ctx, tx, `SELECT id FROM sessions WHERE project_id=? AND `+match+` AND instance=? AND owner_principal=?`, append(append([]any{pid}, args...), instance, s.principal)...)
	if err != nil {
		return scopedError(err, "session instance mismatch; pass the instance returned by register", "forbidden")
	}
	tx.actorID = actor["id"].(string)
	if tx.pure {
		// A heartbeat read: the session is verified here, read-only, and
		// its last_seen is recorded after the read.
		tx.beat = &heartbeat{tx.workspace, pid, tx.actorID}
		return nil
	}
	_, err = tx.ExecContext(ctx, `UPDATE sessions SET last_seen=? WHERE project_id=? AND id=?`, stamp(), pid, actor["id"])
	return err
}
