package integration

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"os/user"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"

	"clawbridge/internal/bus"
	"clawbridge/internal/linearboard"
	"clawbridge/internal/linearconn"
	"clawbridge/internal/linearprovider"
)

// These are real old-source serialized files, not records created by the code
// under test. The matching authority DB preserves original canonical IDs.
type l2HistoricalLedger struct {
	SourceCommit    string                 `json:"source_commit"`
	SchemaVersion   int                    `json:"schema_version"`
	Authority       string                 `json:"authority"`
	ProfileID       string                 `json:"profile_id"`
	Scope           bus.Request            `json:"scope"`
	AcceptedInput   linearboard.WriteInput `json:"accepted_input"`
	PendingInput    linearboard.WriteInput `json:"pending_input"`
	AcceptedReceipt map[string]any         `json:"accepted_receipt"`
	LedgerFiles     map[string]string      `json:"ledger_files"`
}

type l2ForbiddenConnections struct{ calls atomic.Int64 }

func (c *l2ForbiddenConnections) Status(context.Context, string) (linearconn.Profile, error) {
	c.calls.Add(1)
	return linearconn.Profile{}, errors.New("historical retry must not open credentials")
}
func (c *l2ForbiddenConnections) HTTPClient(context.Context, string) (*http.Client, error) {
	c.calls.Add(1)
	return nil, errors.New("historical retry must not construct provider HTTP")
}

func l2LoadHistoricalL1Ledger(t *testing.T) (*bus.Store, l2HistoricalLedger, string) {
	t.Helper()
	var fixture l2HistoricalLedger
	raw, err := os.ReadFile("testdata/linear-l1-v3-ledgers.json")
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &fixture); err != nil {
		t.Fatal(err)
	}
	if fixture.SourceCommit != "8aef0f97802169efdf6895eb84bf468a3a9f05c8" || fixture.SchemaVersion != 3 || len(fixture.LedgerFiles) != 2 {
		t.Fatal("historical fixture source/schema/ledger set changed")
	}
	dump, err := os.ReadFile("testdata/linear-l1-v3.sqlite.sql")
	if err != nil {
		t.Fatal(err)
	}
	u, err := user.Current()
	if err != nil {
		t.Fatal(err)
	}
	principal := strings.ReplaceAll("local:"+u.Uid+":"+u.Username, "'", "''")
	dbPath := filepath.Join(t.TempDir(), "actual-l1-schema3.db")
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(strings.ReplaceAll(string(dump), "__LOCAL_PRINCIPAL__", principal)); err != nil {
		db.Close()
		t.Fatal(err)
	}
	var version int
	err = db.QueryRow("PRAGMA user_version").Scan(&version)
	db.Close()
	if err != nil || version != 3 {
		t.Fatalf("old-source fixture was not schema3: %d %v", version, err)
	}
	store, err := bus.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	ledger := filepath.Join(t.TempDir(), "historical-ledger")
	if err := os.Mkdir(ledger, 0700); err != nil {
		t.Fatal(err)
	}
	l2FixturePrivate(t, ledger)
	valid := regexp.MustCompile(`^[0-9a-f]{64}/[0-9a-f]{64}\.json$`)
	for name, content := range fixture.LedgerFiles {
		if !valid.MatchString(name) {
			t.Fatal("unsafe historical fixture path")
		}
		path := filepath.Join(ledger, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		l2FixturePrivate(t, filepath.Dir(path))
		if err := os.WriteFile(path, []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
		l2FixturePrivate(t, path)
	}
	return store, fixture, ledger
}

func TestL2ActualSerializedL1AcceptedAndPendingCompatibility(t *testing.T) {
	store, fixture, ledger := l2LoadHistoricalL1Ledger(t)
	before := l1LedgerBytes(t, ledger)
	connections := new(l2ForbiddenConnections)
	service := linearboard.New(linearboard.Config{Authority: fixture.Authority, Executor: store, Connections: connections, ProfileID: fixture.ProfileID, LedgerDirectory: ledger})
	receipt, err := service.Write(context.Background(), fixture.Scope, fixture.AcceptedInput)
	if err != nil || !reflect.DeepEqual(receipt, fixture.AcceptedReceipt) {
		t.Fatalf("historical accepted request lost original digest/receipt: %+v %v", receipt, err)
	}
	if _, err := service.Write(context.Background(), fixture.Scope, fixture.PendingInput); err == nil || !strings.Contains(err.Error(), "uncertain") {
		t.Fatalf("historical pending request reset, changed digest or replayed: %v", err)
	}
	if connections.calls.Load() != 0 {
		t.Fatal("accepted/pending historical retry opened credential/provider route")
	}
	if !reflect.DeepEqual(before, l1LedgerBytes(t, ledger)) {
		t.Fatal("historical serialized files were rewritten during retry")
	}
	changed := fixture.AcceptedInput
	title := "changed payload under the old operation ID"
	changed.Title = &title
	if _, err := service.Write(context.Background(), fixture.Scope, changed); err == nil || !strings.Contains(err.Error(), "different scoped payload") {
		t.Fatalf("changed old operation ID did not conflict: %v", err)
	}
	if connections.calls.Load() != 0 || !reflect.DeepEqual(before, l1LedgerBytes(t, ledger)) {
		t.Fatal("conflicting historical retry performed IO or changed ledger")
	}
}

func TestL2V2RoutePreservesActualSerializedL1Digest(t *testing.T) {
	store, fixture, ledger := l2LoadHistoricalL1Ledger(t)
	before := l1LedgerBytes(t, ledger)
	connections := new(l2ForbiddenConnections)
	service := linearboard.New(linearboard.Config{Authority: fixture.Authority, Executor: store, Connections: connections, ProfileID: fixture.ProfileID, LedgerDirectory: ledger})
	accepted := linearboard.WriteInputV2{WriteInput: fixture.AcceptedInput}
	receipt, err := service.WriteV2(context.Background(), fixture.Scope, accepted)
	if err != nil || !reflect.DeepEqual(receipt, fixture.AcceptedReceipt) {
		t.Fatalf("V2 entrypoint lost actual L1 receipt/digest: %v %v", receipt, err)
	}
	if _, err := service.WriteV2(context.Background(), fixture.Scope, linearboard.WriteInputV2{WriteInput: fixture.PendingInput}); err == nil || !strings.Contains(err.Error(), "uncertain") {
		t.Fatalf("V2 entrypoint replayed or reset actual L1 pending record: %v", err)
	}
	// An explicit empty replacement is new intent, not a legacy omission. Its
	// payload must conflict in the SAME old operation namespace before any IO.
	accepted.Labels = linearprovider.Field[[]string]{Present: true, Value: []string{}}
	if _, err := service.WriteV2(context.Background(), fixture.Scope, accepted); err == nil || !strings.Contains(err.Error(), "different scoped payload") {
		t.Fatalf("new L2 intent reused old operation ID without conflict: %v", err)
	}
	if connections.calls.Load() != 0 || !reflect.DeepEqual(before, l1LedgerBytes(t, ledger)) {
		t.Fatal("V2 historical replay/conflict opened credentials/provider or changed old ledger bytes")
	}
}
