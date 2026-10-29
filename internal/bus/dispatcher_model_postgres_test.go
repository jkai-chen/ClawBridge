package bus

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

// On the hosted authority: only a project owner sets up a model key, which
// is the caller's own; a project owner by role has its projects; one
// owner's credential never yields another's key or projects; key delete
// revokes the credential and its tokens.
func TestPostgresDispatcherModelFlow(t *testing.T) {
	f := newPGIdentityFixture(t)
	ctx := context.Background()
	seal, _ := NewModelSeal(bytes.Repeat([]byte{5}, 32), nil)
	f.s.SetModelSeal(seal)
	_, launcher, err := f.s.OperatorLauncherTokenCreate(ctx)
	if err != nil {
		t.Fatal(err)
	}
	args := func(key string) map[string]any {
		return map[string]any{"provider_id": "anthropic", "api": "anthropic-messages", "base_url": "https://api.anthropic.com", "model": "claude-test", "key": key, "caps": map[string]any{"daily_tokens": 500000}, "egress_text_version": "v2", "egress_confirmed": true}
	}
	setup := func(principal, subject, key string) (Result, error) {
		return f.as(principal, subject, Request{Operation: "dispatcher_model_setup", Workspace: f.workspace, Project: "p", Args: args(key)})
	}
	member := newID("m") + "@identity-test.example"
	f.member(member)
	if _, err = setup(member, "sub-member", "sk-member-key-000000"); err == nil || !strings.Contains(err.Error(), "owner") {
		t.Fatal("a member set a model key:", err)
	}
	lead := newID("o") + "@identity-test.example"
	f.must(f.owner, "sub-owner", Request{Operation: "project_role_set", Workspace: f.workspace, Project: "p", Args: map[string]any{"principal": lead, "role": "owner"}})
	ownerKey, leadKey := "sk-owner-key-1111111111111111", "sk-lead-key-22222222222222222"
	if _, err = setup(f.owner, "sub-owner", ownerKey); err != nil {
		t.Fatal(err)
	}
	if _, err = setup(lead, "sub-lead", leadKey); err != nil {
		t.Fatal(err)
	}
	var pid string
	if err = f.s.db.QueryRowContext(ctx, `SELECT p.id FROM projects p JOIN workspaces w ON w.id=p.workspace_id WHERE w.name=$1`, f.workspace).Scan(&pid); err != nil {
		t.Fatal(err)
	}
	if _, err = f.s.db.ExecContext(ctx, `INSERT INTO dispatcher_policies(project_id,revision,enabled,mode,domains,updated_at,updated_by_principal) VALUES($1,1,1,'model','{}','2026-01-01T00:00:00Z','x')`, pid); err != nil {
		t.Fatal(err)
	}
	// M2: doctor's model fields are a pure read, for a project owner only,
	// and list every key-holder who owns the project, the owner by role too.
	doctor := Request{Operation: "dispatcher_model_doctor", Workspace: f.workspace, Project: "p", Args: map[string]any{}}
	if _, err = f.as(member, "sub-member", doctor); err == nil || !strings.Contains(err.Error(), "owner") {
		t.Fatal("a member read doctor's model fields:", err)
	}
	read, err := f.as(lead, "sub-lead", doctor)
	if err != nil {
		t.Fatal(err)
	}
	holders := read.Data.(map[string]any)["holders"].([]any)
	if len(holders) != 2 || read.Data.(map[string]any)["model_on"] != true {
		t.Fatal("doctor's holders:", read.Data)
	}
	for _, raw := range holders {
		h := raw.(map[string]any)
		if h["key"] != "pending_test" || h["you"] != (h["principal"] == lead) || h["principal"] != lead && h["principal"] != f.owner {
			t.Fatal("doctor's holder:", h)
		}
	}
	refs := map[string]string{}
	for _, principal := range []string{f.owner, lead} {
		var ref string
		if err = f.s.db.QueryRowContext(ctx, `SELECT ref FROM dispatcher_owners WHERE owner_principal=$1`, principal).Scan(&ref); err != nil {
			t.Fatal(err)
		}
		refs[principal] = ref
	}
	wanted, err := f.s.LauncherWanted(ctx, launcher)
	if err != nil {
		t.Fatal(err)
	}
	listed := map[string]bool{}
	for _, w := range wanted {
		listed[w.OwnerRef] = true
	}
	if !listed[refs[f.owner]] || !listed[refs[lead]] {
		t.Fatal("both owners' pending keys want an agent:", wanted, refs)
	}
	configs := map[string]AgentConfig{}
	creds := map[string]string{}
	for principal, ref := range refs {
		cred, _, err := f.s.LauncherCredential(ctx, launcher, ref)
		if err != nil {
			t.Fatal(err)
		}
		cfg, err := f.s.RedeemAgentCredential(ctx, cred, "i-"+ref, "10.0.0.9")
		if err != nil {
			t.Fatal(err)
		}
		configs[principal], creds[principal] = cfg, cred
	}
	if configs[f.owner].Provider.Key != ownerKey || configs[lead].Provider.Key != leadKey {
		t.Fatal("a credential yielded another owner's key")
	}
	// The workspace owner and the owner by role both own the project.
	for principal, cfg := range configs {
		if len(cfg.Projects) != 1 || cfg.Projects[0].ProjectID != pid {
			t.Fatal(principal, cfg.Projects)
		}
	}
	if _, err = f.s.RedeemAgentCredential(ctx, creds[lead], "again", "10.0.0.9"); !errors.Is(err, ErrModelUnauthorized) {
		t.Fatal("redeemed twice:", err)
	}
	if _, err = f.s.RenewAgentTokens(ctx, creds[lead], configs[f.owner].BindingSecret); !errors.Is(err, ErrModelUnauthorized) {
		t.Fatal("another credential's binding secret renewed:", err)
	}
	// The first flag, second_redemption, is kept over wrong_binding.
	var flagged string
	if err = f.s.db.QueryRowContext(ctx, `SELECT flagged FROM dispatcher_agent_credentials WHERE owner_principal=$1 AND revoked_at IS NULL`, lead).Scan(&flagged); err != nil || flagged != "second_redemption" {
		t.Fatal(flagged, err)
	}
	if err = f.s.AgentTestResult(ctx, creds[f.owner], configs[f.owner].BindingSecret, configs[f.owner].ConfigRevision, "active", ""); err != nil {
		t.Fatal(err)
	}
	if err = f.s.AgentStatus(ctx, creds[f.owner], configs[f.owner].BindingSecret, "ok"); err != nil {
		t.Fatal(err)
	}
	if err = f.s.LauncherReport(ctx, launcher, refs[lead], "agent_crashloop"); err != nil {
		t.Fatal(err)
	}
	f.must(lead, "sub-lead", Request{Operation: "dispatcher_model_key_delete", Workspace: f.workspace, Project: "p", Args: map[string]any{}})
	if _, err = f.s.RenewAgentTokens(ctx, creds[lead], configs[lead].BindingSecret); !errors.Is(err, ErrModelUnauthorized) {
		t.Fatal("a deleted key's credential renewed:", err)
	}
	if tokens, err := f.s.RenewAgentTokens(ctx, creds[f.owner], configs[f.owner].BindingSecret); err != nil || len(tokens) != 1 {
		t.Fatal("the other owner was touched:", tokens, err)
	}
	var history string
	if err = f.s.db.QueryRowContext(ctx, `SELECT COALESCE(string_agg(details,' '),'') FROM history WHERE entity='dispatcher_model_key'`).Scan(&history); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{ownerKey, leadKey} {
		if strings.Contains(history, key[:12]) {
			t.Fatal("a key is in history")
		}
	}
}

// R100-H1, M4 on the hosted authority: two concurrent redemptions of one
// credential give the key once and flag the other; two concurrent mints
// leave one live credential.
func TestPostgresDispatcherModelConcurrency(t *testing.T) {
	f := newPGIdentityFixture(t)
	ctx := context.Background()
	seal, _ := NewModelSeal(bytes.Repeat([]byte{6}, 32), nil)
	f.s.SetModelSeal(seal)
	_, launcher, err := f.s.OperatorLauncherTokenCreate(ctx)
	if err != nil {
		t.Fatal(err)
	}
	f.must(f.owner, "sub-owner", Request{Operation: "dispatcher_model_setup", Workspace: f.workspace, Project: "p", Args: map[string]any{"provider_id": "anthropic", "api": "anthropic-messages", "base_url": "https://api.anthropic.com", "model": "claude-test", "key": "sk-concurrent-key-0000000000", "caps": map[string]any{"daily_tokens": 500000}, "egress_text_version": "v2", "egress_confirmed": true}})
	launchers := [2]string{launcher}
	if _, launchers[1], err = f.s.OperatorLauncherTokenCreate(ctx); err != nil {
		t.Fatal(err)
	}
	var ref string
	if err = f.s.db.QueryRowContext(ctx, `SELECT ref FROM dispatcher_owners WHERE owner_principal=$1`, f.owner).Scan(&ref); err != nil {
		t.Fatal(err)
	}
	both := func(fn func(i int)) {
		var wg sync.WaitGroup
		start := make(chan struct{})
		for i := range 2 {
			wg.Add(1)
			go func() { defer wg.Done(); <-start; fn(i) }()
		}
		close(start)
		wg.Wait()
	}
	for round := range 10 {
		// Each round mints three times: the earlier mints are moved back
		// two hours so maxMintsPerHour never refuses one here.
		if _, err = f.s.db.ExecContext(ctx, `UPDATE dispatcher_agent_credentials SET created_at=$1 WHERE owner_principal=$2`, layout(time.Now().UTC().Add(-2*time.Hour)), f.owner); err != nil {
			t.Fatal(err)
		}
		// Two launchers: one launcher token's own row would serialize them.
		both(func(i int) { _, _, _ = f.s.LauncherCredential(ctx, launchers[i], ref) })
		var live int
		if err = f.s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM dispatcher_agent_credentials WHERE owner_principal=$1 AND revoked_at IS NULL`, f.owner).Scan(&live); err != nil || live != 1 {
			t.Fatal("round", round, "live credentials:", live, err)
		}
		cred, _, err := f.s.LauncherCredential(ctx, launcher, ref)
		if err != nil {
			t.Fatal(err)
		}
		var keys [2]string
		both(func(i int) {
			if cfg, err := f.s.RedeemAgentCredential(ctx, cred, "i-concurrent", "10.0.0.9"); err == nil {
				keys[i] = cfg.Provider.Key
			}
		})
		if (keys[0] == "") == (keys[1] == "") {
			t.Fatal("round", round, "two concurrent redemptions, keys given:", keys[0] != "", keys[1] != "")
		}
		var flagged string
		if err = f.s.db.QueryRowContext(ctx, `SELECT flagged FROM dispatcher_agent_credentials WHERE owner_principal=$1 AND revoked_at IS NULL`, f.owner).Scan(&flagged); err != nil || flagged != "second_redemption" {
			t.Fatal("round", round, "the second redemption was not flagged:", flagged, err)
		}
	}
}

// d5 on the hosted authority: two key-holders' agents polling at once
// lease an open case exactly once.
func TestPostgresModelCaseLeaseOnce(t *testing.T) {
	f := newPGIdentityFixture(t)
	ctx := context.Background()
	var pid string
	if err := f.s.db.QueryRowContext(ctx, `SELECT p.id FROM projects p JOIN workspaces w ON w.id=p.workspace_id WHERE w.name=$1`, f.workspace).Scan(&pid); err != nil {
		t.Fatal(err)
	}
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := f.s.db.ExecContext(ctx, q, args...); err != nil {
			t.Fatal(err)
		}
	}
	exec(`INSERT INTO dispatcher_policies(project_id,revision,enabled,mode,domains,updated_at,updated_by_principal) VALUES($1,1,1,'model','{"domains":[]}','2026-01-01T00:00:00Z','x')`, pid)
	now := layout(time.Now())
	var tokens [2]string
	for i, holder := range []string{"a-" + newID("h") + "@identity-test.example", "b-" + newID("h") + "@identity-test.example"} {
		exec(`INSERT INTO dispatcher_model_keys(owner_principal,state,provider_id,api,base_url,model,ciphertext,nonce,key_fingerprint,config_revision,caps,egress_text_version,egress_confirmed_at,created_by,created_at) VALUES($1,'active','anthropic','anthropic-messages','https://api.anthropic.com','claude-test','x','y','-key',1,'{"daily_tokens":500000,"runs_per_hour":100,"runs_per_day":1000}','v2','2026-01-01T00:00:00Z',$1,'2026-01-01T00:00:00Z')`, holder)
		token, err := randomToken("cbm_")
		if err != nil {
			t.Fatal(err)
		}
		tokens[i] = token
		exec(`INSERT INTO dispatcher_model_tokens(id,project_id,token_sha256,created_by_principal,created_at,expires_at,last_used_at) VALUES($1,$2,$3,$4,$5,'2999-01-01T00:00:00.000000000Z',$5)`, newID("mtok_"), pid, tokenDigest(tokens[i]), holder, now)
	}
	// Each round's case is for a live claim: next serves no other.
	registered := f.must(f.owner, "sub-owner", Request{Operation: "register_session", Workspace: f.workspace, Project: "p", Session: "eng", Args: map[string]any{"instance": "eng-i"}})
	instance := registered.Data.(map[string]any)["instance"].(string)
	for round := range 10 {
		id := newID("case_")
		f.must(f.owner, "sub-owner", Request{Operation: "assignment_propose", Workspace: f.workspace, Project: "p", Session: "eng", Args: map[string]any{"instance": instance, "issue_uuid": "aaaa1111-1111-4111-8111-11111111111" + string(rune('0'+round)), "issue_identifier": "FIX-1"}})
		var claim, revision string
		if err := f.s.db.QueryRowContext(ctx, `SELECT id,revision FROM session_assignments WHERE project_id=$1 AND state='proposed' ORDER BY proposed_at DESC,id LIMIT 1`, pid).Scan(&claim, &revision); err != nil {
			t.Fatal(err)
		}
		exec(`INSERT INTO dispatcher_cases(id,project_id,kind,subject_type,subject_id,subject_revision,permitted,facts,deadline,created_at) VALUES($1,$2,'decide','proposal',$3,$4,'["escalate"]','{}','2999-01-01T00:00:00.000000000Z',$5)`, id, pid, claim, revision, now)
		var served [2]bool
		var wg sync.WaitGroup
		start := make(chan struct{})
		for i := range 2 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				if n, err := f.s.ModelNextCase(ctx, tokens[i], 10); err == nil && n.Case != nil {
					served[i] = true
				}
			}()
		}
		close(start)
		wg.Wait()
		if served[0] == served[1] {
			t.Fatal("round", round, "served:", served)
		}
		exec(`UPDATE dispatcher_cases SET closed_at=$1,outcome='abstain' WHERE id=$2`, now, id)
	}
}

// R142-L6: a key-holder who is no longer an owner can still delete their
// own key, as a member or a reader, and only their own; setup, status and
// doctor stay the owner's.
func TestPostgresModelKeyDeleteOwn(t *testing.T) {
	f := newPGIdentityFixture(t)
	ctx := context.Background()
	seal, _ := NewModelSeal(bytes.Repeat([]byte{6}, 32), nil)
	f.s.SetModelSeal(seal)
	setup := func(principal, subject string) (Result, error) {
		return f.as(principal, subject, Request{Operation: "dispatcher_model_setup", Workspace: f.workspace, Project: "p", Args: map[string]any{"provider_id": "anthropic", "api": "anthropic-messages", "base_url": "https://api.anthropic.com", "model": "claude-test", "key": "sk-" + subject + "-000000000000", "caps": map[string]any{"daily_tokens": 500000}, "egress_text_version": "v2", "egress_confirmed": true}})
	}
	keys := func(principal string) int {
		var n int
		if err := f.s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM dispatcher_model_keys WHERE owner_principal=$1`, principal).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	former := newID("o") + "@identity-test.example"
	f.must(f.owner, "sub-owner", Request{Operation: "project_role_set", Workspace: f.workspace, Project: "p", Args: map[string]any{"principal": former, "role": "owner"}})
	for _, who := range []struct{ principal, subject string }{{f.owner, "sub-owner"}, {former, "sub-former"}} {
		if _, err := setup(who.principal, who.subject); err != nil {
			t.Fatal(err)
		}
	}
	remove := Request{Operation: "dispatcher_model_key_delete", Workspace: f.workspace, Project: "p", Args: map[string]any{}}
	for _, role := range []string{"member", "reader"} {
		f.must(f.owner, "sub-owner", Request{Operation: "project_role_set", Workspace: f.workspace, Project: "p", Args: map[string]any{"principal": former, "role": role}})
		if _, err := setup(former, "sub-former"); err == nil || !strings.Contains(err.Error(), "owner") {
			t.Fatal("a", role, "set a model key:", err)
		}
		if _, err := f.as(former, "sub-former", Request{Operation: "dispatcher_model_status", Workspace: f.workspace, Project: "p", Args: map[string]any{"config_revision": 1}}); err == nil || !strings.Contains(err.Error(), "owner") {
			t.Fatal("a", role, "read the setup status:", err)
		}
		out, err := f.as(former, "sub-former", remove)
		if err != nil {
			t.Fatal("a", role, "couldn't delete their own key:", err)
		}
		if role == "member" && out.Data.(map[string]any)["deleted"] != true || keys(former) != 0 {
			t.Fatal("as a", role, out.Data, keys(former))
		}
		if keys(f.owner) != 1 {
			t.Fatal("a", role, "deleted another principal's key")
		}
	}
}
