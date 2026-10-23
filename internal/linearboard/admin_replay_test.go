package linearboard

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"clawbridge/internal/linearprovider"
)

// RN7-1: an admin write whose audit failed, and whose binding changed since,
// cannot be reached by a retry; the replay records it from the ledger and
// sends nothing to Linear.
func TestAdminAuditReplayAfterTheBindingChanged(t *testing.T) {
	s, a, p, scope := adminTest(t)
	a.auditErr = errors.New("authority unavailable")
	in := AdminInput{Kind: "project_create", OperationID: "rp-01", TeamID: teamID, Name: "Review · Search"}
	r, e := s.Admin(context.Background(), scope, in)
	if e != nil || !strings.HasPrefix(r.Audit, "failed") || len(p.writes) != 1 {
		t.Fatal(r, e)
	}
	a.authorityV2.b.Revision = 2
	a.authorityV2.b.LinearProjectIDs = append(a.authorityV2.b.LinearProjectIDs, newProject)
	if _, e := s.Admin(context.Background(), scope, in); e == nil || !strings.Contains(e.Error(), "different scoped payload") {
		t.Fatal("a retry after the binding changed was not refused", e)
	}
	opened := 0
	s.cfg.Open = func(context.Context, *http.Client, bool) (Provider, error) { opened++; return p, nil }
	got, e := s.AdminAuditReplay(context.Background(), scope, "rp-01")
	if e != nil || !got.Replayed || got.Audit != "recorded" || got.Receipt.ID != newProject || got.Receipt.Outcome != "accepted" {
		t.Fatal(got, e)
	}
	want := map[string]any{"kind": "project_create", "outcome": "accepted", "operation_id": "rp-01", "linear_team_id": teamID, "linear_id": newProject, "name": "Review · Search"}
	if len(a.audits) != 1 {
		t.Fatal("audits", a.audits)
	}
	for k, v := range want {
		if a.audits[0][k] != v {
			t.Fatal("replayed audit", k, a.audits[0])
		}
	}
	for _, k := range []string{"previous_name", "linear_project_id", "policy_revision"} {
		if _, ok := a.audits[0][k]; ok {
			t.Fatal("replayed audit carries", k, a.audits[0])
		}
	}
	if again, e := s.AdminAuditReplay(context.Background(), scope, "rp-01"); e != nil || again.Audit != "already" || len(a.audits) != 1 {
		t.Fatal("second replay", again, e)
	}
	if opened != 0 || len(p.writes) != 1 {
		t.Fatal("the replay opened Linear or wrote", opened, p.writes)
	}
}

// A label replay carries the policy revision it was checked against, and an
// initiative add-project names the initiative and the project.
func TestAdminAuditReplayRebuildsEveryKind(t *testing.T) {
	s, a, p, scope := adminTest(t)
	p.initiatives = map[string][]linearprovider.Initiative{}
	writes := []AdminInput{
		{Kind: "label_create", OperationID: "l-1", TeamID: teamID, Name: "Bug"},
		{Kind: "initiative_create", OperationID: "i-1", Name: "Product Review"},
		{Kind: "initiative_add_project", OperationID: "i-2", InitiativeID: newInitiative, ProjectID: projectA},
	}
	for _, in := range writes {
		a.auditErr = errors.New("authority unavailable")
		if r, e := s.Admin(context.Background(), scope, in); e != nil || !strings.HasPrefix(r.Audit, "failed") {
			t.Fatal(in.Kind, r, e)
		}
	}
	a.revision = 4
	for _, in := range writes {
		if r, e := s.AdminAuditReplay(context.Background(), scope, in.OperationID); e != nil || r.Audit != "recorded" {
			t.Fatal(in.Kind, r, e)
		}
	}
	if len(a.audits) != 3 || a.audits[0]["policy_revision"] != 3 || a.audits[1]["linear_id"] != newInitiative || a.audits[1]["name"] != "Product Review" || a.audits[2]["linear_id"] != newInitiative || a.audits[2]["linear_project_id"] != projectA {
		t.Fatal(a.audits)
	}
}

// Only the owner replays, and only an accepted admin write of this project
// and session; nothing else is recorded.
func TestAdminAuditReplayRefusals(t *testing.T) {
	s, a, _, scope := adminTest(t)
	l := ledger{s.cfg.LedgerDirectory, s.cfg.Now}
	namespace := digest([]string{s.cfg.Authority, "ws", "prj", "ses"})
	if _, e := l.write(namespace, "pending-1", "payload", func() (any, error) { return nil, errors.New("connection reset") }); e == nil {
		t.Fatal("pending write succeeded")
	}
	if _, e := l.write(namespace, "refused-1", "payload", func() (any, error) { return nil, refusedBeforeDispatch(errors.New("binding changed")) }); e == nil {
		t.Fatal("refused write succeeded")
	}
	if _, e := l.write(namespace, "issue-1", "payload", func() (any, error) { return map[string]any{"id": issueID}, nil }); e != nil {
		t.Fatal(e)
	}
	for id, want := range map[string]string{
		"pending-1": "uncertain",
		"refused-1": "refused",
		"issue-1":   "not an admin write",
		"missing":   "no admin write with this operation_id",
		"bad id!":   "requires operation_id",
	} {
		if _, e := s.AdminAuditReplay(context.Background(), scope, id); e == nil || !strings.Contains(e.Error(), want) {
			t.Fatal(id, e)
		}
	}
	// Another session's write is in another namespace.
	a.auditErr = errors.New("authority unavailable")
	if _, e := s.Admin(context.Background(), scope, AdminInput{Kind: "project_create", OperationID: "rp-02", TeamID: teamID, Name: "Q4"}); e != nil {
		t.Fatal(e)
	}
	a.authorityV2.actor = "other"
	if _, e := s.AdminAuditReplay(context.Background(), scope, "rp-02"); e == nil || !strings.Contains(e.Error(), "--session") {
		t.Fatal("another session replayed", e)
	}
	a.authorityV2.actor = "ses"
	a.role = "member"
	if _, e := s.AdminAuditReplay(context.Background(), scope, "rp-02"); !errors.Is(e, errAdminOwnerOnly) {
		t.Fatal("a member replayed", e)
	}
	if len(a.audits) != 0 {
		t.Fatal("a refused replay recorded", a.audits)
	}
}

// A locked record is an operation still running: it is not replayed.
func TestAdminAuditReplayRefusesALockedRecord(t *testing.T) {
	s, a, _, scope := adminTest(t)
	a.auditErr = errors.New("authority unavailable")
	if _, e := s.Admin(context.Background(), scope, AdminInput{Kind: "project_create", OperationID: "rp-03", TeamID: teamID, Name: "Q4"}); e != nil {
		t.Fatal(e)
	}
	path := filepath.Join(s.cfg.LedgerDirectory, digest(digest([]string{s.cfg.Authority, "ws", "prj", "ses"})), digest("rp-03")+".json.lock")
	if e := os.WriteFile(path, nil, 0o600); e != nil {
		t.Fatal(e)
	}
	if _, e := s.AdminAuditReplay(context.Background(), scope, "rp-03"); e == nil || !strings.Contains(e.Error(), "locked") || len(a.audits) != 0 {
		t.Fatal("a locked record was replayed", e)
	}
}

// The write path keeps the payload check that only a replay skips: the same
// operation ID with another payload is refused before its dispatch runs.
func TestLedgerWriteRefusesAnotherPayloadForTheSameOperation(t *testing.T) {
	s, _, _, _ := adminTest(t)
	l := ledger{s.cfg.LedgerDirectory, s.cfg.Now}
	if _, e := l.write("ns", "op-1", "payload-a", func() (any, error) { return map[string]any{"id": issueID}, nil }); e != nil {
		t.Fatal(e)
	}
	dispatched := false
	if _, e := l.write("ns", "op-1", "payload-b", func() (any, error) { dispatched = true; return map[string]any{"id": issueID}, nil }); e == nil || !strings.Contains(e.Error(), "different scoped payload") || dispatched {
		t.Fatal("another payload reused the operation", e, dispatched)
	}
	if r, ok, e := l.lookupAny("ns", "op-1"); e != nil || !ok || r == nil {
		t.Fatal("lookupAny", r, ok, e)
	}
}

// A service with a shared ledger (a hosted server) replays from that ledger,
// the one its admin writes were recorded in.
func TestAdminAuditReplayUsesTheServiceLedger(t *testing.T) {
	s, a, p, scope := adminTest(t)
	s.cfg.Ledger = SharedLedger(newMemLedgerStore(), nil)
	a.auditErr = errors.New("authority unavailable")
	if r, e := s.Admin(context.Background(), scope, AdminInput{Kind: "project_create", OperationID: "rp-04", TeamID: teamID, Name: "Q4"}); e != nil || !strings.HasPrefix(r.Audit, "failed") {
		t.Fatal(r, e)
	}
	a.authorityV2.b.Revision = 2
	if r, e := s.AdminAuditReplay(context.Background(), scope, "rp-04"); e != nil || r.Audit != "recorded" || len(a.audits) != 1 || len(p.writes) != 1 {
		t.Fatal("replay from the shared ledger", r, e, a.audits)
	}
}
