package linearboard

import (
	"context"
	"crypto/ed25519"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"clawbridge/internal/bus"
	"clawbridge/internal/devicekey"
)

// storeAuthority runs requests on a real bus store, as the local store or
// as a hosted owner, and fails the first admin audit, as v0.2.6 did.
type storeAuthority struct {
	run      func(context.Context, bus.Request) (bus.Result, error)
	failNext bool
}

func (a *storeAuthority) Execute(ctx context.Context, r bus.Request) (bus.Result, error) {
	if r.Operation == "linear_write_charge" {
		return budgetAnswer(r)
	}
	if r.Operation == "linear_admin_audit" && a.failNext {
		a.failNext = false
		return bus.Result{}, errors.New("authority unavailable")
	}
	return a.run(ctx, r)
}

// The replay against the real bus: the binding changes after a write whose
// audit failed, the replay records one history row, and a second replay is
// answered from history.
func testAdminAuditReplayOnStore(t *testing.T, run func(context.Context, bus.Request) (bus.Result, error), workspace string) {
	ctx := context.Background()
	must := func(op string, a map[string]any) bus.Result {
		t.Helper()
		session := "own"
		if op == "taskboard_set_v2" {
			// The binding is a person's: sent without a session.
			session = ""
			delete(a, "instance")
		}
		r, e := run(ctx, bus.Request{Operation: op, Workspace: workspace, Project: "p", Session: session, Args: a})
		if e != nil {
			t.Fatal(op, e)
		}
		return r
	}
	must("taskboard_set_v2", map[string]any{"expected_revision": 0, "mode": "project_set", "linear_team_ids": []string{teamID, otherTeamID}, "linear_default_team_id": teamID, "linear_project_ids": []string{projectA, projectB}})
	s, _, p, _ := adminTest(t)
	authority := &storeAuthority{run: run, failNext: true}
	s.cfg.Executor = authority
	scope := bus.Request{Workspace: workspace, Project: "p", Session: "own", Args: map[string]any{"instance": "own-i"}}
	in := AdminInput{Kind: "project_create", OperationID: "cb-rp-01", TeamID: teamID, Name: "Review · Search"}
	r, e := s.Admin(ctx, scope, in)
	if e != nil || !strings.HasPrefix(r.Audit, "failed") || len(p.writes) != 1 {
		t.Fatal(r, e)
	}
	must("taskboard_set_v2", map[string]any{"expected_revision": 1, "mode": "project_set", "linear_team_ids": []string{teamID, otherTeamID}, "linear_default_team_id": teamID, "linear_project_ids": []string{projectA, projectB, newProject}})
	if _, e := s.Admin(ctx, scope, in); e == nil || !strings.Contains(e.Error(), "different scoped payload") {
		t.Fatal("a retry after the binding changed was not refused", e)
	}
	if got, e := s.AdminAuditReplay(ctx, scope, "cb-rp-01"); e != nil || got.Audit != "recorded" {
		t.Fatal(got, e)
	}
	if got, e := s.AdminAuditReplay(ctx, scope, "cb-rp-01"); e != nil || got.Audit != "already" {
		t.Fatal("second replay", got, e)
	}
	events := must("history", map[string]any{"instance": "own-i", "entity": "linear_project", "limit": 10}).Data.(map[string]any)["events"].([]map[string]any)
	if len(events) != 1 {
		t.Fatal("history rows", events)
	}
	d := events[0]["details"].(map[string]any)
	if d["operation_id"] != "cb-rp-01" || d["linear_id"] != newProject || d["name"] != "Review · Search" || d["outcome"] != "accepted" {
		t.Fatal("history row", d)
	}
	if len(p.writes) != 1 {
		t.Fatal("the replay wrote to Linear", p.writes)
	}
}

func TestAdminAuditReplayOnSQLite(t *testing.T) {
	store, e := bus.Open(filepath.Join(t.TempDir(), "bus.db"))
	if e != nil {
		t.Fatal(e)
	}
	defer store.Close()
	run := store.Execute
	for _, r := range []bus.Request{
		{Operation: "workspace_init", Workspace: "w", Args: map[string]any{"mode": "local"}},
		{Operation: "project_create", Workspace: "w", Args: map[string]any{"slug": "p"}},
		{Operation: "register_session", Workspace: "w", Project: "p", Session: "own", Args: map[string]any{"instance": "own-i"}},
	} {
		if _, e := run(context.Background(), r); e != nil {
			t.Fatal(r.Operation, e)
		}
	}
	testAdminAuditReplayOnStore(t, run, "w")
}

func TestAdminAuditReplayOnPostgres(t *testing.T) {
	dsn := os.Getenv("CLAWBRIDGE_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("reviewer supplies disposable Postgres DSN")
	}
	suffix := strconv.FormatInt(time.Now().UnixNano(), 36)
	owner := "owner-" + suffix + "@identity-test.example"
	store, e := bus.OpenPostgres(context.Background(), dsn, owner)
	if e != nil {
		t.Fatal(e)
	}
	defer store.Close()
	google := bus.Identity{Principal: owner, Issuer: bus.GoogleIssuer, Subject: "sub-owner"}
	var device string
	run := func(ctx context.Context, r bus.Request) (bus.Result, error) {
		identity := google
		if r.Operation == "taskboard_set_v2" {
			// A binding is changed by a person on their own device.
			if device == "" {
				pub, _, e := ed25519.GenerateKey(nil)
				if e != nil {
					return bus.Result{}, e
				}
				enroll := bus.Request{Operation: "device_enroll", Workspace: r.Workspace, Args: map[string]any{"public_key": devicekey.EncodePublicKey(pub), "label": "binding", "key_store": "keyring"}}
				if _, e = store.ExecuteAsIdentity(ctx, google, enroll); e != nil {
					return bus.Result{}, e
				}
				device = devicekey.ID(pub)
			}
			identity = bus.Identity{Principal: owner, Issuer: bus.DeviceIssuer, Subject: device}
		}
		out, e := store.ExecuteAsIdentity(ctx, identity, r)
		out.Source = "server"
		return out, e
	}
	workspace := "ws-" + suffix
	for _, r := range []bus.Request{
		{Operation: "workspace_init", Workspace: workspace, Args: map[string]any{"mode": "server"}},
		{Operation: "project_create", Workspace: workspace, Args: map[string]any{"slug": "p"}},
		{Operation: "register_session", Workspace: workspace, Project: "p", Session: "own", Args: map[string]any{"instance": "own-i", "role": "coordinator"}},
	} {
		if _, e := run(context.Background(), r); e != nil {
			t.Fatal(r.Operation, e)
		}
	}
	testAdminAuditReplayOnStore(t, run, workspace)
}
