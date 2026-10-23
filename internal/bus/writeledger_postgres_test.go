package bus

import (
	"context"
	"strings"
	"testing"
)

// The shared ledger's records: insert once, compare-and-swap settle and
// delete, and the unsettled-comment notes.
func TestPostgresWriteLedgerRecords(t *testing.T) {
	f := newPGIdentityFixture(t)
	ctx := context.Background()
	ns := digestOf(newID("ns"))
	if _, created, err := f.s.LedgerInsert(ctx, ns, "op-1", "pending"); err != nil || !created {
		t.Fatal(created, err)
	}
	if existing, created, err := f.s.LedgerInsert(ctx, ns, "op-1", "other"); err != nil || created || existing != "pending" {
		t.Fatal("a second insert replaced the record", existing, created, err)
	}
	if err := f.s.LedgerReplace(ctx, ns, "op-1", "stale", "accepted"); err == nil {
		t.Fatal("a stale replace succeeded")
	}
	if err := f.s.LedgerReplace(ctx, ns, "op-1", "pending", "accepted"); err != nil {
		t.Fatal(err)
	}
	if r, found, err := f.s.LedgerGet(ctx, ns, "op-1"); err != nil || !found || r != "accepted" {
		t.Fatal(r, found, err)
	}
	if err := f.s.LedgerDelete(ctx, ns, "op-1", "pending"); err == nil {
		t.Fatal("a stale delete succeeded")
	}
	if err := f.s.LedgerDelete(ctx, ns, "op-1", "accepted"); err != nil {
		t.Fatal(err)
	}
	if _, found, _ := f.s.LedgerGet(ctx, ns, "op-1"); found {
		t.Fatal("deleted record found")
	}
	for _, op := range []string{"b", "a"} {
		if err := f.s.LedgerMarkUnsettled(ctx, ns, op, true); err != nil {
			t.Fatal(err)
		}
	}
	f.s.LedgerMarkUnsettled(ctx, ns, "a", true)
	if got, err := f.s.LedgerUnsettled(ctx, ns, 10); err != nil || strings.Join(got, ",") != "a,b" {
		t.Fatal(got, err)
	}
	f.s.LedgerMarkUnsettled(ctx, ns, "a", false)
	if got, _ := f.s.LedgerUnsettled(ctx, ns, 10); strings.Join(got, ",") != "b" {
		t.Fatal(got)
	}
	if _, _, err := f.s.LedgerInsert(ctx, "short", "op", "x"); err == nil {
		t.Fatal("an invalid namespace accepted")
	}
}
