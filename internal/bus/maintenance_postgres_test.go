package bus

import (
	"context"
	"database/sql"
	"errors"
	"net/url"
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"
)

// maintenanceReadOps is every operation pureRead accepts, pinned (D6): a
// new one must be added here, and so pass the audit below.
var maintenanceReadOps = []string{
	"comment_watch_status", "context_watch_get", "device_events", "device_list", "dispatcher_chat_show", "dispatcher_decisions_list",
	"dispatcher_escalations_list", "dispatcher_get", "dispatcher_model_doctor", "doc_policy_get", "doc_publication_get", "doc_seed_get",
	"health_get", "heartbeat", "history", "inbox_status", "intake_list", "intake_policy_get", "intake_trust_list", "invite_list",
	"label_policy_get", "lane_pins_get", "linear_comment_precheck", "linear_comments_posted", "linear_view_get", "linear_views_list", "list_members",
	"list_projects", "list_sessions", "notification_status", "owner_transfer_get", "project_kind_get", "project_overview",
	"project_team_add_list", "resolve_project", "snapshot", "status_policy_get", "taskboard_access", "taskboard_get", "taskboard_get_v2", "team_roles_get", "user_list", "whoami",
}

// The pinned list is exactly pureRead's.
func TestMaintenanceReadOpsArePureRead(t *testing.T) {
	src, err := os.ReadFile("store.go")
	if err != nil {
		t.Fatal(err)
	}
	start := strings.Index(string(src), "\nfunc pureRead(")
	end := strings.Index(string(src)[start+1:], "\n}\n")
	body := string(src)[start : start+1+end]
	got := []string{}
	for _, m := range regexp.MustCompile(`"([a-z0-9_]+)"`).FindAllStringSubmatch(body, -1) {
		if m[1] != "instance" && !slices.Contains(got, m[1]) {
			got = append(got, m[1])
		}
	}
	slices.Sort(got)
	want := slices.Clone(maintenanceReadOps)
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Fatalf("pureRead's operations changed; audit the new ones in maintenance and pin them:\n got %v\nwant %v", got, want)
	}
	for _, op := range maintenanceReadOps {
		r := Request{Operation: op, Session: "me", Args: map[string]any{"instance": "i"}}
		if op == "resolve_project" {
			r = Request{Operation: op}
		}
		if !ServedInMaintenance(r) {
			t.Fatal(op, "isn't served in maintenance")
		}
	}
	for _, op := range []string{"send", "reply", "ack", "nack", "lease", "extend", "mark_read", "inbox", "stream_ticket", "register_session", "linear_write", "comment_reserve", "assignment_confirm"} {
		if ServedInMaintenance(Request{Operation: op, Session: "me", Args: map[string]any{"instance": "i"}}) {
			t.Fatal(op, "is served in maintenance")
		}
	}
}

// D6: every pure read, with and without its session, runs in maintenance
// against a database whose every transaction is read-only: none tries a
// write (a misclassified read), none records a heartbeat or renews a lease
// after it, and none fails as the read-only database's unavailable.
func TestPostgresPureReadsWriteNothingInMaintenance(t *testing.T) {
	f := newReadLockFixture(t)
	misread, skipped := []string{}, []string{}
	oldMis, oldBeat := logMisclassifiedRead, logHeartbeatSkipped
	logMisclassifiedRead = func(op string) { misread = append(misread, op) }
	logHeartbeatSkipped = func(op string, err error) { skipped = append(skipped, op) }
	t.Cleanup(func() { logMisclassifiedRead, logHeartbeatSkipped = oldMis, oldBeat })
	dsn := os.Getenv("CLAWBRIDGE_TEST_POSTGRES_DSN")
	sep := "?"
	if strings.Contains(dsn, "?") {
		sep = "&"
	}
	roDB, err := sql.Open("pgx", dsn+sep+"default_transaction_read_only=on")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { roDB.Close() })
	ro := &Store{db: roDB, postgres: true, bootstrapOwner: f.owner}
	ro.SetQuiet(func() bool { return true })
	before := f.lastSeen("me")
	ctx := context.Background()
	for _, op := range maintenanceReadOps {
		for _, session := range []string{"", "me"} {
			a := map[string]any{}
			if session != "" {
				a["instance"] = session + "-inst"
			}
			r := Request{Operation: op, Workspace: f.w, Project: "p", Session: session, Args: a}
			if !pureRead(r) {
				continue
			}
			out, e := ro.ExecuteAs(ctx, f.owner, r)
			if codeOf(e) == "unavailable" {
				t.Errorf("%s (session %q) failed on the read-only database: %v", op, session, e)
			}
			if op == "heartbeat" && e == nil && out.Data.(map[string]any)["renewed"] != false {
				t.Errorf("heartbeat renewed its lease in maintenance: %v", out.Data)
			}
		}
	}
	if len(misread) > 0 || len(skipped) > 0 {
		t.Fatalf("pure reads tried to write in maintenance: misclassified %v, beats %v", misread, skipped)
	}
	if after := f.lastSeen("me"); after != before {
		t.Fatal("a heartbeat was recorded in maintenance")
	}
	// Out of maintenance the same store tries the heartbeat (and the
	// read-only database refuses it, which is logged and skipped).
	ro.SetQuiet(func() bool { return false })
	if _, e := ro.ExecuteAs(ctx, f.owner, Request{Operation: "whoami", Workspace: f.w, Project: "p", Session: "me", Args: map[string]any{"instance": "me-inst"}}); e != nil {
		t.Fatal(e)
	}
	if len(skipped) != 1 {
		t.Fatal("the quiet switch isn't what skips the beat:", skipped)
	}
}

// maintenanceStore is a hosted store in a schema of its own: the
// maintenance row is one per database.
func maintenanceStore(t *testing.T) (*Store, *sql.DB, string) {
	t.Helper()
	dsn := os.Getenv("CLAWBRIDGE_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("reviewer supplies disposable Postgres DSN")
	}
	ctx := context.Background()
	admin, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { admin.Close() })
	schema := strings.ReplaceAll(newID("mt"), "-", "_")
	if _, err = admin.ExecContext(ctx, `CREATE SCHEMA `+schema); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { admin.ExecContext(context.Background(), `DROP SCHEMA `+schema+` CASCADE`) })
	parsed, err := url.Parse(dsn)
	if err != nil {
		t.Fatal(err)
	}
	q := parsed.Query()
	q.Set("search_path", schema)
	parsed.RawQuery = q.Encode()
	s, err := OpenPostgres(ctx, parsed.String(), newID("owner")+"@maintenance-test.example")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s, s.db, parsed.String()
}

// The row: a start, a start again that extends and keeps its start, an
// end, the TTL, and the input bounds.
func TestPostgresMaintenanceRow(t *testing.T) {
	s, _, _ := maintenanceStore(t)
	ctx := context.Background()
	if m, err := s.MaintenanceState(ctx); err != nil || m.Active || m.Now.IsZero() {
		t.Fatal(m, err)
	}
	first, err := s.StartMaintenance(ctx, "deploy v0.3.9", "operator", 10*time.Minute)
	if err != nil || !first.Active || first.Reason != "deploy v0.3.9" || first.StartedBy != "operator" {
		t.Fatal(first, err)
	}
	if left := first.Remaining(); left < 9*time.Minute || left > 10*time.Minute {
		t.Fatal(left)
	}
	again, err := s.StartMaintenance(ctx, "deploy v0.3.9 retry", "operator", 20*time.Minute)
	if err != nil || !again.StartedAt.Equal(first.StartedAt) || !again.EndsAt.After(first.EndsAt) || again.Reason != "deploy v0.3.9 retry" {
		t.Fatal("a start while on must extend and keep its start:", again, err)
	}
	if ended, err := s.EndMaintenance(ctx); err != nil || !ended {
		t.Fatal(ended, err)
	}
	if ended, err := s.EndMaintenance(ctx); err != nil || ended {
		t.Fatal("a second end:", ended, err)
	}
	short, err := s.StartMaintenance(ctx, "short", "operator", time.Second)
	if err != nil || !short.Active {
		t.Fatal(short, err)
	}
	time.Sleep(1100 * time.Millisecond)
	if m, err := s.MaintenanceState(ctx); err != nil || m.Active || m.Remaining() != 0 {
		t.Fatal("the TTL didn't end it:", m, err)
	}
	// A start after a run-out one starts afresh.
	fresh, err := s.StartMaintenance(ctx, "next", "operator", time.Minute)
	if err != nil || !fresh.StartedAt.After(short.StartedAt) {
		t.Fatal(fresh, short, err)
	}
	for _, c := range []struct {
		reason string
		ttl    time.Duration
	}{{"", time.Minute}, {"  ", time.Minute}, {"bad\nline", time.Minute}, {strings.Repeat("x", 201), time.Minute}, {"ok", 0}, {"ok", 61 * time.Minute}} {
		if _, err := s.StartMaintenance(ctx, c.reason, "operator", c.ttl); !errors.Is(err, ErrMaintenanceInput) {
			t.Fatal(c, err)
		}
	}
	if _, err := s.StartMaintenance(ctx, strings.Repeat("é", 200), "operator", time.Minute); err != nil {
		t.Fatal("200 characters is the bound, not 200 bytes:", err)
	}
}

// A serving process's start ends a maintenance that began before it, says
// so only for an active one, and prunes instance rows unseen for a day.
func TestPostgresMaintenanceClearedAtStart(t *testing.T) {
	s, db, _ := maintenanceStore(t)
	ctx := context.Background()
	if cleared, err := s.ClearMaintenanceAtStart(ctx); err != nil || cleared {
		t.Fatal("no row:", cleared, err)
	}
	if _, err := s.StartMaintenance(ctx, "deploy", "operator", time.Minute); err != nil {
		t.Fatal(err)
	}
	if cleared, err := s.ClearMaintenanceAtStart(ctx); err != nil || !cleared {
		t.Fatal("an active row:", cleared, err)
	}
	if m, _ := s.MaintenanceState(ctx); m.Active {
		t.Fatal("still on")
	}
	if _, err := s.StartMaintenance(ctx, "deploy", "operator", time.Second); err != nil {
		t.Fatal(err)
	}
	time.Sleep(1100 * time.Millisecond)
	if cleared, err := s.ClearMaintenanceAtStart(ctx); err != nil || cleared {
		t.Fatal("a run-out row isn't an active one:", cleared, err)
	}
	var rows int
	if err := db.QueryRow(`SELECT COUNT(*) FROM server_maintenance`).Scan(&rows); err != nil || rows != 0 {
		t.Fatal("the run-out row stayed:", rows, err)
	}
	if err := s.ReportServerInstance(ctx, "clawbridge-server-old", time.Time{}); err != nil {
		t.Fatal(err)
	}
	if err := s.ReportServerInstance(ctx, "clawbridge-server-new", time.Time{}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE server_maintenance_instances SET seen_at=now()-interval '25 hours' WHERE instance='clawbridge-server-old'`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ClearMaintenanceAtStart(ctx); err != nil {
		t.Fatal(err)
	}
	list, err := s.MaintenanceInstances(ctx)
	if err != nil || len(list) != 1 || list[0].Instance != "clawbridge-server-new" {
		t.Fatal("not pruned:", list, err)
	}
}

// Instances: live within the window by the database's clock; drained only
// for the current maintenance, so a drain reported for an earlier one never
// counts for the next.
func TestPostgresMaintenanceInstances(t *testing.T) {
	s, db, _ := maintenanceStore(t)
	ctx := context.Background()
	m, err := s.StartMaintenance(ctx, "deploy", "operator", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	for _, i := range []string{"clawbridge-server-a", "clawbridge-server-b", "clawbridge-server-gone"} {
		if err := s.ReportServerInstance(ctx, i, time.Time{}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.Exec(`UPDATE server_maintenance_instances SET seen_at=now()-interval '16 seconds' WHERE instance='clawbridge-server-gone'`); err != nil {
		t.Fatal(err)
	}
	if err := s.ReportServerInstance(ctx, "clawbridge-server-a", m.StartedAt); err != nil {
		t.Fatal(err)
	}
	list, err := s.MaintenanceInstances(ctx)
	want := []MaintenanceInstance{{"clawbridge-server-a", true, true}, {"clawbridge-server-b", true, false}, {"clawbridge-server-gone", false, false}}
	if err != nil || !slices.Equal(list, want) {
		t.Fatal(list, err)
	}
	var at1 time.Time
	if err := db.QueryRow(`SELECT drained_at FROM server_maintenance_instances WHERE instance='clawbridge-server-a'`).Scan(&at1); err != nil {
		t.Fatal(err)
	}
	// Reporting the same drain again keeps when it drained.
	if err := s.ReportServerInstance(ctx, "clawbridge-server-a", m.StartedAt); err != nil {
		t.Fatal(err)
	}
	var at2 time.Time
	if err := db.QueryRow(`SELECT drained_at FROM server_maintenance_instances WHERE instance='clawbridge-server-a'`).Scan(&at2); err != nil || !at2.Equal(at1) {
		t.Fatal(at1, at2, err)
	}
	// A new maintenance: a's drain was for the old one.
	if _, err := s.EndMaintenance(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := s.StartMaintenance(ctx, "again", "operator", time.Minute); err != nil {
		t.Fatal(err)
	}
	if list, _ = s.MaintenanceInstances(ctx); list[0].Drained {
		t.Fatal("an old maintenance's drain counted:", list)
	}
	// A run-out maintenance's row drains no one.
	short, err := s.StartMaintenance(ctx, "short", "operator", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.ReportServerInstance(ctx, "clawbridge-server-a", short.StartedAt); err != nil {
		t.Fatal(err)
	}
	if list, _ = s.MaintenanceInstances(ctx); !list[0].Drained {
		t.Fatal("not drained for the short one:", list)
	}
	time.Sleep(1100 * time.Millisecond)
	if list, _ = s.MaintenanceInstances(ctx); list[0].Drained {
		t.Fatal("drained for a run-out maintenance:", list)
	}
	// Outside maintenance nobody is drained, whatever they reported.
	if _, err := s.EndMaintenance(ctx); err != nil {
		t.Fatal(err)
	}
	if list, _ = s.MaintenanceInstances(ctx); list[0].Drained {
		t.Fatal(list)
	}
	if err := s.ReportServerInstance(ctx, "clawbridge-server-a", time.Time{}); err != nil {
		t.Fatal(err)
	}
	var drainedAt sql.NullTime
	if err := db.QueryRow(`SELECT drained_at FROM server_maintenance_instances WHERE instance='clawbridge-server-a'`).Scan(&drainedAt); err != nil || drainedAt.Valid {
		t.Fatal("an undrained report kept the drain:", drainedAt, err)
	}
}

// SQLite has no servers to drain: the API refuses, and the schema only
// takes the number.
func TestMaintenanceIsHostedOnly(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	if _, err := h.s.MaintenanceState(ctx); err == nil {
		t.Fatal("SQLite answered")
	}
	if _, err := h.s.StartMaintenance(ctx, "x", "operator", time.Minute); err == nil {
		t.Fatal("SQLite started")
	}
	var version int
	if err := h.s.db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil || version < maintenanceSchemaVersion {
		t.Fatal(version, err)
	}
}

// The maintenance command opens any schema with the drain's tables, older
// or newer than its own (the new image drains the running server before
// the swap), and refuses one without them.
func TestPostgresMaintenanceOpensAcrossVersions(t *testing.T) {
	_, db, dsn := maintenanceStore(t)
	ctx := context.Background()
	open := func() error {
		s, err := OpenPostgresMaintenance(ctx, dsn)
		if err == nil {
			_, err = s.MaintenanceState(ctx)
			s.Close()
		}
		return err
	}
	if err := open(); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO clawbridge_schema(version) VALUES($1)`, postgresSchemaVersion+1); err != nil {
		t.Fatal(err)
	}
	if err := open(); err != nil {
		t.Fatal("a newer schema:", err)
	}
	if _, err := OpenPostgresOperator(ctx, dsn); err == nil {
		t.Fatal("the other operator commands still need the exact schema")
	}
	if _, err := db.Exec(`DELETE FROM clawbridge_schema WHERE version>=$1`, maintenanceSchemaVersion); err != nil {
		t.Fatal(err)
	}
	if err := open(); err == nil || !strings.Contains(err.Error(), "skip maintenance for this deploy") {
		t.Fatal("a schema without the drain:", err)
	}
}

// The migration that creates the drain's tables is the one
// maintenanceSchemaVersion names, so a renumber at landing can't leave
// OpenPostgresMaintenance accepting a schema without them (H1).
func TestMaintenanceMigrationUsesItsVersion(t *testing.T) {
	for file, step := range map[string]string{
		"postgres.go": "\tif version < maintenanceSchemaVersion {\n\t\tif _, err = tx.ExecContext(ctx, maintenanceSchema+fmt.Sprintf(`INSERT INTO clawbridge_schema(version) VALUES(%d)`, maintenanceSchemaVersion)); err != nil {",
		"store.go":    "\tif version < maintenanceSchemaVersion {\n\t\t// The server drain's tables are hosted only (maintenance.go).\n\t\tif _, err = tx.Exec(fmt.Sprintf(`PRAGMA user_version=%d;`, maintenanceSchemaVersion)); err != nil {",
	} {
		src, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Count(string(src), step) != 1 || strings.Count(string(src), "maintenanceSchema+") != boolCount(file == "postgres.go") {
			t.Fatalf("%s: the drain's migration step must be gated and numbered by maintenanceSchemaVersion", file)
		}
	}
	if maintenanceSchemaVersion > postgresSchemaVersion {
		t.Fatal(maintenanceSchemaVersion, postgresSchemaVersion)
	}
}

func boolCount(b bool) int {
	if b {
		return 1
	}
	return 0
}

// The drain's columns, pinned (H2): operator and servers may differ in
// version, so a change must be additive and nullable (maintenance.go).
// Adding a nullable column means adding it here; anything else is a
// breaking change this test is meant to stop.
func TestPostgresMaintenanceColumnsPinned(t *testing.T) {
	_, db, _ := maintenanceStore(t)
	rows, err := db.Query(`SELECT table_name,column_name,data_type,is_nullable FROM information_schema.columns
		WHERE table_schema=current_schema() AND table_name IN ('server_maintenance','server_maintenance_instances')
		ORDER BY table_name,ordinal_position`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	got := []string{}
	for rows.Next() {
		var table, column, kind, nullable string
		if err := rows.Scan(&table, &column, &kind, &nullable); err != nil {
			t.Fatal(err)
		}
		got = append(got, table+"."+column+" "+kind+" "+nullable)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"server_maintenance.id integer NO",
		"server_maintenance.started_at timestamp with time zone NO",
		"server_maintenance.ends_at timestamp with time zone NO",
		"server_maintenance.reason text NO",
		"server_maintenance.started_by text NO",
		"server_maintenance_instances.instance text NO",
		"server_maintenance_instances.started_at timestamp with time zone NO",
		"server_maintenance_instances.seen_at timestamp with time zone NO",
		"server_maintenance_instances.drained_for timestamp with time zone YES",
		"server_maintenance_instances.drained_at timestamp with time zone YES",
	}
	if !slices.Equal(got, want) {
		t.Fatalf("drain columns changed:\n got %q\nwant %q", got, want)
	}
	var recorded bool
	if err := db.QueryRow(`SELECT EXISTS(SELECT 1 FROM clawbridge_schema WHERE version=$1)`, maintenanceSchemaVersion).Scan(&recorded); err != nil || !recorded {
		t.Fatal("the migration didn't record maintenanceSchemaVersion", err)
	}
}
