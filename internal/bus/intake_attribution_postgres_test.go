package bus

import (
	"context"
	"strings"
	"testing"
	"time"
)

// On the hosted authority, trust add links a person by an email that
// exactly matches a project member (Q2), their own device write is credited
// to them, and a member whose role is revoked is credited nothing (A4).
func TestPostgresIntakeTrustLinksByEmail(t *testing.T) {
	f := newPGIdentityFixture(t)
	ctx := context.Background()
	member := newID("m") + "@identity-test.example"
	f.member(member)
	var pid string
	if err := f.s.db.QueryRowContext(ctx, `SELECT p.id FROM projects p JOIN workspaces w ON w.id=p.workspace_id WHERE w.name=$1 AND p.slug='p'`, f.workspace).Scan(&pid); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.db.ExecContext(ctx, `INSERT INTO taskboard_bindings(project_id,revision,mapping,updated_at) VALUES($1,1,'{}','2026-01-01T00:00:00Z')`, pid); err != nil {
		t.Fatal(err)
	}
	owner := func(a map[string]any) []map[string]any {
		t.Helper()
		out := f.must(f.owner, "sub-owner", Request{Operation: "intake_trust_add", Workspace: f.workspace, Project: "p", Args: a})
		return out.Data.([]map[string]any)
	}
	if list := owner(map[string]any{"linear_user_id": trustedUser, "link_email": "nobody@identity-test.example"}); list[0]["principal"] != nil {
		t.Fatal("an email no member has was linked:", list)
	}
	if list := owner(map[string]any{"linear_user_id": trustedUser, "link_email": strings.ToUpper(member)}); list[0]["principal"] != member {
		t.Fatal("the member's email did not link:", list)
	}
	write := AppWrite{Workspace: f.workspace, Project: "p", Issue: intakeIssue, Labels: []string{needsAgentID}, Identity: Identity{Principal: member, Issuer: DeviceIssuer, Subject: "dev_m"}}
	if err := f.s.RecordAppWrite(ctx, write); err != nil {
		t.Fatal(err)
	}
	credit := func() AttributionCredit {
		t.Helper()
		now := time.Now()
		c, err := f.s.IntakeAttribution(ctx, pid, intakeIssue, "label", needsAgentID, now.Add(-time.Minute), now.Add(time.Minute))
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	if c := credit(); c.Result != "credited" || c.Actor != trustedUser || c.Principal != member {
		t.Fatal(c)
	}
	f.must(f.owner, "sub-owner", Request{Operation: "project_role_revoke", Workspace: f.workspace, Project: "p", Args: map[string]any{"principal": member}})
	if c := credit(); c.Result != "attributed_untrusted" {
		t.Fatal("a revoked member was credited:", c)
	}
}
