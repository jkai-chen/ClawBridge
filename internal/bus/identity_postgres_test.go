package bus

import (
	"context"
	"errors"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
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

func (f *pgIdentityFixture) asHD(principal, hd string, r Request) (Result, error) {
	return f.s.ExecuteAsIdentity(context.Background(), Identity{Principal: principal, Issuer: GoogleIssuer, Subject: "sub-" + principal, HostedDomain: hd}, r)
}

func TestPostgresWorkspaceDomainAllowList(t *testing.T) {
	f := newPGIdentityFixture(t)
	ctx := context.Background()
	owner := func(r Request) (Result, error) {
		if r.Workspace == "" {
			r.Workspace = f.workspace
		}
		return f.s.ExecuteAsIdentity(ctx, Identity{Principal: f.owner, Issuer: GoogleIssuer, Subject: "sub-owner", HostedDomain: "Corp.Example."}, r)
	}
	setDomains := func(domains ...any) (Result, error) {
		return owner(Request{Operation: "workspace_set_domains", Args: map[string]any{"domains": append([]any{}, domains...)}})
	}
	member := newID("m") + "@corp.example"
	stranger := newID("s") + "@corp.example"
	robot := newID("bot") + "@proj.iam.gserviceaccount.com"
	outsideRobot := newID("bot") + "@proj.iam.gserviceaccount.com"
	f.member(member)
	f.member(stranger)
	f.member(robot)
	if _, err := setDomains("corp.example"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.asHD(member, "corp.example", f.list()); err != nil {
		t.Fatal("listed domain refused", err)
	}
	for _, hd := range []string{"other.example", ""} {
		if _, err := f.asHD(member, hd, f.list()); !refusedWith(err, "allowed Google Workspace domains") {
			t.Fatalf("account from %q admitted: %v", hd, err)
		}
	}
	// A member refused by the allow-list writes no binding or event.
	if _, err := f.asHD(stranger, "other.example", f.list()); !refusedWith(err, "allowed Google Workspace domains") {
		t.Fatal(err)
	}
	if n := f.count(`SELECT COUNT(*) FROM principal_subjects WHERE principal=$1`, stranger) + f.count(`SELECT COUNT(*) FROM principal_events WHERE principal=$1`, stranger); n != 0 {
		t.Fatal("a domain-refused member wrote binding rows or events:", n)
	}
	if _, err := f.asHD(member, "corp.example", Request{Operation: "workspace_set_domains", Workspace: f.workspace, Args: map[string]any{"domains": []any{}}}); !refusedWith(err, "only the workspace owner") {
		t.Fatal("a member changed the allow-list", err)
	}
	// A service account never carries hd: a member one is admitted, one that
	// is not a member is still refused by membership.
	if _, err := f.asHD(robot, "", f.list()); err != nil {
		t.Fatal("member service account refused", err)
	}
	if _, err := f.asHD(outsideRobot, "", f.list()); !refusedWith(err, "membership") {
		t.Fatal("non-member service account admitted", err)
	}

	// resolve_project leaves out a workspace that refuses the caller: it
	// neither resolves nor makes another match ambiguous.
	open := newID("idws")
	remote := "https://git.example.test/" + newID("r") + ".git"
	if _, err := owner(Request{Operation: "workspace_init", Workspace: open, Args: map[string]any{"mode": "server"}}); err != nil {
		t.Fatal(err)
	}
	for _, w := range []string{f.workspace, open} {
		if _, err := owner(Request{Operation: "project_create", Workspace: w, Args: map[string]any{"slug": "shared", "git_remote": remote}}); err != nil {
			t.Fatal(err)
		}
		if _, err := owner(Request{Operation: "project_role_set", Workspace: w, Project: "shared", Args: map[string]any{"principal": member, "role": "member"}}); err != nil {
			t.Fatal(err)
		}
	}
	resolve := func(hd, workspace string) (map[string]any, error) {
		out, err := f.asHD(member, hd, Request{Operation: "resolve_project", Workspace: workspace, Args: map[string]any{"git_remote": remote}})
		m, _ := out.Data.(map[string]any)
		return m, err
	}
	if _, err := resolve("corp.example", ""); err == nil || !strings.Contains(err.Error(), "ambiguous") {
		t.Fatal("an admitted member should see both matches", err)
	}
	if _, err := resolve("other.example", f.workspace); err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatal("a refused workspace resolved", err)
	}
	got, err := resolve("other.example", "")
	if err != nil || got["workspace_id"] == nil {
		t.Fatal("the refused workspace made the open one ambiguous", got, err)
	}

	// Break-glass: the owner's account moved to another domain, so the list
	// refuses the owner; setting domains is still allowed to repair it.
	moved := func(r Request) (Result, error) {
		r.Workspace = f.workspace
		return f.s.ExecuteAsIdentity(ctx, Identity{Principal: f.owner, Issuer: GoogleIssuer, Subject: "sub-owner", HostedDomain: "new.example"}, r)
	}
	if _, err = moved(f.list()); !refusedWith(err, "allowed Google Workspace domains") {
		t.Fatal("owner from another domain admitted", err)
	}
	if _, err = moved(Request{Operation: "workspace_set_domains", Args: map[string]any{"domains": []any{"new.example"}}}); err != nil {
		t.Fatal("owner could not repair the allow-list", err)
	}
	if _, err = moved(f.list()); err != nil {
		t.Fatal(err)
	}

	// A stored null is not "off": it admits no one.
	if _, err = f.s.db.ExecContext(ctx, `UPDATE workspaces SET allowed_domains='null' WHERE name=$1`, f.workspace); err != nil {
		t.Fatal(err)
	}
	if _, err = f.asHD(member, "corp.example", f.list()); !refusedWith(err, "allowed Google Workspace domains") {
		t.Fatal("a null allow-list admitted a member", err)
	}
	operator, err := OpenPostgresOperator(ctx, os.Getenv("CLAWBRIDGE_TEST_POSTGRES_DSN"))
	if err != nil {
		t.Fatal(err)
	}
	defer operator.Close()
	if _, err = operator.OperatorClearWorkspaceDomains(ctx, f.workspace); err != nil {
		t.Fatal(err)
	}
	if _, err = f.asHD(member, "", f.list()); err != nil {
		t.Fatal("operator clear did not reopen the workspace", err)
	}
}

// A write that waited on the workspace lock while the allow-list was
// narrowed sees the new list, not the one it read before the lock.
func TestPostgresDomainRestrictionAppliesToWritesWaitingOnTheLock(t *testing.T) {
	f := newPGIdentityFixture(t)
	ctx := context.Background()
	member := newID("m") + "@corp.example"
	f.member(member)
	if _, err := f.asHD(member, "corp.example", f.list()); err != nil {
		t.Fatal(err)
	}
	var wid string
	if err := f.s.db.QueryRowContext(ctx, `SELECT id FROM workspaces WHERE name=$1`, f.workspace).Scan(&wid); err != nil {
		t.Fatal(err)
	}
	setter, err := f.s.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer setter.Rollback()
	if _, err = setter.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, "workspace:"+wid); err != nil {
		t.Fatal(err)
	}
	if _, err = setter.ExecContext(ctx, `UPDATE workspaces SET allowed_domains='["other.example"]' WHERE id=$1`, wid); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := f.asHD(member, "corp.example", Request{Operation: "register_session", Workspace: f.workspace, Project: "p", Args: map[string]any{}, Session: "late"})
		done <- err
	}()
	// Let the write reach the lock, then record the restriction.
	waitForLockWaiter(t, f.s, wid)
	if err = setter.Commit(); err != nil {
		t.Fatal(err)
	}
	if err = <-done; !refusedWith(err, "allowed Google Workspace domains") {
		t.Fatal("a write that waited on the lock committed under the old allow-list", err)
	}
}

// waitForLockWaiter returns once a session waits on this workspace's
// advisory lock, so the test orders the write after the setter's lock.
func waitForLockWaiter(t *testing.T, s *Store, wid string) {
	t.Helper()
	for range 200 {
		var waiting int
		if err := s.db.QueryRow(`WITH k AS (SELECT hashtextextended($1,0) AS key) SELECT COUNT(*) FROM pg_locks l, k WHERE l.locktype='advisory' AND NOT l.granted AND l.database=(SELECT oid FROM pg_database WHERE datname=current_database()) AND l.classid::bigint=((k.key>>32)&4294967295) AND l.objid::bigint=(k.key&4294967295) AND l.objsubid=1`, "workspace:"+wid).Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		if waiting > 0 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("the write never waited on the workspace lock")
}
