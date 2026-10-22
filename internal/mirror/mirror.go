// Package mirror atomically publishes scoped snapshots and opens them read-only
// for application reads. It is separate from the writable local authority.
package mirror

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"time"

	"clawbridge/internal/bus"
	_ "modernc.org/sqlite"
)

type Binding struct{ Server, Audience, Principal, Workspace, Project, Session, Instance string }
type Selection struct{ Workspace, Project, GitRemote string }
type Snapshot struct {
	Binding    Binding        `json:"binding"`
	Selections []Selection    `json:"selections"`
	UpdatedAt  string         `json:"updated_at"`
	Data       map[string]any `json:"data"`
}

func key(b Binding) string {
	raw, _ := json.Marshal(b)
	h := sha256.Sum256(raw)
	return hex.EncodeToString(h[:])
}
func decode(raw []byte) (Snapshot, error) {
	var s Snapshot
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	e := d.Decode(&s)
	return s, e
}

func open(path string, write bool) (*sql.DB, error) {
	if path == "" {
		return nil, errors.New("mirror cache path is required")
	}
	abs, e := filepath.Abs(path)
	if e != nil {
		return nil, e
	}
	if write {
		if e = os.MkdirAll(filepath.Dir(abs), 0700); e != nil {
			return nil, e
		}
		f, e := os.OpenFile(abs, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0600)
		if e != nil && !errors.Is(e, os.ErrExist) {
			return nil, e
		}
		if e == nil {
			if e = f.Close(); e != nil {
				return nil, e
			}
		}
	}
	u := url.URL{Scheme: "file", Path: bus.SQLiteURIPath(runtime.GOOS, abs)}
	query := url.Values{"_pragma": []string{"busy_timeout(3000)"}}
	if !write {
		query.Set("mode", "ro")
		query.Add("_pragma", "query_only(1)")
	}
	u.RawQuery = query.Encode()
	db, e := sql.Open("sqlite", u.String())
	if e != nil {
		return nil, e
	}
	db.SetMaxOpenConns(1)
	return db, nil
}

// Save is the only cache data writer. One transaction replaces one complete view;
// failed validation or SQL leaves the prior snapshot intact.
func Save(ctx context.Context, path string, s Snapshot) error {
	b := s.Binding
	if b.Server == "" || b.Audience == "" || b.Principal == "" || b.Workspace == "" || b.Project == "" || b.Session == "" || b.Instance == "" || s.Data == nil {
		return errors.New("incomplete mirror identity or scope")
	}
	if _, e := time.Parse(time.RFC3339Nano, s.UpdatedAt); e != nil {
		return errors.New("invalid snapshot timestamp")
	}
	raw, e := json.Marshal(s)
	if e != nil || len(raw) > 16<<20 {
		return errors.New("invalid or oversized snapshot")
	}
	db, e := open(path, true)
	if e != nil {
		return e
	}
	defer db.Close()
	tx, e := db.BeginTx(ctx, nil)
	if e != nil {
		return e
	}
	defer tx.Rollback()
	var version int
	if e = tx.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); e != nil {
		return e
	}
	if version == 0 {
		var tables int
		if e = tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name NOT LIKE 'sqlite_%'").Scan(&tables); e != nil {
			return e
		}
		if tables != 0 {
			return errors.New("refusing to replace unrelated database with a mirror")
		}
		if _, e = tx.ExecContext(ctx, "CREATE TABLE snapshots(key TEXT PRIMARY KEY,data BLOB NOT NULL); PRAGMA user_version=1"); e != nil {
			return e
		}
	} else if version != 1 {
		return errors.New("unsupported mirror schema version")
	}
	if _, e = tx.ExecContext(ctx, "INSERT INTO snapshots(key,data) VALUES(?,?) ON CONFLICT(key) DO UPDATE SET data=excluded.data", key(b), raw); e != nil {
		return e
	}
	return tx.Commit()
}

func snapshots(ctx context.Context, path string) ([]Snapshot, error) {
	db, e := open(path, false)
	if e != nil {
		return nil, errors.New("no readable mirror is available")
	}
	defer db.Close()
	var version int
	if e = db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); e != nil || version != 1 {
		return nil, errors.New("invalid mirror schema")
	}
	rows, e := db.QueryContext(ctx, "SELECT data FROM snapshots")
	if e != nil {
		return nil, errors.New("no complete mirror is available")
	}
	defer rows.Close()
	out := []Snapshot{}
	for rows.Next() {
		var raw []byte
		if e = rows.Scan(&raw); e != nil {
			return nil, e
		}
		if len(raw) > 16<<20 {
			return nil, errors.New("oversized stored snapshot")
		}
		s, e := decode(raw)
		if e != nil {
			return nil, errors.New("invalid stored snapshot")
		}
		out = append(out, s)
		if len(out) > 10000 {
			return nil, errors.New("mirror has too many bindings")
		}
	}
	return out, rows.Err()
}
func sameAuthority(a, b Binding) bool {
	return a.Server == b.Server && a.Audience == b.Audience && a.Principal == b.Principal && a.Principal != ""
}
func matchesSelection(s Snapshot, selection Selection) bool {
	if s.Binding.Workspace == selection.Workspace && s.Binding.Project == selection.Project && selection.Project != "" {
		return true
	}
	for _, a := range s.Selections {
		if a == selection {
			return true
		}
	}
	return false
}

// Resolve only finds a previously synced binding for this exact authority and
// selector. Conflicting historical mappings refuse instead of choosing a master.
func Resolve(ctx context.Context, path string, authority Binding, selection Selection) (string, string, error) {
	all, e := snapshots(ctx, path)
	if e != nil {
		return "", "", e
	}
	w, p := "", ""
	for _, s := range all {
		if sameAuthority(s.Binding, authority) && matchesSelection(s, selection) {
			if w != "" && (w != s.Binding.Workspace || p != s.Binding.Project) {
				return "", "", errors.New("ambiguous cached project resolution")
			}
			w, p = s.Binding.Workspace, s.Binding.Project
		}
	}
	if w == "" {
		return "", "", errors.New("no synced project matches this authority and selection")
	}
	return w, p, nil
}

func Load(ctx context.Context, path string, b Binding) (Snapshot, error) {
	all, e := snapshots(ctx, path)
	if e != nil {
		return Snapshot{}, e
	}
	var found *Snapshot
	for _, s := range all {
		if !sameAuthority(s.Binding, b) || (b.Session != "" && (s.Binding.Session != b.Session || b.Instance == "" || s.Binding.Instance != b.Instance)) || !matchesSelection(s, Selection{Workspace: b.Workspace, Project: b.Project}) {
			continue
		}
		if found != nil && (found.Binding.Workspace != s.Binding.Workspace || found.Binding.Project != s.Binding.Project) {
			return Snapshot{}, errors.New("ambiguous cached scope")
		}
		if found == nil || s.UpdatedAt > found.UpdatedAt {
			copy := s
			found = &copy
		}
	}
	if found == nil {
		return Snapshot{}, errors.New("no complete mirror matches this principal, scope, session and instance")
	}
	return *found, nil
}

func Offline(ctx context.Context, path string, b Binding, r bus.Request) (bus.Result, error) {
	if !Readable(r.Operation) {
		return bus.Result{}, errors.New("cloud write unavailable; no write queued; tell the human and use an authorized fallback")
	}
	if (r.Operation == "inbox" || r.Operation == "message_status") && (b.Session == "" || b.Instance == "") {
		return bus.Result{}, errors.New("offline inbox/status requires a bound session and instance")
	}
	s, e := Load(ctx, path, b)
	if e != nil {
		return bus.Result{}, e
	}
	a := r.Args
	if a == nil {
		a = map[string]any{}
	}
	allowed := map[string]map[string]bool{
		"list_sessions": {"instance": true, "status": true, "role": true, "limit": true, "after": true},
		"inbox":         {"instance": true, "unread_only": true, "limit": true, "after": true},
		"history":       {"instance": true, "entity": true, "key": true, "limit": true, "after": true},
		"whoami":        {"instance": true}, "message_status": {"instance": true, "message_id": true},
	}
	for k := range a {
		if !allowed[r.Operation][k] {
			return bus.Result{}, errors.New("unsupported offline operation argument")
		}
		switch k {
		case "limit", "after":
			if _, e := number(a[k]); e != nil {
				return bus.Result{}, e
			}
		case "unread_only":
			if _, ok := a[k].(bool); !ok {
				return bus.Result{}, errors.New("unread_only must be boolean")
			}
		default:
			if _, ok := a[k].(string); !ok {
				return bus.Result{}, errors.New("offline filter must be a string")
			}
		}
	}
	if v, ok := a["instance"]; ok && v != b.Instance {
		return bus.Result{}, errors.New("mirror session instance mismatch")
	}
	var data map[string]any
	switch r.Operation {
	case "whoami":
		data = map[string]any{"workspace": s.Data["workspace"], "project": s.Data["project"], "session": s.Data["session"], "principal": s.Binding.Principal, "verified": false, "identity_provenance": "cached-google-id-token", "storage_mode": "offline-mirror", "online": false}
		if b.Session == "" {
			data["session"] = nil
		}
	case "message_status":
		id, ok := a["message_id"].(string)
		if !ok || id == "" {
			return bus.Result{}, errors.New("message_id is required")
		}
		for _, row := range records(s.Data["sent_messages"]) {
			if row["id"] == id {
				data = row
				break
			}
		}
		if data == nil {
			return bus.Result{}, errors.New("sent message is not in this session's mirror")
		}
		expiry, e := time.Parse(time.RFC3339Nano, text(data, "expires_at"))
		if e != nil || !expiry.After(time.Now()) {
			return bus.Result{}, errors.New("sent message status is expired or unavailable in the mirror")
		}
	default:
		limit, after, e := pageArgs(a)
		if e != nil {
			return bus.Result{}, e
		}
		key := "sessions"
		if r.Operation == "inbox" {
			key = "messages"
		}
		if r.Operation == "history" {
			key = "events"
		}
		rows := records(s.Data[key])
		filtered := []map[string]any{}
		unread := true
		if status := text(a, "status"); status != "" && status != "active" && status != "retired" && status != "moved" {
			return bus.Result{}, errors.New("invalid session status")
		}
		if raw, ok := a["unread_only"]; ok {
			var valid bool
			unread, valid = raw.(bool)
			if !valid {
				return bus.Result{}, errors.New("unread_only must be boolean")
			}
		}
		for _, row := range rows {
			n, _ := number(row["sequence"])
			if n <= after {
				continue
			}
			if key == "sessions" {
				if v := text(a, "status"); v != "" && row["status"] != v {
					continue
				}
				if v := text(a, "role"); v != "" && row["role"] != v {
					continue
				}
			}
			if key == "events" {
				if v := text(a, "entity"); v != "" && row["entity"] != v {
					continue
				}
				if v := text(a, "key"); v != "" && row["entity_key"] != v {
					continue
				}
			}
			if key == "messages" {
				expiry, e := time.Parse(time.RFC3339Nano, text(row, "expires_at"))
				if e != nil || !expiry.After(time.Now()) {
					continue
				}
				if unread && row["read_at"] != nil && row["read_at"] != "" {
					continue
				}
			}
			filtered = append(filtered, row)
		}
		sort.Slice(filtered, func(i, j int) bool {
			x, _ := number(filtered[i]["sequence"])
			y, _ := number(filtered[j]["sequence"])
			return x < y
		})
		more := len(filtered) > limit
		if more {
			filtered = filtered[:limit]
		}
		next := after
		if len(filtered) > 0 {
			next, _ = number(filtered[len(filtered)-1]["sequence"])
		}
		data = map[string]any{key: filtered, "next_cursor": next, "has_more": more}
		// The hosted snapshot holds only recent history and retired sessions;
		// say so, so a missing older row is not read as absent.
		switch key {
		case "events":
			data["truncated"] = s.Data["events_truncated"] == true
		case "sessions":
			data["truncated"] = s.Data["sessions_truncated"] == true
		}
	}
	updated, e := time.Parse(time.RFC3339Nano, s.UpdatedAt)
	if e != nil {
		return bus.Result{}, errors.New("invalid mirror timestamp")
	}
	age := int64(time.Since(updated).Seconds())
	if age < 0 {
		age = 0
	}
	data["mirror"] = map[string]any{"updated_at": s.UpdatedAt, "age_seconds": age, "stale": true}
	return bus.Result{Source: "offline-mirror", Data: data}, nil
}
func Readable(op string) bool {
	switch op {
	case "list_sessions", "inbox", "history", "whoami", "message_status":
		return true
	}
	return false
}
func text(m map[string]any, k string) string { s, _ := m[k].(string); return s }
func records(v any) []map[string]any {
	out := []map[string]any{}
	if a, ok := v.([]any); ok {
		for _, x := range a {
			if m, ok := x.(map[string]any); ok {
				out = append(out, m)
			}
		}
	}
	return out
}
func number(v any) (int64, error) {
	switch n := v.(type) {
	case json.Number:
		return n.Int64()
	case int:
		return int64(n), nil
	case int64:
		return n, nil
	case float64:
		if n == float64(int64(n)) {
			return int64(n), nil
		}
	}
	return 0, errors.New("expected an integer")
}
func pageArgs(a map[string]any) (int, int64, error) {
	n, after := int64(100), int64(0)
	var e error
	if v, ok := a["limit"]; ok {
		n, e = number(v)
		if e != nil {
			return 0, 0, e
		}
	}
	if v, ok := a["after"]; ok {
		after, e = number(v)
		if e != nil {
			return 0, 0, e
		}
	}
	if n < 1 || n > 1000 || after < 0 {
		return 0, 0, fmt.Errorf("limit must be 1–1000 and after nonnegative")
	}
	return int(n), after, nil
}
