package bus

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"strconv"
	"strings"
	"testing"
	"time"
)

func itoa(i int) string { return strconv.Itoa(i) }

// PR-B: the shared ledger's uncertain issue writes are listed to the
// row's own person, all of them to the project owner, never another
// project's or an accepted one; an ack, by the row's person or the owner
// from their own device, hides the row and nothing else: its record
// stays pending, so a retry still sends nothing.
func TestPostgresWriteLedgerUncertain(t *testing.T) {
	f := newPGIdentityFixture(t)
	ctx := context.Background()
	device := map[string]string{}
	people := map[string]string{}
	for _, role := range []string{"owner", "member"} {
		p := newID(role) + "@identity-test.example"
		code := f.invite(f.owner, p, role)["code"].(string)
		key := newKey(t)
		if _, err := f.join(p, code, key); err != nil {
			t.Fatal(err)
		}
		people[role], device[role] = p, keyID(t, key)
	}
	as := func(role string, r Request) (Result, error) {
		r.Workspace, r.Project = f.workspace, "p"
		return f.asDevice(people[role], device[role], r)
	}
	list := func(role string) []string {
		t.Helper()
		out, err := as(role, Request{Operation: "issue_writes_uncertain", Args: map[string]any{}})
		if err != nil {
			t.Fatal(err)
		}
		var ops []string
		for _, w := range out.Data.(map[string]any)["writes"].([]map[string]any) {
			ops = append(ops, w["operation_id"].(string)+"/"+w["operation"].(string))
		}
		return ops
	}
	var pid string
	if err := f.s.db.QueryRow(`SELECT p.id FROM projects p JOIN workspaces w ON w.id=p.workspace_id WHERE p.slug='p' AND w.name=$1`, f.workspace).Scan(&pid); err != nil {
		t.Fatal(err)
	}
	pending := `{"operation_id":"%s","operation":"create","project_id":"` + pid + `","version":1,"digest":"` + strings.Repeat("a", 64) + `","state":"pending","reason":"timeout","updated_at":"2026-10-29T06:00:00Z"}`
	// The fixture's database outlives a run: each run's rows get their own
	// namespaces.
	run := newID("run")
	ns := func(c string) string { sum := sha256.Sum256([]byte(run + c)); return hex.EncodeToString(sum[:]) }
	rows := []struct{ ns, op, record, project, who string }{
		{ns("1"), "op-m", strings.Replace(pending, "%s", "op-m", 1), pid, people["member"]},
		{ns("2"), "op-o", strings.Replace(pending, "%s", "op-o", 1), pid, people["owner"]},
		{ns("3"), "op-w", strings.Replace(pending, "%s", "op-w", 1), pid, ""},
		{ns("4"), "op-a", strings.Replace(strings.Replace(pending, "%s", "op-a", 1), `"pending"`, `"accepted"`, 1), pid, people["member"]},
		{ns("5"), "op-x", strings.Replace(pending, "%s", "op-x", 1), "another-project", people["member"]},
		{ns("6"), "op-old", strings.Replace(pending, "%s", "op-old", 1), "", people["member"]},
	}
	for _, r := range rows {
		if _, created, err := f.s.LedgerInsert(ctx, r.ns, r.op, r.record, r.project, r.who); err != nil || !created {
			t.Fatal(r.op, err)
		}
	}
	// R1: the project's write history is many settled rows; only pending
	// ones are read. A row settled by LedgerReplace stops being pending.
	accepted := strings.Replace(pending, `"pending"`, `"accepted"`, 1)
	for i := 0; i < 300; i++ {
		op := "hist-" + strings.Repeat("0", 3-len(itoa(i))) + itoa(i)
		if _, _, err := f.s.LedgerInsert(ctx, ns("h"+itoa(i)), op, strings.Replace(accepted, "%s", op, 1), pid, people["member"]); err != nil {
			t.Fatal(err)
		}
	}
	settling := strings.Replace(pending, "%s", "op-settled", 1)
	if _, _, err := f.s.LedgerInsert(ctx, ns("s"), "op-settled", settling, pid, people["member"]); err != nil {
		t.Fatal(err)
	}
	if err := f.s.LedgerReplace(ctx, ns("s"), "op-settled", settling, strings.Replace(accepted, "%s", "op-settled", 1)); err != nil {
		t.Fatal(err)
	}
	var stillPending int
	if err := f.s.db.QueryRow(`SELECT COUNT(*) FROM linear_write_ledger WHERE project_id=$1 AND pending=1`, pid).Scan(&stillPending); err != nil || stillPending != 3 {
		t.Fatal("pending rows:", stillPending, err)
	}
	// R2/R3: a write still being sent (no reason, just now) is listed but
	// not counted, and can't be acknowledged.
	sending := strings.Replace(strings.Replace(strings.Replace(pending, "%s", "op-now", 1), `"reason":"timeout",`, "", 1), "2026-10-29T06:00:00Z", time.Now().UTC().Format(time.RFC3339Nano), 1)
	if _, _, err := f.s.LedgerInsert(ctx, ns("n"), "op-now", sending, pid, people["member"]); err != nil {
		t.Fatal(err)
	}
	if _, err := as("member", Request{Operation: "issue_writes_ack", Args: map[string]any{"operation_id": "op-now"}}); err == nil || !strings.Contains(err.Error(), "still being sent") {
		t.Fatal("a write still being sent was acknowledged:", err)
	}
	count := func(role string) any {
		t.Helper()
		ov, err := as(role, Request{Operation: "project_overview", Args: map[string]any{}})
		if err != nil {
			t.Fatal(err)
		}
		return ov.Data.(map[string]any)["linear_writes"].(map[string]any)["uncertain_issue_writes"]
	}
	if count("member") != 1 || count("owner") != 3 {
		t.Fatal("counts by role, without the one still being sent:", count("member"), count("owner"))
	}
	if got := list("member"); strings.Join(got, ",") != "op-m/create,op-now/create" {
		t.Fatal("member sees:", got)
	}
	if got := list("owner"); strings.Join(got, ",") != "op-m/create,op-now/create,op-o/create,op-w/create" {
		t.Fatal("owner sees:", got)
	}
	if _, err := as("member", Request{Operation: "issue_writes_ack", Args: map[string]any{"operation_id": "op-o"}}); err == nil || !strings.Contains(err.Error(), "no unacknowledged") {
		t.Fatal("member acked the owner's write:", err)
	}
	if _, err := as("member", Request{Operation: "issue_writes_ack", Session: "m", Args: map[string]any{"operation_id": "op-m"}}); err == nil {
		t.Fatal("a session acked")
	}
	if _, err := f.as(f.owner, "sub-owner", Request{Operation: "issue_writes_ack", Workspace: f.workspace, Project: "p", Args: map[string]any{"operation_id": "op-w"}}); !refusedWith(err, "device") {
		t.Fatal("a non-device ack:", err)
	}
	out, err := as("member", Request{Operation: "issue_writes_ack", Args: map[string]any{"operation_id": "op-m"}})
	if err != nil || out.Data.(map[string]any)["acknowledged"] != 1 {
		t.Fatal(out, err)
	}
	if got := list("member"); strings.Join(got, ",") != "op-now/create" {
		t.Fatal("an acknowledged write is listed:", got)
	}
	// The record is untouched: a retry under the same operation ID still
	// finds it pending.
	if record, found, err := f.s.LedgerGet(ctx, ns("1"), "op-m"); err != nil || !found || record != rows[0].record {
		t.Fatal("the ack changed the record:", record, err)
	}
	if _, err = as("owner", Request{Operation: "issue_writes_ack", Args: map[string]any{"operation_id": "op-w"}}); err != nil {
		t.Fatal("the owner acks the server's own write:", err)
	}
	ov, err := as("owner", Request{Operation: "project_overview", Args: map[string]any{}})
	if err != nil || ov.Data.(map[string]any)["linear_writes"].(map[string]any)["uncertain_issue_writes"] != 1 {
		t.Fatal("overview count after the acks:", ov.Data, err)
	}
}
