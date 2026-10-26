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
	"sync/atomic"
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
	if version > 20 {
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
	if version < 7 {
		if _, err = tx.Exec(commentSchemaV7 + `PRAGMA user_version=7;`); err != nil {
			return err
		}
	}
	if version < 8 {
		if _, err = tx.Exec(labelPolicySchemaV8 + `PRAGMA user_version=8;`); err != nil {
			return err
		}
	}
	if version < 9 {
		if _, err = tx.Exec(appIdentitySchemaV9 + `PRAGMA user_version=9;`); err != nil {
			return err
		}
	}
	if version < 10 {
		if _, err = tx.Exec(writeLedgerSchemaV10 + `PRAGMA user_version=10;`); err != nil {
			return err
		}
	}
	if version < 11 {
		if _, err = tx.Exec(markerEngineerSchemaV11 + `PRAGMA user_version=11;`); err != nil {
			return err
		}
	}
	if version < 12 {
		if _, err = tx.Exec(intakeSchemaV12 + `PRAGMA user_version=12;`); err != nil {
			return err
		}
	}
	if version < 13 {
		if _, err = tx.Exec(docSeedSchemaV13 + `PRAGMA user_version=13;`); err != nil {
			return err
		}
	}
	if version < 14 {
		if _, err = tx.Exec(statusPolicySchemaV14 + `PRAGMA user_version=14;`); err != nil {
			return err
		}
	}
	if version < 15 {
		if _, err = tx.Exec(healthSchemaV15 + `PRAGMA user_version=15;`); err != nil {
			return err
		}
	}
	if version < 16 {
		if _, err = tx.Exec(docPolicySchemaV16 + `PRAGMA user_version=16;`); err != nil {
			return err
		}
	}
	if version < 17 {
		if _, err = tx.Exec(dispatcherSchemaV17 + `PRAGMA user_version=17;`); err != nil {
			return err
		}
	}
	if version < 18 {
		if _, err = tx.Exec(authSchemaV18 + `PRAGMA user_version=18;`); err != nil {
			return err
		}
	}
	if version < 19 {
		if _, err = tx.Exec(healthPostSchemaV19 + `PRAGMA user_version=19;`); err != nil {
			return err
		}
	}
	if version < 20 {
		if _, err = tx.Exec(presenceSchemaV20 + `PRAGMA user_version=20;`); err != nil {
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

// stringList reads a string array as decoded from JSON ([]any) or built in Go
// ([]string). isArray is false for any other value; ok is false when an
// element is not a string. A nil array stays nil and an empty one is empty.
func stringList(v any) (list []string, isArray, ok bool) {
	switch items := v.(type) {
	case []string:
		if items == nil {
			return nil, true, true
		}
		return append([]string{}, items...), true, true
	case []any:
		if items == nil {
			return nil, true, true
		}
		list = make([]string, 0, len(items))
		for _, item := range items {
			s, isString := item.(string)
			if !isString {
				return nil, true, false
			}
			list = append(list, s)
		}
		return list, true, true
	}
	return nil, false, false
}

// hostClock is this host's clock; a test skews it to prove a hosted binding
// time comes from the database instead.
var hostClock = time.Now

func stamp() string                         { return hostClock().UTC().Format(timestampLayout) }
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
	if reservedDispatcherName(v) {
		return errors.New("the session names clawbridge-dispatcher and clawbridge-dispatcher-model are reserved for the dispatcher")
	}
	if strings.EqualFold(v, SystemSessionName) {
		return errors.New("the session name clawbridge is reserved for the server's watcher")
	}
	return nil
}

// sessionAddress returns the SQL predicate and arguments that resolve a
// session address: an exact ID for ID-shaped addresses, otherwise ID or name.
// No address ever resolves the system session.
func sessionAddress(v string) (string, []any) {
	if sessionIDShaped(v) {
		return "id=? AND role<>'system'", []any{v}
	}
	return "(id=? OR name=?) AND role<>'system'", []any{v, v}
}

// operationArgs names every operation and the argument keys it accepts.
var operationArgs = map[string]string{
	"taskboard_get":               "instance",
	"taskboard_get_v2":            "instance",
	"comment_watch_status":        "",
	"taskboard_set_v2":            "instance expected_revision mode linear_team_ids linear_default_team_id linear_project_ids linear_workspace_id",
	"taskboard_clear_v2":          "instance expected_revision repair",
	"taskboard_access":            "instance ledger engineer",
	"member_display_name_set":     "display_name if_unset",
	"taskboard_set":               "instance expected_revision linear_team_ids linear_default_team_id linear_project_id linear_workspace_id",
	"taskboard_clear":             "instance expected_revision repair",
	"inbox_status":                "instance after",
	"notification_status":         "instance after",
	"set_notification_policy":     "instance policy",
	"assignment_propose":          "instance issue_uuid issue_identifier task_role scope_repo scope_paths issue_project_id issue_labels",
	"assignment_confirm":          "instance target_session revision allow_overlap",
	"assignment_release":          "instance target_session revision outcome",
	"coordinator_set":             "instance target_session",
	"linear_comment_reserve":      "instance issue_uuid operation_id body_sha256 parent_id in_response_to",
	"linear_comment_precheck":     "instance issue_uuid operation_id in_response_to",
	"linear_comment_settle":       "instance operation_id outcome comment_id via",
	"linear_comments_posted":      "instance issue_uuid comment_ids",
	"linear_comment_settle_match": "instance lease_token generation operation_digest issue_uuid comment_id created_at",
	"comment_watch_lease":         "instance action lease_token generation budget",
	"comment_watch_due":           "instance lease_token generation",
	"comment_watch_report":        "instance lease_token generation issue_uuid calls error comments",
	"marker_go":                   "instance lease_token generation assignment_id phase part",
	"marker_settle":               "instance lease_token generation assignment_id phase part outcome comment_id created_at via error state_target state_landed skip_reason",
	"marker_due":                  "instance lease_token generation",
	"marker_spent":                "instance lease_token generation assignment_id phase calls charged",
	"label_policy_get":            "instance",
	"status_policy_get":           "instance",
	"status_policy_set":           "instance expected_revision status_policy",
	"doc_policy_get":              "instance",
	"dispatcher_get":              "instance",
	"dispatcher_cases_due":        "instance",
	"dispatcher_decision_record":  "instance subject_id subject_revision policy_revision engine proposal reason_code explanation guardrail action domain facts_source app_project_id",
	"invite_create":               "email grants purpose expires_hours revoke_others",
	"invite_list":                 "",
	"invite_revoke":               "invite_id",
	"device_list":                 "principal",
	"device_revoke":               "device_id reason",
	"device_label":                "device_id label",
	"device_enroll":               "public_key label key_store",
	"device_ci_create":            "name grants expires_days public_key label",
	"user_list":                   "",
	"user_revoke":                 "principal",
	"owner_transfer_offer":        "to",
	"owner_transfer_respond":      "action",
	"owner_transfer_get":          "",
	"dispatcher_domains_set":      "instance expected_revision dispatcher_domains",
	"dispatcher_domain_edit":      "instance expected_revision domain action target_session",
	"dispatcher_enable":           "instance expected_revision mode confirm",
	"dispatcher_disable":          "instance expected_revision",
	"doc_watch_due":               "instance lease_token generation",
	"doc_watch_report":            "instance lease_token generation polled_at documents complete error",
	"doc_verify_report":           "instance lease_token generation document_id content_sha256 error",
	"doc_publication_get":         "instance parent_kind parent_id source_path",
	"doc_publication_put":         "instance parent_kind parent_id source_path part_number part_count generation state publish_token expect_token document_id published_sha256 source_commit source_sha256",
	"doc_publish_override":        "instance parent_kind parent_id source_path operation_id pattern_ids lines source_commit",
	"doc_policy_set":              "instance expected_revision doc_policy",
	"health_get":                  "instance",
	"health_set":                  "instance linear_project expected_revision health_policy",
	"health_due":                  "instance lease_token generation limit",
	"health_report":               "instance lease_token generation linear_project calls error facts",
	"health_post_settle":          "instance lease_token generation linear_project day outcome update_id error",
	"doc_seed_get":                "instance parent_kind parent_id",
	"doc_seed_put":                "instance parent_kind parent_id template template_version state seed_token expect_token document_id seeded_sha256",
	"label_policy_set":            "instance expected_revision label_policy",
	"linear_admin_audit":          "instance kind outcome operation_id linear_team_id linear_id linear_project_id name previous_name policy_revision",
	"watch_charge":                "instance lease_token generation calls",
	"intake_policy_get":           "instance",
	"intake_policy_set":           "instance expected_revision mode signals roles s5_calls_per_hour out_of_binding_limit",
	"intake_trust_add":            "instance linear_user_id",
	"intake_trust_remove":         "instance linear_user_id",
	"intake_trust_list":           "instance",
	"intake_token_create":         "instance expires_days",
	"intake_token_list":           "instance",
	"intake_token_revoke":         "instance token_id",
	"intake_list":                 "instance state",
	"intake_dismiss":              "instance issue_uuid",
	"intake_rearm":                "instance issue_uuid",
	"workspace_init":              "mode name retention_days", "project_create": "slug name git_remote", "list_projects": "", "list_members": "", "register_session": "instance resume role agent_kind model host account notes transport", "list_sessions": "instance status role limit after issue scope_path assignment_state", "send": "instance to body kind thread_id operation_id coordination", "reply": "instance message_id body kind operation_id coordination", "inbox": "instance unread_only limit after", "mark_read": "instance ids", "update_session": "instance target_session role model host account notes transport", "retire_session": "instance target_session successor status", "history": "instance entity key limit after", "whoami": "instance", "heartbeat": "instance state", "message_status": "instance message_id", "resolve_project": "git_remote instance", "snapshot": "instance", "project_role_set": "instance principal role", "project_role_revoke": "instance principal", "purge_expired": "instance", "principal_unbind": "principal allow_previous", "workspace_set_domains": "domains",
}

func validateArgs(op string, a map[string]any) error {
	keys, ok := operationArgs[op]
	if !ok {
		return fmt.Errorf("unsupported operation %q", op)
	}
	for k, v := range a {
		if !strings.Contains(" "+keys+" ", " "+k+" ") {
			return fmt.Errorf("unsupported argument %q for %s", k, op)
		}
		switch k {
		case "retention_days", "limit", "after", "expected_revision", "ledger", "generation", "budget", "calls", "charged", "policy_revision", "engineer", "if_unset", "expires_days", "expires_hours", "s5_calls_per_hour", "out_of_binding_limit", "template_version", "part_number", "part_count":
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
		case "label_policy":
			if _, err := ParseLabelPolicy(v); err != nil {
				return err
			}
		case "status_policy":
			if _, err := ParseStatusPolicy(v); err != nil {
				return err
			}
		case "doc_policy":
			if _, err := ParseDocPolicy(v); err != nil {
				return err
			}
		case "dispatcher_domains":
			if _, err := ParseDispatcherDomains(v); err != nil {
				return err
			}
		case "health_policy":
			if _, err := ParseHealthPolicy(v); err != nil {
				return err
			}
		case "facts":
			if _, ok := v.(map[string]any); !ok {
				return errors.New("facts must be an object")
			}
		case "grants":
			if _, ok := v.(map[string]any); !ok {
				return errors.New("grants must be an object of project to role")
			}
		case "revoke_others":
			if _, ok := v.(bool); !ok {
				return errors.New("revoke_others must be boolean")
			}
		case "transport":
			if _, ok := v.(map[string]any); !ok {
				return errors.New("transport must be an object")
			}
		case "documents":
			list, ok := v.([]any)
			if !ok {
				return errors.New("documents must be an array of objects")
			}
			for _, item := range list {
				if _, ok := item.(map[string]any); !ok {
					return errors.New("documents must be an array of objects")
				}
			}
		case "complete":
			if _, ok := v.(bool); !ok {
				return errors.New("complete must be boolean")
			}
		case "comments":
			if _, ok := v.([]any); !ok {
				return errors.New("comments must be an array of objects")
			}
		case "lines":
			if _, ok := intList(v, 1000); !ok {
				return errors.New("lines must be an array of line numbers")
			}
		case "ids", "linear_team_ids", "linear_project_ids", "scope_paths", "domains", "comment_ids", "signals", "roles", "pattern_ids", "issue_labels":
			switch v.(type) {
			case []string, []any:
			default:
				return errors.New("ids must be an array of strings")
			}
		case "unread_only", "resume", "allow_overlap", "allow_previous", "repair", "confirm":
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
	case "snapshot", "inbox_status", "notification_status", "taskboard_get", "taskboard_get_v2", "taskboard_access", "list_projects", "list_members", "comment_watch_status", "label_policy_get", "status_policy_get", "doc_policy_get", "dispatcher_get", "health_get", "doc_seed_get", "doc_publication_get", "invite_list", "device_list", "user_list", "owner_transfer_get":
		return true
	case "list_sessions", "history", "whoami", "linear_comments_posted", "linear_comment_precheck", "intake_policy_get", "intake_trust_list", "intake_list":
		// Naming a session with its instance makes it a heartbeat read: the
		// read itself is pure and the heartbeat follows (see beat).
		return (r.Session == "") == (str(r.Args, "instance") == "")
	case "resolve_project":
		return r.Session == "" && str(r.Args, "instance") == ""
	case "heartbeat":
		// The lease is written after the read, on the beat's light path.
		return true
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
// beforeBeat, when set by a test, runs after a heartbeat read commits and
// before its beat, where another request's change can land. It is atomic
// because concurrent requests read it.
var beforeBeat atomic.Pointer[func()]

func (s *Store) beat(ctx context.Context, r Request, b *heartbeat) (string, bool) {
	now := stamp()
	return now, s.sessionWrite(ctx, r.Operation, b.workspace, `UPDATE sessions SET last_seen=? FROM workspaces w WHERE sessions.project_id=? AND sessions.id=? AND sessions.status='active'`, []any{now, b.project, b.session})
}

// sessionWrite runs one light session update after a read has committed:
// a beat or a lease. update is an UPDATE sessions … FROM workspaces w WHERE
// …; the access checks are appended to it.
func (s *Store) sessionWrite(ctx context.Context, operation, workspace, update string, args []any) bool {
	if hook := beforeBeat.Load(); hook != nil {
		(*hook)()
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	raw, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		logHeartbeatSkipped(operation, err)
		return false
	}
	tx := &transaction{Tx: raw, postgres: true}
	defer tx.Rollback()
	var acquired bool
	now := stamp()
	if _, err = tx.ExecContext(ctx, `SET LOCAL lock_timeout='1s'`); err == nil {
		err = tx.QueryRowContext(ctx, `SELECT pg_try_advisory_xact_lock(hashtextextended(?,0))`, "workspace:"+workspace).Scan(&acquired)
	}
	if err == nil && acquired {
		// Like any write, the update re-checks access under the workspace
		// lock, in the same statement: it updates only an active session
		// of a current member, and returns the allow-list to check before
		// commit. A session retired, a membership revoked or an allow-list
		// narrowed while the read ran skips the update silently.
		// A device identity's device must still be live and of this
		// workspace too.
		q := update + ` AND w.id=? AND EXISTS(SELECT 1 FROM members m WHERE m.workspace_id=w.id AND m.principal=?)`
		args = append(args, workspace, s.principal)
		if s.identity != nil && s.identity.Issuer == DeviceIssuer {
			q += ` AND EXISTS(SELECT 1 FROM devices d WHERE d.device_id=? AND d.workspace_id=w.id AND d.principal=? AND d.revoked_at IS NULL AND (d.expires_at IS NULL OR d.expires_at>?))`
			args = append(args, s.identity.Subject, s.principal, now)
		}
		var domains string
		err = tx.QueryRowContext(ctx, q+` RETURNING w.allowed_domains`, args...).Scan(&domains)
		if errors.Is(err, sql.ErrNoRows) || (err == nil && s.domainGate(domains) != nil) {
			return false
		}
		if err == nil {
			err = tx.Commit()
		}
	}
	if err != nil {
		logHeartbeatSkipped(operation, err)
	}
	return err == nil && acquired
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
	if tx.lease != nil {
		data.(map[string]any)["renewed"] = s.renewLease(ctx, r, tx.lease)
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
			// A device signs only in its own workspace, so never makes one.
			err = s.requireBound(ctx, tx, "")
		}
		return out, err
	}
	if r.Operation == "resolve_project" {
		out, err := s.resolveInTransaction(ctx, tx, r)
		if err == nil {
			err = s.requireBound(ctx, tx, out.(map[string]any)["workspace_id"].(string))
		}
		return out, err
	}
	if r.Operation == "principal_unbind" {
		return s.unbindPrincipal(ctx, tx, a)
	}
	if !s.postgres && (r.Operation == "snapshot" || r.Operation == "project_role_set" || r.Operation == "project_role_revoke" || r.Operation == "purge_expired" || r.Operation == "workspace_set_domains" || r.Operation == "member_display_name_set" || authOperation(r.Operation)) {
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
		}
		switch {
		case r.Operation == "workspace_set_domains" && w["owner_principal"] == s.principal:
			// The workspace owner may always repair or clear the allow-list.
		case tx.pure:
			if err = s.domainGate(w["allowed_domains"]); err != nil {
				return nil, err
			}
		default:
			// One rule for every write (here, resolve and the heartbeat):
			// membership and the allow-list are read again under the
			// workspace lock the setter holds, so a write never commits after
			// a restriction it predates.
			if err = s.admittedUnderLock(ctx, tx, wid); err != nil {
				return nil, err
			}
		}
		// A member's account must be the one bound to the principal; a
		// non-member or a refused domain was refused above and writes no
		// binding.
		if err = s.requireBound(ctx, tx, wid); err != nil {
			return nil, err
		}
		if r.Operation == "workspace_set_domains" {
			return s.setWorkspaceDomains(ctx, tx, w, a)
		}
		if r.Operation == "member_display_name_set" {
			return s.setDisplayName(ctx, tx, wid, a)
		}
		if authOperation(r.Operation) {
			return s.authOp(ctx, tx, w, r)
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
	if r.Operation == "heartbeat" {
		return s.heartbeatOp(ctx, tx, pid, r)
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
	if r.Operation == "comment_watch_status" {
		return watchStatus(ctx, tx, pid)
	}
	// Any project member may read the approved label list, with or
	// without a session; setting it is the owner's (labelPolicyOp).
	if r.Operation == "label_policy_get" {
		if r.Session == "" && str(a, "instance") != "" {
			return nil, errors.New("instance requires session")
		}
		return readLabelPolicy(ctx, tx, pid)
	}
	// Any project member may read the status policy too; every create path
	// reads it, so an issue lands in the same state whichever route it takes.
	if r.Operation == "status_policy_get" {
		if r.Session == "" && str(a, "instance") != "" {
			return nil, errors.New("instance requires session")
		}
		return statusPolicyView(ctx, tx, pid)
	}
	// Any project member may read the dispatcher's policy and domains.
	if r.Operation == "dispatcher_get" {
		if r.Session == "" && str(a, "instance") != "" {
			return nil, errors.New("instance requires session")
		}
		return dispatcherView(ctx, tx, pid)
	}
	// Any project member may read the document policy, its deny patterns
	// by ID only unless they may publish (docPolicyView).
	if r.Operation == "doc_policy_get" {
		if r.Session == "" && str(a, "instance") != "" {
			return nil, errors.New("instance requires session")
		}
		return docPolicyView(ctx, tx, pid, projectRole == "owner" || projectRole == "lead")
	}
	// Any project member may read project health: it is the fallback's
	// view (design §6).
	if r.Operation == "health_get" {
		if r.Session == "" && str(a, "instance") != "" {
			return nil, errors.New("instance requires session")
		}
		return healthRows(ctx, tx, pid)
	}
	if r.Operation == "taskboard_access" || r.Operation == "taskboard_get" || r.Operation == "taskboard_set" || r.Operation == "taskboard_clear" || r.Operation == "taskboard_get_v2" || r.Operation == "taskboard_set_v2" || r.Operation == "taskboard_clear_v2" {
		if r.Session == "" && str(a, "instance") != "" {
			return nil, errors.New("instance requires session")
		}
		if r.Operation == "taskboard_access" {
			// Only a client that records its comments in the ledger may
			// write to Linear: an older one would post comments the
			// watcher takes for a human's.
			if n, e := integer(a["ledger"]); e != nil || n != 1 {
				return nil, invalid(upgradeRequired)
			}
			access := TaskboardAccess{WorkspaceID: wid, ProjectID: pid, ProjectRole: projectRole, CanWrite: projectRole != "reader"}
			if actor != nil {
				access.SessionID = actor["id"].(string)
				access.SessionName = actor["name"].(string)
				access.CanWrite = access.CanWrite && actor["status"] == "active"
			}
			out := accessMap(access)
			// A client that asks gets the engineer behind the session, from
			// the authority's records; an older client never sees the field.
			if n, e := integer(a["engineer"]); e == nil && n == 1 && actor != nil {
				engineer, e := s.engineerOf(ctx, tx, wid, actor)
				if e != nil {
					return nil, e
				}
				if engineer != "" {
					out["engineer"] = engineer
				}
			}
			return out, nil
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
		return s.taskboard(ctx, tx, wid, pid, projectRole, r.Operation, a)
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
			provenance, mode = s.provenance(), "cloud"
		}
		if actor != nil {
			live, e := liveAssignment(ctx, tx, pid, actor["id"].(string))
			if e != nil {
				return nil, e
			}
			actor["assignment"] = assignmentView(live, actor)
		}
		out := map[string]any{"workspace": shownWorkspace(w, w["owner_principal"] == s.principal), "project": p, "session": actor, "principal": s.principal, "identity_provenance": provenance, "verified": s.postgres, "storage_mode": mode, "online": true}
		if s.postgres && s.identity != nil && s.identity.Issuer == DeviceIssuer {
			device, e := one(ctx, tx, `SELECT * FROM devices WHERE device_id=?`, s.identity.Subject)
			if e != nil {
				return nil, e
			}
			view := deviceView(device, stamp())
			out["device"] = map[string]any{"device_id": view["device_id"], "label": view["label"], "kind": view["kind"], "key_store": view["key_store"], "expires_at": view["expires_at"], "status": view["status"]}
		}
		return out, nil
	}
	// The owner's admin commands are CLI-only and may run without a session:
	// the Linear write is authorized sessionless, so its audit and the
	// approved list it is checked against must be too.
	if actor == nil && labelPolicyOperation(r.Operation) {
		if str(a, "instance") != "" {
			return nil, invalid("instance requires session")
		}
		return s.labelPolicyOp(ctx, tx, w, p, projectRole, nil, r)
	}
	if actor == nil && statusPolicyOperation(r.Operation) {
		if str(a, "instance") != "" {
			return nil, invalid("instance requires session")
		}
		return s.statusPolicyOp(ctx, tx, w, p, projectRole, nil, r)
	}
	if dispatcherSwitchOperation(r.Operation) {
		if actor != nil || str(a, "instance") != "" {
			return nil, errSwitchFromSession
		}
		return s.dispatcherSwitch(ctx, tx, w, p, projectRole, r)
	}
	if actor == nil && dispatcherOperation(r.Operation) {
		if str(a, "instance") != "" {
			return nil, invalid("instance requires session")
		}
		return s.dispatcherOp(ctx, tx, w, p, projectRole, nil, r)
	}
	if actor == nil && docPolicyOperation(r.Operation) {
		if str(a, "instance") != "" {
			return nil, invalid("instance requires session")
		}
		return s.docPolicyOp(ctx, tx, w, p, projectRole, nil, r)
	}
	if actor == nil && healthOperation(r.Operation) {
		if str(a, "instance") != "" {
			return nil, invalid("instance requires session")
		}
		return s.healthOp(ctx, tx, w, p, projectRole, nil, r)
	}
	// Publishing documents is a lead's or owner's CLI command, as seeding.
	if docPublicationOperation(r.Operation) {
		if actor == nil && str(a, "instance") != "" {
			return nil, invalid("instance requires session")
		}
		if actor != nil {
			tx.actorID = actor["id"].(string)
		}
		return s.docPublicationOp(ctx, tx, w, p, projectRole, actor, r)
	}
	// Seeding guide documents is a lead's or owner's CLI command too.
	if docSeedOperation(r.Operation) {
		if actor == nil && str(a, "instance") != "" {
			return nil, invalid("instance requires session")
		}
		if actor != nil {
			tx.actorID = actor["id"].(string)
		}
		return s.docSeedOp(ctx, tx, w, p, projectRole, actor, r)
	}
	if actor == nil {
		return nil, invalid("session is required; register and pass --session and --instance")
	}
	sid := actor["id"].(string)
	tx.actorID = sid
	if assignmentOperation(r.Operation) {
		return s.assignment(ctx, tx, w, p, projectRole, actor, r)
	}
	if commentOperation(r.Operation) {
		return s.commentOp(ctx, tx, w, p, projectRole, actor, r)
	}
	if labelPolicyOperation(r.Operation) {
		return s.labelPolicyOp(ctx, tx, w, p, projectRole, actor, r)
	}
	if statusPolicyOperation(r.Operation) {
		return s.statusPolicyOp(ctx, tx, w, p, projectRole, actor, r)
	}
	if docPolicyOperation(r.Operation) {
		return s.docPolicyOp(ctx, tx, w, p, projectRole, actor, r)
	}
	if dispatcherOperation(r.Operation) {
		return s.dispatcherOp(ctx, tx, w, p, projectRole, actor, r)
	}
	if healthOperation(r.Operation) {
		return s.healthOp(ctx, tx, w, p, projectRole, actor, r)
	}
	if intakeOperation(r.Operation) {
		return s.intakeOp(ctx, tx, w, p, projectRole, actor, r)
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
	if _, leased := m["alive_until"]; leased {
		// The dispatcher's presence (§1); a domain's own stale_after_hours
		// replaces the default where one applies.
		m["liveness"] = liveness(m, hostClock().UTC(), defaultStaleAfterHour)
	}
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
	// Like a heartbeat, only an active session's last_seen moves.
	_, err = tx.ExecContext(ctx, `UPDATE sessions SET last_seen=? WHERE project_id=? AND id=? AND status='active'`, stamp(), pid, actor["id"])
	return err
}
