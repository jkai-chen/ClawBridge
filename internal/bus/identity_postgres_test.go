package bus

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"sort"
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
	// lastJoined is the key of a device a test joined, when it needs it later.
	lastJoined string
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
	subject := "sub-" + principal
	if principal == f.owner {
		subject = "sub-owner"
	}
	return f.s.ExecuteAsIdentity(context.Background(), Identity{Principal: principal, Issuer: GoogleIssuer, Subject: subject, HostedDomain: hd}, r)
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
		if _, err := f.asHD(member, hd, f.list()); !refusedWith(err, "in its allowed domains") {
			t.Fatalf("account from %q admitted: %v", hd, err)
		}
	}
	// A member refused by the allow-list writes no binding or event.
	if _, err := f.asHD(stranger, "other.example", f.list()); !refusedWith(err, "in its allowed domains") {
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
	// The only match is refused by its allow-list: the member learns it is
	// refused (forbidden), not that nothing matched.
	if _, err := resolve("other.example", f.workspace); !refusedWith(err, "in its allowed domains") {
		t.Fatal("a refused workspace resolved, or its refusal read as not found", err)
	}
	if n := f.count(`SELECT COUNT(*) FROM principal_subjects WHERE principal=$1 AND subject='sub-'||$1`, member); n != 1 {
		t.Fatal("binding changed by a refused resolve", n)
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
	if _, err = moved(f.list()); !refusedWith(err, "in its allowed domains") {
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
	if _, err = f.asHD(member, "corp.example", f.list()); !refusedWith(err, "in its allowed domains") {
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
	if err = <-done; !refusedWith(err, "in its allowed domains") {
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

// The server owner cannot release their own address over the API; their
// recovery is the operator command on the host.
func TestPostgresServerOwnerCannotUnbindThemselves(t *testing.T) {
	f := newPGIdentityFixture(t)
	_, err := f.as(f.owner, "sub-owner", Request{Operation: "principal_unbind", Args: map[string]any{"principal": strings.ToUpper(f.owner)}})
	var coded interface{ Code() string }
	if err == nil || !errors.As(err, &coded) || coded.Code() != "invalid" || !strings.Contains(err.Error(), "clawbridge operator principal-unbind") {
		t.Fatal("self-unbind not refused with the operator path", err)
	}
	f.must(f.owner, "sub-owner", f.list())
}

// Operator commands never migrate: they open only a schema at exactly this
// binary's version. Each case uses its own Postgres schema via search_path.
func TestPostgresOperatorRefusesAnyOtherSchemaVersion(t *testing.T) {
	dsn := os.Getenv("CLAWBRIDGE_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("reviewer supplies disposable Postgres DSN")
	}
	ctx := context.Background()
	admin, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	// A URL DSN takes search_path as a query parameter; a key=value DSN
	// takes it as one more pair.
	withSchema := func(schema string) string {
		switch {
		case !strings.Contains(dsn, "://"):
			return dsn + " search_path=" + schema
		case strings.Contains(dsn, "?"):
			return dsn + "&search_path=" + schema
		}
		return dsn + "?search_path=" + schema
	}
	for _, c := range []struct {
		version int // 0: no clawbridge_schema table at all
		want    string
	}{{0, "cannot read"}, {postgresSchemaVersion - 1, "run the operator command from the server's own release"}, {postgresSchemaVersion + 1, "run the operator command from the server's own release"}, {postgresSchemaVersion, ""}} {
		schema := "op_" + strings.ToLower(newID("v"))
		if _, err = admin.ExecContext(ctx, `CREATE SCHEMA `+schema); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { admin.Exec(`DROP SCHEMA ` + schema + ` CASCADE`) })
		if c.version > 0 {
			if _, err = admin.ExecContext(ctx, `CREATE TABLE `+schema+`.clawbridge_schema(version INTEGER PRIMARY KEY)`); err != nil {
				t.Fatal(err)
			}
			if _, err = admin.ExecContext(ctx, `INSERT INTO `+schema+`.clawbridge_schema VALUES($1)`, c.version); err != nil {
				t.Fatal(err)
			}
		}
		s, err := OpenPostgresOperator(ctx, withSchema(schema))
		if c.want == "" {
			if err != nil {
				t.Fatal("current schema refused", err)
			}
			s.Close()
		} else if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Fatalf("version %d: %v", c.version, err)
		}
		// Nothing was migrated into the schema.
		var tables int
		if err = admin.QueryRowContext(ctx, `SELECT COUNT(*) FROM information_schema.tables WHERE table_schema=$1`, schema).Scan(&tables); err != nil || (c.version == 0 && tables != 0) || (c.version > 0 && tables != 1) {
			t.Fatalf("version %d: operator open created tables: %d %v", c.version, tables, err)
		}
	}
}

// Every hosted operation requires the account bound to its principal. The
// list is operationArgs itself, so an operation added later that is
// dispatched without the binding check fails here. The caller is the server
// owner (who passes every access check) signing in with another account; the
// arguments are the minimum that passes argument validation.
func TestPostgresEveryHostedOperationRequiresTheBoundAccount(t *testing.T) {
	f := newPGIdentityFixture(t)
	minimal := map[string]map[string]any{
		"workspace_init":      {"mode": "server"},
		"principal_unbind":    {"principal": "someone@identity-test.example"},
		"taskboard_set":       taskboardArgs(0),
		"taskboard_set_v2":    taskboardArgsV2(0, projectOne),
		"taskboard_clear":     {"expected_revision": 0},
		"taskboard_clear_v2":  {"expected_revision": 0},
		"taskboard_names_set": {"expected_revision": 0},
	}
	ops := make([]string, 0, len(operationArgs))
	for op := range operationArgs {
		ops = append(ops, op)
	}
	sort.Strings(ops)
	for _, op := range ops {
		args := map[string]any{}
		for k, v := range minimal[op] {
			args[k] = v
		}
		_, err := f.as(f.owner, "sub-intruder", Request{Operation: op, Workspace: f.workspace, Project: "p", Args: args})
		if !refusedWith(err, "not the one bound") {
			t.Errorf("%s ran without the bound account: %v", op, err)
		}
	}
}

// The operator's clear gives up behind a held workspace lock with a clear
// error instead of hanging, and names a workspace that does not exist.
func TestPostgresOperatorClearDomainsIsBoundedAndPrecise(t *testing.T) {
	f := newPGIdentityFixture(t)
	ctx := context.Background()
	operator, err := OpenPostgresOperator(ctx, os.Getenv("CLAWBRIDGE_TEST_POSTGRES_DSN"))
	if err != nil {
		t.Fatal(err)
	}
	defer operator.Close()
	if _, err = operator.OperatorClearWorkspaceDomains(ctx, newID("absent")); err == nil || err.Error() != "workspace not found" {
		t.Fatal(err)
	}
	var wid string
	if err = f.s.db.QueryRowContext(ctx, `SELECT id FROM workspaces WHERE name=$1`, f.workspace).Scan(&wid); err != nil {
		t.Fatal(err)
	}
	holder, err := f.s.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Rollback()
	if _, err = holder.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, "workspace:"+wid); err != nil {
		t.Fatal(err)
	}
	// The test bounds its own wait, so a clear that would hang fails here
	// instead of at the package timeout.
	bounded, cancel := context.WithTimeout(ctx, 12*time.Second)
	defer cancel()
	start := time.Now()
	_, err = operator.OperatorClearWorkspaceDomains(bounded, f.workspace)
	if err == nil || !strings.Contains(err.Error(), "busy") || time.Since(start) > 10*time.Second {
		t.Fatal("operator clear behind a held lock:", err, time.Since(start))
	}
}

func (f *pgIdentityFixture) session(principal, hd, name string) string {
	f.t.Helper()
	out, err := f.asHD(principal, hd, Request{Operation: "register_session", Workspace: f.workspace, Project: "p", Session: name, Args: map[string]any{}})
	if err != nil {
		f.t.Fatal(err)
	}
	return out.Data.(map[string]any)["instance"].(string)
}

// The allow-list is the owner's configuration: only the owner's whoami shows
// it, and no snapshot (which feeds the offline mirror) carries it.
func TestPostgresAllowListIsShownOnlyToTheWorkspaceOwner(t *testing.T) {
	f := newPGIdentityFixture(t)
	member := newID("m") + "@corp.example"
	f.member(member)
	if _, err := f.asHD(f.owner, "corp.example", Request{Operation: "workspace_set_domains", Workspace: f.workspace, Args: map[string]any{"domains": []any{"corp.example"}}}); err != nil {
		t.Fatal(err)
	}
	shows := func(principal, op, session, instance string) bool {
		t.Helper()
		args := map[string]any{}
		if instance != "" {
			args["instance"] = instance
		}
		out, err := f.asHD(principal, "corp.example", Request{Operation: op, Workspace: f.workspace, Project: "p", Session: session, Args: args})
		if err != nil {
			t.Fatal(op, err)
		}
		_, ok := out.Data.(map[string]any)["workspace"].(map[string]any)["allowed_domains"]
		return ok
	}
	if !shows(f.owner, "whoami", "", "") || shows(member, "whoami", "", "") {
		t.Fatal("whoami shows the allow-list to the wrong principal")
	}
	for _, p := range []string{f.owner, member} {
		name := newID("s")
		if shows(p, "snapshot", name, f.session(p, "corp.example", name)) {
			t.Fatal("a snapshot carried the allow-list for", p)
		}
	}
}

// resolve_project with a session writes (it touches the session), so it reads
// the allow-list under the workspace lock: narrowed while it waits, it is
// refused and touches nothing.
func TestPostgresResolveWithASessionSeesANarrowedAllowList(t *testing.T) {
	f := newPGIdentityFixture(t)
	ctx := context.Background()
	member := newID("m") + "@corp.example"
	f.member(member)
	instance := f.session(member, "corp.example", "rs")
	var wid string
	if err := f.s.db.QueryRowContext(ctx, `SELECT id FROM workspaces WHERE name=$1`, f.workspace).Scan(&wid); err != nil {
		t.Fatal(err)
	}
	seen := func() string {
		var at string
		if err := f.s.db.QueryRowContext(ctx, `SELECT last_seen FROM sessions WHERE owner_principal=$1`, member).Scan(&at); err != nil {
			t.Fatal(err)
		}
		return at
	}
	before := seen()
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
		_, err := f.asHD(member, "corp.example", Request{Operation: "resolve_project", Workspace: f.workspace, Project: "p", Session: "rs", Args: map[string]any{"instance": instance}})
		done <- err
	}()
	waitForLockWaiter(t, f.s, wid)
	if err = setter.Commit(); err != nil {
		t.Fatal(err)
	}
	if err = <-done; !refusedWith(err, "in its allowed domains") {
		t.Fatal("resolve with a session passed a restriction recorded while it waited", err)
	}
	if seen() != before {
		t.Fatal("a refused resolve touched the session")
	}
}

// A heartbeat is a write after the read commits: an allow-list narrowed, or
// the session retired, while the read was in flight skips the beat.
func TestPostgresHeartbeatRechecksAccessBeforeItWrites(t *testing.T) {
	if os.Getenv("CLAWBRIDGE_TEST_POSTGRES_DSN") == "" {
		t.Skip("reviewer supplies disposable Postgres DSN")
	}
	ctx := context.Background()
	t.Cleanup(func() { beforeBeat.Store(nil) })
	for name, change := range map[string]string{
		"none":               "",
		"domain narrowed":    `UPDATE workspaces SET allowed_domains='["other.example"]' WHERE name=$1`,
		"session retired":    `UPDATE sessions SET status='retired' WHERE project_id=(SELECT p.id FROM projects p JOIN workspaces w ON w.id=p.workspace_id WHERE w.name=$1) AND name='hb'`,
		"membership revoked": `DELETE FROM members WHERE workspace_id=(SELECT id FROM workspaces WHERE name=$1) AND principal<>(SELECT owner_principal FROM workspaces WHERE name=$1)`,
	} {
		t.Run(name, func(t *testing.T) {
			g := newPGIdentityFixture(t)
			member := newID("m") + "@corp.example"
			g.member(member)
			instance := g.session(member, "corp.example", "hb")
			lastSeen := func() string {
				var at string
				if err := g.s.db.QueryRowContext(ctx, `SELECT last_seen FROM sessions WHERE owner_principal=$1`, member).Scan(&at); err != nil {
					t.Fatal(err)
				}
				return at
			}
			before := lastSeen()
			hook := func() {
				if change != "" {
					if _, err := g.s.db.ExecContext(ctx, change, g.workspace); err != nil {
						t.Error(err)
					}
				}
			}
			beforeBeat.Store(&hook)
			_, err := g.asHD(member, "corp.example", Request{Operation: "whoami", Workspace: g.workspace, Project: "p", Session: "hb", Args: map[string]any{"instance": instance}})
			beforeBeat.Store(nil)
			if err != nil {
				t.Fatal("the read itself was admitted and must succeed", err)
			}
			if moved := lastSeen() != before; moved != (change == "") {
				t.Fatalf("beat wrote=%v after %s", moved, name)
			}
		})
	}
}

// An unreachable Postgres is "unavailable", never "not the server's
// database": only a missing schema table means that. Needs no DSN.
func TestOperatorOpenTellsUnreachableFromNotTheServersDatabase(t *testing.T) {
	_, err := OpenPostgresOperator(context.Background(), "postgres://nobody@127.0.0.1:1/none?sslmode=disable&connect_timeout=2")
	if err == nil || !strings.Contains(err.Error(), "Postgres unavailable") || strings.Contains(err.Error(), "server's database") {
		t.Fatal(err)
	}
}

// resolve_project with a retired session's instance does not move its
// last_seen, like the heartbeat.
func TestPostgresResolveDoesNotTouchARetiredSession(t *testing.T) {
	f := newPGIdentityFixture(t)
	ctx := context.Background()
	member := newID("m") + "@corp.example"
	f.member(member)
	instance := f.session(member, "corp.example", "gone")
	if _, err := f.s.db.ExecContext(ctx, `UPDATE sessions SET status='retired',last_seen='2000-01-01T00:00:00.000000000Z' WHERE owner_principal=$1`, member); err != nil {
		t.Fatal(err)
	}
	_, _ = f.asHD(member, "corp.example", Request{Operation: "resolve_project", Workspace: f.workspace, Project: "p", Session: "gone", Args: map[string]any{"instance": instance}})
	var at string
	if err := f.s.db.QueryRowContext(ctx, `SELECT last_seen FROM sessions WHERE owner_principal=$1`, member).Scan(&at); err != nil || at != "2000-01-01T00:00:00.000000000Z" {
		t.Fatal("a retired session was touched", at, err)
	}
}

// Every write re-checks membership, not only the allow-list, under the
// workspace lock: a member removed while the write waited is refused.
func TestPostgresWriteWaitingOnTheLockSeesARevokedMembership(t *testing.T) {
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
	remover, err := f.s.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer remover.Rollback()
	if _, err = remover.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, "workspace:"+wid); err != nil {
		t.Fatal(err)
	}
	if _, err = remover.ExecContext(ctx, `DELETE FROM members WHERE workspace_id=$1 AND principal=$2`, wid, member); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := f.asHD(member, "corp.example", Request{Operation: "register_session", Workspace: f.workspace, Project: "p", Session: "late", Args: map[string]any{}})
		done <- err
	}()
	waitForLockWaiter(t, f.s, wid)
	if err = remover.Commit(); err != nil {
		t.Fatal(err)
	}
	if err = <-done; !refusedWith(err, "membership") {
		t.Fatal("a write that waited on the lock committed for a removed member", err)
	}
}

// Binding times and their audit events come from the database clock, not this
// host's: with the host clock six hours behind, every stamp a bind, a refusal
// and an unbind write is still the server's now().
func TestPostgresBindingTimesUseTheDatabaseClock(t *testing.T) {
	f := newPGIdentityFixture(t)
	user := newID("u") + "@identity-test.example"
	f.member(user)
	hostClock = func() time.Time { return time.Now().Add(-6 * time.Hour) }
	t.Cleanup(func() { hostClock = time.Now })
	f.must(user, "sub-1", f.list())
	if _, err := f.as(user, "sub-2", f.list()); !refusedWith(err, "not the one bound") {
		t.Fatal("another account was accepted for a bound principal", err)
	}
	f.must(f.owner, "sub-owner", Request{Operation: "principal_unbind", Args: map[string]any{"principal": user}})
	ctx := context.Background()
	var dbNow time.Time
	if err := f.s.db.QueryRowContext(ctx, `SELECT now()`).Scan(&dbNow); err != nil {
		t.Fatal(err)
	}
	stamps := map[string]string{}
	var boundAt string
	if err := f.s.db.QueryRowContext(ctx, `SELECT bound_at FROM principal_subjects WHERE principal=$1`, user).Scan(&boundAt); err != nil {
		t.Fatal(err)
	}
	stamps["unbind bound_at"] = boundAt
	rows, err := f.s.db.QueryContext(ctx, `SELECT action,changed_at FROM principal_events WHERE principal=$1`, user)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var action, at string
		if err := rows.Scan(&action, &at); err != nil {
			t.Fatal(err)
		}
		stamps[action] = at
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	for _, action := range []string{"principal_bound", "principal_subject_mismatch", "principal_unbound"} {
		if _, ok := stamps[action]; !ok {
			t.Fatal("missing event", action, stamps)
		}
	}
	for what, raw := range stamps {
		at, err := time.Parse(timestampLayout, raw)
		if err != nil {
			t.Fatal(what, err)
		}
		if d := dbNow.Sub(at); d < 0 || d > time.Minute {
			t.Fatalf("%s stamped %s, %s from the database's now() %s", what, raw, d, dbNow.UTC().Format(timestampLayout))
		}
	}
}

// A member with no project yet (the bootstrap owner, or one waiting for a
// grant) still reads whoami for the workspace: who they are and whose
// workspace it is. A session still needs its project, and a stranger is
// refused.
func TestPostgresWhoamiWithoutAProject(t *testing.T) {
	f := newPGIdentityFixture(t)
	waiting := newID("w") + "@identity-test.example"
	if _, err := f.s.db.ExecContext(context.Background(), `INSERT INTO members(workspace_id,principal,added_by,added_at) SELECT id,$1,owner_principal,created_at FROM workspaces WHERE name=$2`, waiting, f.workspace); err != nil {
		t.Fatal(err)
	}
	subjects := map[string]string{f.owner: "sub-owner", waiting: "sub-waiting"}
	for _, who := range []string{f.owner, waiting} {
		out := f.must(who, subjects[who], Request{Operation: "whoami", Workspace: f.workspace, Args: map[string]any{}})
		d := out.Data.(map[string]any)
		w, _ := d["workspace"].(map[string]any)
		if d["principal"] != who || w["owner_principal"] != f.owner || d["project"] != nil && d["project"].(map[string]any) != nil || d["session"] != nil && d["session"].(map[string]any) != nil {
			t.Fatalf("%s: %+v", who, d)
		}
	}
	if _, err := f.as(waiting, "sub-waiting", Request{Operation: "whoami", Workspace: f.workspace, Session: "s", Args: map[string]any{"instance": "i"}}); err == nil {
		t.Fatal("a session's whoami without its project answered")
	}
	if _, err := f.as("stranger@identity-test.example", "sub-stranger", Request{Operation: "whoami", Workspace: f.workspace, Args: map[string]any{}}); err == nil {
		t.Fatal("a non-member's whoami answered")
	}
}
