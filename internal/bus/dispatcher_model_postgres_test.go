package bus

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
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
