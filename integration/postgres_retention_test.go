package integration

import (
	"context"
	"net/url"

	"clawbridge/internal/bus"
	"database/sql"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"
)

// Direct SQL here only constructs expiry/failure/cap fixtures in the disposable
// reviewer database; application assertions still cross Store.ExecuteAs.
func TestPostgresRetentionRollbackAndHonestCap(t *testing.T) {
	f := newPGFixture(t)
	f.setup("p")
	db, err := sql.Open("pgx", os.Getenv("CLAWBRIDGE_TEST_POSTGRES_DSN"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	who := object(t, f.call(pgOwner, "whoami", "p", "", nil))
	wid := stringField(t, who["workspace"], "id")
	pid := stringField(t, who["project"], "id")
	sent := f.send("p", "alice", "bob", "retention body", "retention-send")
	id := stringField(t, sent, "id")
	expired := time.Now().UTC().Add(-time.Hour).Format(time.RFC3339Nano)
	if _, err = db.Exec(`UPDATE messages SET expires_at=$1 WHERE id=$2`, expired, id); err != nil {
		t.Fatal(err)
	}
	if got := pageItems(t, "messages", f.call(pgPrincipal("bob"), "inbox", "p", "bob", actor("bob", nil))); len(got) != 0 {
		t.Fatal("expired message returned in inbox")
	}
	snapshot := object(t, f.call(pgPrincipal("bob"), "snapshot", "p", "bob", actor("bob", nil)))
	if len(array(t, snapshot["messages"])) != 0 {
		t.Fatal("expired body exported in snapshot")
	}
	f.reject(pgPrincipal("alice"), "message_status", "p", "alice", actor("alice", map[string]any{"message_id": id}))
	f.reject(pgPrincipal("alice"), "purge_expired", "p", "", nil)
	purged := object(t, f.call(pgOwner, "purge_expired", "p", "", nil))
	if purged["purged_messages"] != float64(1) || purged["deleted_deliveries"] != float64(1) {
		t.Fatalf("purge counts %v", purged)
	}
	var body string
	var deliveries int
	if err = db.QueryRow(`SELECT body FROM messages WHERE id=$1`, id).Scan(&body); err != nil || body != "" {
		t.Fatalf("purged content retained: %v", err)
	}
	if err = db.QueryRow(`SELECT COUNT(*) FROM deliveries WHERE message_id=$1`, id).Scan(&deliveries); err != nil || deliveries != 0 {
		t.Fatalf("purged deliveries retained: %d %v", deliveries, err)
	}
	// An identical retry must retain the original identity/tombstone and cannot resurrect content.
	replay := f.send("p", "alice", "bob", "retention body", "retention-send")
	if stringField(t, replay, "id") != id || object(t, replay)["body"] != "" {
		t.Fatalf("purged retry resurrected message: %v", replay)
	}
	f.reject(pgPrincipal("alice"), "send", "p", "alice", actor("alice", map[string]any{"to": "bob", "body": "changed", "operation_id": "retention-send"}))
	// Install a scoped delivery failure after expansion starts; all writes must roll back.
	bob := object(t, f.call(pgPrincipal("bob"), "whoami", "p", "bob", actor("bob", nil)))["session"]
	recipient := stringField(t, bob, "id")
	suffix := strings.ReplaceAll(f.w, "-", "_")
	function := "review_fail_" + suffix
	trigger := "review_trigger_" + suffix
	if _, err = db.Exec(`CREATE FUNCTION ` + function + `() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'reviewer injected delivery failure'; END $$`); err != nil {
		t.Fatal(err)
	}
	defer db.Exec(`DROP FUNCTION IF EXISTS ` + function + `() CASCADE`)
	if _, err = db.Exec(fmt.Sprintf(`CREATE TRIGGER %s BEFORE INSERT ON deliveries FOR EACH ROW WHEN (NEW.recipient_id='%s') EXECUTE FUNCTION %s()`, trigger, recipient, function)); err != nil {
		t.Fatal(err)
	}
	defer db.Exec(`DROP TRIGGER IF EXISTS ` + trigger + ` ON deliveries`)
	count := func(table string) int {
		t.Helper()
		var n int
		if err := db.QueryRow(`SELECT COUNT(*) FROM `+table+` WHERE project_id=$1`, pid).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	beforeMessages, beforeDeliveries, beforeHistory := count("messages"), count("deliveries"), count("history")
	f.reject(pgPrincipal("alice"), "send", "p", "alice", actor("alice", map[string]any{"to": "@all", "body": "atomic fanout", "operation_id": "atomic-fanout"}))
	if count("messages") != beforeMessages || count("deliveries") != beforeDeliveries || count("history") != beforeHistory {
		t.Fatal("injected fanout failure left partial message/delivery/audit")
	}
	if _, err = db.Exec(`DROP TRIGGER ` + trigger + ` ON deliveries`); err != nil {
		t.Fatal(err)
	}
	f.send("p", "alice", "@all", "atomic fanout", "atomic-fanout")
	// Exceed the documented snapshot cap without 10001 application roundtrips.
	_, err = db.Exec(`INSERT INTO history(id,workspace_id,project_id,entity,entity_key,action,details,changed_at,changed_by_principal) SELECT 'review-cap-' || $1 || '-' || g::text,$2,$1,'fixture','fixture','fixture','{}',$3,$4 FROM generate_series(1,10001) g`, pid, wid, time.Now().UTC().Format(time.RFC3339Nano), pgOwner)
	if err != nil {
		t.Fatal(err)
	}
	err = f.reject(pgPrincipal("bob"), "snapshot", "p", "bob", actor("bob", nil))
	if !strings.Contains(err.Error(), "10000") {
		t.Fatalf("cap failure not explicit: %v", err)
	}
}

func TestPostgresRejectsNewerSchema(t *testing.T) {
	f := newPGFixture(t)
	db, err := sql.Open("pgx", os.Getenv("CLAWBRIDGE_TEST_POSTGRES_DSN"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	schema := strings.ReplaceAll(f.w, "-", "_")
	if _, err = db.Exec(`CREATE SCHEMA ` + schema); err != nil {
		t.Fatal(err)
	}
	defer db.Exec(`DROP SCHEMA ` + schema + ` CASCADE`)
	if _, err = db.Exec(`CREATE TABLE ` + schema + `.clawbridge_schema(version INTEGER PRIMARY KEY); INSERT INTO ` + schema + `.clawbridge_schema VALUES(99)`); err != nil {
		t.Fatal(err)
	}
	parsed, err := url.Parse(os.Getenv("CLAWBRIDGE_TEST_POSTGRES_DSN"))
	if err != nil {
		t.Fatal(err)
	}
	query := parsed.Query()
	query.Set("search_path", schema)
	parsed.RawQuery = query.Encode()
	if store, err := bus.OpenPostgres(context.Background(), parsed.String(), pgOwner); err == nil {
		store.Close()
		t.Fatal("newer Postgres schema accepted")
	}
	var n int
	if err = db.QueryRow(`SELECT COUNT(*) FROM information_schema.tables WHERE table_schema=$1 AND table_name='workspaces'`, schema).Scan(&n); err != nil || n != 0 {
		t.Fatalf("unsupported schema modified: %d %v", n, err)
	}
}
