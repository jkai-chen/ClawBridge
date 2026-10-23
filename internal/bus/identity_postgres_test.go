package bus

import (
	"context"
	"errors"
	"os"
	"strings"
	"sync"
	"testing"
)

type pgIdentityFixture struct {
	t         *testing.T
	s         *Store
	owner     string
	workspace string
}

// newPGIdentityFixture opens the reviewer's disposable Postgres with a fresh
// owner and workspace, so a reused database never sees an earlier binding.
func newPGIdentityFixture(t *testing.T) *pgIdentityFixture {
	t.Helper()
	dsn := os.Getenv("CLAWBRIDGE_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("reviewer supplies disposable Postgres DSN")
	}
	f := &pgIdentityFixture{t: t, owner: newID("owner") + "@identity-test.example", workspace: newID("idws")}
	s, err := OpenPostgres(context.Background(), dsn, f.owner)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	f.s = s
	f.must(f.owner, "sub-owner", Request{Operation: "workspace_init", Workspace: f.workspace, Args: map[string]any{"mode": "server"}})
	f.must(f.owner, "sub-owner", Request{Operation: "project_create", Workspace: f.workspace, Args: map[string]any{"slug": "p"}})
	return f
}

func (f *pgIdentityFixture) as(principal, subject string, r Request) (Result, error) {
	return f.s.ExecuteAsIdentity(context.Background(), Identity{Principal: principal, Issuer: GoogleIssuer, Subject: subject}, r)
}

func (f *pgIdentityFixture) must(principal, subject string, r Request) Result {
	f.t.Helper()
	out, err := f.as(principal, subject, r)
	if err != nil {
		f.t.Fatal(r.Operation, err)
	}
	return out
}

func (f *pgIdentityFixture) member(principal string) {
	f.t.Helper()
	f.must(f.owner, "sub-owner", Request{Operation: "project_role_set", Workspace: f.workspace, Project: "p", Args: map[string]any{"principal": principal, "role": "member"}})
}

func (f *pgIdentityFixture) list() Request {
	return Request{Operation: "list_projects", Workspace: f.workspace}
}

func (f *pgIdentityFixture) count(query, principal string) int {
	f.t.Helper()
	var n int
	if err := f.s.db.QueryRowContext(context.Background(), query, principal).Scan(&n); err != nil {
		f.t.Fatal(err)
	}
	return n
}

func refusedWith(err error, want string) bool {
	var coded interface{ Code() string }
	return err != nil && errors.As(err, &coded) && coded.Code() == "forbidden" && strings.Contains(err.Error(), want)
}

// End to end on Postgres: the reviewer supplies a disposable DSN.
func TestPostgresIdentityBindingRefusalAndOwnerUnbind(t *testing.T) {
	f := newPGIdentityFixture(t)
	user := newID("u") + "@identity-test.example"
	outsider := newID("x") + "@identity-test.example"
	// A non-member is refused by the membership check and writes nothing,
	// however often it calls.
	for range 3 {
		if _, err := f.as(outsider, "sub-x", f.list()); !refusedWith(err, "membership") {
			t.Fatal("outsider not refused at membership", err)
		}
	}
	if n := f.count(`SELECT COUNT(*) FROM principal_subjects WHERE principal=$1`, outsider) + f.count(`SELECT COUNT(*) FROM principal_events WHERE principal=$1`, outsider); n != 0 {
		t.Fatal("a non-member wrote binding rows or events:", n)
	}
	f.member(user)
	f.must(user, "sub-1", f.list())
	for range 3 {
		if _, err := f.as(user, "sub-2", f.list()); !refusedWith(err, "not the one bound") {
			t.Fatal("another account was accepted for a bound principal", err)
		}
	}
	if n := f.count(`SELECT COUNT(*) FROM principal_events WHERE principal=$1 AND action='principal_subject_mismatch'`, user); n != 1 {
		t.Fatal("mismatch events not deduplicated:", n)
	}
	// The user is a bound member signing in with the bound account, so the
	// only thing refusing this unbind is the server-owner rule.
	if _, err := f.as(user, "sub-1", Request{Operation: "principal_unbind", Args: map[string]any{"principal": user}}); !refusedWith(err, "only the server owner") {
		t.Fatal("a non-owner unbind was not refused by the owner rule", err)
	}
	f.must(f.owner, "sub-owner", Request{Operation: "principal_unbind", Args: map[string]any{"principal": user}})
	// The released account races the new one and loses.
	if _, err := f.as(user, "sub-1", f.list()); !refusedWith(err, "released from") {
		t.Fatal("old account rebound after unbind", err)
	}
	f.must(user, "sub-2", f.list())
	if _, err := f.as(user, "sub-1", f.list()); !refusedWith(err, "not the one bound") {
		t.Fatal("old account accepted after rebinding", err)
	}
	// An unbind made by mistake is undone with allow_previous.
	f.must(f.owner, "sub-owner", Request{Operation: "principal_unbind", Args: map[string]any{"principal": user}})
	f.must(f.owner, "sub-owner", Request{Operation: "principal_unbind", Args: map[string]any{"principal": user, "allow_previous": true}})
	f.must(user, "sub-2", f.list())
	if n := f.count(`SELECT COUNT(*) FROM principal_events WHERE principal=$1 AND action='principal_unbound_allow_previous'`, user); n != 1 {
		t.Fatal("allow-previous unbind not audited:", n)
	}
	_, err := f.s.db.ExecContext(context.Background(), `UPDATE principal_events SET action='principal_bound' WHERE principal=$1`, user)
	if err == nil || !strings.Contains(err.Error(), "principal_events is append-only") {
		t.Fatal("principal events are not append-only, or the refusal does not name them", err)
	}
}

func TestPostgresConcurrentFirstSightBindsOneAccount(t *testing.T) {
	f := newPGIdentityFixture(t)
	user := newID("c") + "@identity-test.example"
	f.member(user)
	var wg sync.WaitGroup
	results := make([]error, 4)
	for i := range results {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, results[i] = f.as(user, []string{"sub-a", "sub-b"}[i%2], f.list())
		}()
	}
	wg.Wait()
	won := map[string]int{}
	for i, err := range results {
		if err == nil {
			won[[]string{"sub-a", "sub-b"}[i%2]]++
		} else if !refusedWith(err, "not the one bound") {
			t.Fatal("unexpected error", err)
		}
	}
	if len(won) != 1 {
		t.Fatal("concurrent first sight bound both or neither account", results)
	}
	if n := f.count(`SELECT COUNT(*) FROM principal_events WHERE principal=$1 AND action='principal_bound'`, user); n != 1 {
		t.Fatal("bound events:", n)
	}
}

func TestPostgresOperatorUnbindsTheServerOwner(t *testing.T) {
	f := newPGIdentityFixture(t)
	operator, err := OpenPostgresOperator(context.Background(), os.Getenv("CLAWBRIDGE_TEST_POSTGRES_DSN"))
	if err != nil {
		t.Fatal(err)
	}
	defer operator.Close()
	if _, err = f.as(f.owner, "sub-owner-new", f.list()); !refusedWith(err, "not the one bound") {
		t.Fatal("owner's new account accepted before recovery", err)
	}
	if out, err := operator.OperatorUnbindPrincipal(context.Background(), f.owner, false); err != nil || out["unbound"] != true {
		t.Fatal(out, err)
	}
	f.must(f.owner, "sub-owner-new", f.list())
	if _, err = f.as(f.owner, "sub-owner", f.list()); !refusedWith(err, "not the one bound") {
		t.Fatal("owner's released account still accepted", err)
	}
}

func TestPostgresHostedStoreRefusesRequestsWithoutAnAccount(t *testing.T) {
	f := newPGIdentityFixture(t)
	f.s.RequireVerifiedAccount()
	// An Executor wrapper that drops the account reaches ExecuteAs.
	if _, err := f.s.ExecuteAs(context.Background(), f.owner, f.list()); err == nil || !strings.Contains(err.Error(), "ExecuteAsIdentity") {
		t.Fatal("hosted store ran a request without its account binding", err)
	}
	f.must(f.owner, "sub-owner", f.list())
}
