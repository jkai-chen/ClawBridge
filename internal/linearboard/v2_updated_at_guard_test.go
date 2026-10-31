package linearboard

import (
	"context"
	"errors"
	"strings"
	"testing"

	"clawbridge/internal/bus"
	"clawbridge/internal/remote"
)

// The same issue mention as the member's read and as the app's read
// render it (FR29): synthetic text only.
const (
	memberMention = `See <issue id="00000000-0000-0000-0000-0000000000a1" href="https://linear.example.test/x/issue/T-1">T-1</issue> first.`
	appMention    = `See [T-1](https://linear.example.test/x/issue/T-1) first.`
	readAt        = "2026-10-31T09:05:12.340Z"
	laterAt       = "2026-10-31T09:06:00.000Z"
)

func guardIssueV2(p *providerV2, description, updatedAt string) {
	i := p.issues[issueID]
	i.Description, i.DescriptionKnown, i.UpdatedAt = description, true, updatedAt
	p.issues[issueID] = i
}

// guardPair is a client whose writes go to the app, and the hosted
// server's own board service that makes them as the app over its own
// fake: the member reads one rendering, the app the other.
type guardPair struct {
	client      *Service
	member, app *providerV2
	server      *appServer
	hosted      *Service
	hostedAuth  *authorityV2
	scope       bus.Request
	// before runs on the server just before the hosted service gets the
	// write: an edit between the client's read and the server's.
	before func()
}

func newGuardPair(t *testing.T, offers bool) *guardPair {
	t.Helper()
	g := &guardPair{}
	hosted, hostedAuth, app, _ := testV2(t)
	hosted.cfg.UpdatedAtGuard = true
	g.hosted, g.hostedAuth, g.app = hosted, hostedAuth, app
	g.server = &appServer{status: func() (bus.Result, error) {
		r, _ := appOn()
		if offers {
			r.Data.(map[string]any)["description_guard_updated_at"] = true
		}
		return r, nil
	}, write: func(r bus.Request) (bus.Result, error) {
		if g.before != nil {
			g.before()
		}
		args, _ := r.Args["args"].(map[string]any)
		operation, _ := r.Args["operation"].(string)
		data, e := g.hosted.ExecuteV2(context.Background(), bus.Request{Workspace: r.Workspace, Project: r.Project, Session: r.Session, Args: map[string]any{"instance": r.Args["instance"]}}, operation, args)
		var receipt *ReceiptError
		switch {
		case e == nil:
			return bus.Result{Source: "server", Data: map[string]any{"receipt": data}}, nil
		case data != nil && errors.As(e, &receipt):
			return bus.Result{Source: "server", Data: map[string]any{"receipt": data, "outcome": receipt.Outcome}}, nil
		}
		return bus.Result{}, &remote.HTTPError{Status: 400, Code: "invalid", Message: e.Error()}
	}}
	g.client, g.member, g.scope = routedV2(t, g.server)
	guardIssueV2(g.member, memberMention, readAt)
	guardIssueV2(g.app, appMention, readAt)
	return g
}

// read is the member's issue read an agent takes its hash from.
func (g *guardPair) read(t *testing.T) string {
	t.Helper()
	read, e := g.client.IssueV2(context.Background(), g.scope, issueID)
	if e != nil || read.DescriptionSHA256 != descriptionSHA256(memberMention) {
		t.Fatal("member read", read.DescriptionSHA256, e)
	}
	return read.DescriptionSHA256
}

func guardedUpdate(op, sum string) WriteInputV2 {
	return WriteInputV2{WriteInput: WriteInput{Operation: "update", OperationID: op, IssueID: issueID, Description: ptrV2("new body")}, ExpectedDescriptionSHA256: &sum}
}

func (g *guardPair) sentUpdatedAt(t *testing.T) (string, bool) {
	t.Helper()
	args := g.server.writes[len(g.server.writes)-1].Args["args"].(map[string]any)
	v, ok := args["expected_updated_at"]
	at, _ := v.(string)
	return at, ok
}

// An unchanged description with an issue mention, which the app reads
// differently from the member, passes the guard on a server that takes
// expected_updated_at; the client sends its own fresh read's updated_at.
func TestAppDescriptionGuardPassesAnUnchangedMentionedDescription(t *testing.T) {
	g := newGuardPair(t, true)
	sum := g.read(t)
	if _, e := g.client.WriteV2(context.Background(), g.scope, guardedUpdate("mention", sum)); e != nil || g.app.writes != 1 || g.member.writes != 0 {
		t.Fatal("an unchanged mentioned description was refused", e, g.app.writes)
	}
	if at, ok := g.sentUpdatedAt(t); !ok || at != readAt {
		t.Fatal("expected_updated_at not sent from the fresh read", at, ok)
	}
}

// A server that does not offer the guard never gets expected_updated_at:
// the hash goes alone and is checked as in 1.0, so the mentioned
// description is refused, failing safe.
func TestAppDescriptionGuardOldServerGetsTheHashAlone(t *testing.T) {
	g := newGuardPair(t, false)
	sum := g.read(t)
	reads := g.member.reads
	_, e := g.client.WriteV2(context.Background(), g.scope, guardedUpdate("old-server", sum))
	if e == nil || !strings.Contains(e.Error(), "differs from expected_description_sha256") || g.app.writes != 0 {
		t.Fatal("old server: the 1.0 refusal changed", e)
	}
	if _, ok := g.sentUpdatedAt(t); ok || g.member.reads != reads {
		t.Fatal("expected_updated_at sent, or the issue read, without the capability")
	}
}

// An edit between the client's read and the server's moves updated_at:
// the server refuses, nothing is sent and the ledger holds nothing.
func TestAppDescriptionGuardRefusesAMovedUpdatedAt(t *testing.T) {
	g := newGuardPair(t, true)
	sum := g.read(t)
	g.before = func() { guardIssueV2(g.app, "Edited. "+appMention, laterAt) }
	in := guardedUpdate("moved", sum)
	if _, e := g.client.WriteV2(context.Background(), g.scope, in); e == nil || !strings.Contains(e.Error(), "differs from expected_description_sha256") || g.app.writes != 0 {
		t.Fatal("an edit after the client's read was overwritten", e)
	}
	if state := ledgerStateV2(t, g.hosted, g.hostedAuth, in.OperationID); state != "absent" {
		t.Fatal("a refused guard left ledger state", state)
	}
}

// A later updated_at with the same description (labels or state moved):
// a description the app reads as hashed still passes on the hash (as in
// 1.0); a mentioned one is refused, failing safe.
func TestAppDescriptionGuardFallsBackToTheHash(t *testing.T) {
	g := newGuardPair(t, true)
	guardIssueV2(g.member, "plain body", readAt)
	guardIssueV2(g.app, "plain body", readAt)
	g.before = func() { guardIssueV2(g.app, "plain body", laterAt) }
	if _, e := g.client.WriteV2(context.Background(), g.scope, guardedUpdate("plain", descriptionSHA256("plain body"))); e != nil || g.app.writes != 1 {
		t.Fatal("an unchanged plain description was refused after a label change", e)
	}
	g = newGuardPair(t, true)
	sum := g.read(t)
	g.before = func() { guardIssueV2(g.app, appMention, laterAt) }
	if _, e := g.client.WriteV2(context.Background(), g.scope, guardedUpdate("mention-later", sum)); e == nil || g.app.writes != 0 {
		t.Fatal("a mentioned description passed with a moved updated_at", e)
	}
}

// The member's fresh read no longer has the agent's hash: the client
// does not refuse by itself, it sends the hash alone, and the server
// refuses it; nothing is sent.
func TestAppDescriptionGuardShaMismatchSendsTheHashAlone(t *testing.T) {
	g := newGuardPair(t, true)
	sum := g.read(t)
	guardIssueV2(g.member, "Edited. "+memberMention, laterAt)
	guardIssueV2(g.app, "Edited. "+appMention, laterAt)
	if _, e := g.client.WriteV2(context.Background(), g.scope, guardedUpdate("stale", sum)); e == nil || !strings.Contains(e.Error(), "differs from expected_description_sha256") || g.app.writes != 0 {
		t.Fatal("a stale hash passed", e)
	}
	if len(g.server.writes) != 1 {
		t.Fatal("the client refused by itself; the server never saw the write")
	}
	if _, ok := g.sentUpdatedAt(t); ok {
		t.Fatal("expected_updated_at sent with a hash the fresh read did not match")
	}
}

// A retry of a write that landed (its answer lost) replays its receipt:
// the member read now hashes the new text, so the hash goes alone, and the
// server's ledger answers before any guard.
func TestAppDescriptionGuardRetryReplays(t *testing.T) {
	g := newGuardPair(t, true)
	sum := g.read(t)
	in := guardedUpdate("landed", sum)
	first, e := g.client.WriteV2(context.Background(), g.scope, in)
	if e != nil || g.app.writes != 1 {
		t.Fatal(e)
	}
	guardIssueV2(g.member, "new body", laterAt)
	guardIssueV2(g.app, "new body", laterAt)
	again, e := g.client.WriteV2(context.Background(), g.scope, in)
	if e != nil || g.app.writes != 1 || again == nil || first == nil {
		t.Fatal("a retry of a landed write was refused or resent", e, g.app.writes)
	}
}

// expected_updated_at is a precondition, not the write: the ledger
// digest leaves it out, so the same operation_id with another
// updated_at replays rather than refusing a different payload.
func TestAppDescriptionGuardLedgerDigestIgnoresUpdatedAt(t *testing.T) {
	s, _, p, scope := testV2(t)
	s.cfg.UpdatedAtGuard = true
	guardIssueV2(p, appMention, readAt)
	args := map[string]any{"id": issueID, "operation_id": "digest", "description": "new", "expected_description_sha256": descriptionSHA256(memberMention), "expected_updated_at": readAt}
	if _, e := s.ExecuteV2(context.Background(), scope, "update", args); e != nil || p.writes != 1 {
		t.Fatal(e)
	}
	guardIssueV2(p, "new", laterAt)
	args["expected_updated_at"] = laterAt
	if _, e := s.ExecuteV2(context.Background(), scope, "update", args); e != nil || p.writes != 1 {
		t.Fatal("a retry with another expected_updated_at did not replay", e, p.writes)
	}
}

// An older client sends the hash alone: the server checks it as in 1.0.
func TestAppDescriptionGuardOldClient(t *testing.T) {
	s, _, p, scope := testV2(t)
	s.cfg.UpdatedAtGuard = true
	guardIssueV2(p, appMention, readAt)
	args := map[string]any{"id": issueID, "operation_id": "old-client", "description": "new", "expected_description_sha256": descriptionSHA256(memberMention)}
	if _, e := s.ExecuteV2(context.Background(), scope, "update", args); e == nil || p.writes != 0 {
		t.Fatal("an old client's mismatched hash passed", e)
	}
	guardIssueV2(p, "plain body", readAt)
	args["operation_id"], args["expected_description_sha256"] = "old-client-2", descriptionSHA256("plain body")
	if _, e := s.ExecuteV2(context.Background(), scope, "update", args); e != nil || p.writes != 1 {
		t.Fatal("an old client's current hash was refused", e)
	}
}

// updated_at moving between the preflight read and the in-lock re-read
// is a terminal refusal, as any in-lock drift is.
func TestAppDescriptionGuardDriftInsideTheLockIsTerminal(t *testing.T) {
	s, a, p, scope := testV2(t)
	s.cfg.UpdatedAtGuard = true
	guardIssueV2(p, appMention, readAt)
	a.afterAccess = func(n int) {
		if n == 2 {
			guardIssueV2(p, appMention, laterAt)
		}
	}
	args := map[string]any{"id": issueID, "operation_id": "drift", "description": "new", "expected_description_sha256": descriptionSHA256(memberMention), "expected_updated_at": readAt}
	if _, e := s.ExecuteV2(context.Background(), scope, "update", args); e == nil || p.writes != 0 {
		t.Fatal("drift inside the lock passed", e)
	}
	if state := ledgerStateV2(t, s, a, "drift"); state != "refused" {
		t.Fatal("in-lock updated_at drift was not terminal", state)
	}
}

// expected_updated_at comes only with a description guard, as an RFC 3339
// time, and only the hosted app service takes it.
func TestAppDescriptionGuardValidation(t *testing.T) {
	sum := descriptionSHA256("x")
	for name, in := range map[string]WriteInputV2{
		"without the hash": {WriteInput: WriteInput{Operation: "update", OperationID: "v1", IssueID: issueID, Description: ptrV2("x")}, ExpectedUpdatedAt: ptrV2(readAt)},
		"not a time":       {WriteInput: WriteInput{Operation: "update", OperationID: "v2", IssueID: issueID, Description: ptrV2("x")}, ExpectedDescriptionSHA256: &sum, ExpectedUpdatedAt: ptrV2("yesterday")},
		"too long":         {WriteInput: WriteInput{Operation: "update", OperationID: "v3", IssueID: issueID, Description: ptrV2("x")}, ExpectedDescriptionSHA256: &sum, ExpectedUpdatedAt: ptrV2("2026-10-31T09:05:12." + strings.Repeat("0", 60) + "Z")},
		"on a title":       {WriteInput: WriteInput{Operation: "update", OperationID: "v4", IssueID: issueID, Title: ptrV2("x")}, ExpectedUpdatedAt: ptrV2(readAt)},
	} {
		if e := ValidateWriteV2(in); e == nil {
			t.Fatal(name, "accepted")
		}
	}
	s, _, p, scope := testV2(t)
	guardIssueV2(p, "x", readAt)
	args := map[string]any{"id": issueID, "operation_id": "local", "description": "new", "expected_description_sha256": sum, "expected_updated_at": readAt}
	if _, e := s.ExecuteV2(context.Background(), scope, "update", args); e == nil || !strings.Contains(e.Error(), "unknown task-board V2 argument") || p.reads != 0 {
		t.Fatal("a service other than the app writer took expected_updated_at", e)
	}
}

// updated_at times compare as instants, never as text.
func TestSameUpdatedAt(t *testing.T) {
	for _, c := range []struct {
		want *string
		got  string
		same bool
	}{
		{ptrV2("2026-10-31T09:05:12.340Z"), "2026-10-31T09:05:12.34+00:00", true},
		{ptrV2("2026-10-31T10:05:12.34+01:00"), "2026-10-31T09:05:12.340Z", true},
		{ptrV2("2026-10-31T09:05:12.340Z"), "2026-10-31T09:05:12.341Z", false},
		{ptrV2("2026-10-31T09:05:12.340Z"), "", false},
		{ptrV2(""), "", false},
		{nil, "2026-10-31T09:05:12.340Z", false},
	} {
		if sameUpdatedAt(c.want, c.got) != c.same {
			t.Fatal(c.want, c.got)
		}
	}
}
