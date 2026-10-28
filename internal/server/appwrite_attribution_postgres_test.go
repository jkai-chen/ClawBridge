package server

import (
	"context"
	"testing"
	"time"

	"clawbridge/internal/bus"
	"clawbridge/internal/linearboard"
	"clawbridge/internal/linearprovider"
)

const (
	awNeedsAgent = "f7000000-0000-4000-8000-0000000000f1"
	awOtherLabel = "f7000000-0000-4000-8000-0000000000f2"
)

// credit reads intake's attribution for target on awIssue around now.
func (f *awFixture) credit(kind, target string) bus.AttributionCredit {
	f.t.Helper()
	ctx := context.Background()
	pid, err := f.store.WatchProjectID(ctx, f.workspace, "p")
	if err != nil || pid == "" {
		f.t.Fatal("the project is not bound:", pid, err)
	}
	now := time.Now()
	c, err := f.store.IntakeAttribution(ctx, pid, awIssue, kind, target, now.Add(-time.Minute), now.Add(time.Minute))
	if err != nil {
		f.t.Fatal(err)
	}
	return c
}

// A1: an accepted comment through the app is recorded, a session's too; a
// session's write is never creditable. The fixture's board is on a legacy
// mapping, so this is also the older receipt's path.
func TestPostgresAppWriteRecordsASessionsComment(t *testing.T) {
	f := newAWFixture(t, awOrg)
	out, err := f.write("comment", map[string]any{"id": awIssue, "body": "hello", "operation_id": "e2e-attr-1"})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := out.Data.(map[string]any)["receipt"].(linearprovider.Receipt); !ok {
		t.Fatalf("the fixture's board no longer answers with the legacy receipt: %T", out.Data.(map[string]any)["receipt"])
	}
	if c := f.credit("comment", awComment); c.Result != "attributed_untrusted" || c.Principal != f.owner {
		t.Fatal("the session's comment was not recorded as untrusted:", c)
	}
}

// The record after a receipt: only the person's own device, sessionless and
// with no agent marker, is creditable; every label a write may have added
// counts, so a replaced set or an uncertain update is never invisible.
func TestPostgresAppWriteAttribution(t *testing.T) {
	ctx := context.Background()
	device := func(f *awFixture) bus.Identity {
		return bus.Identity{Principal: f.owner, Issuer: bus.DeviceIssuer, Subject: "dev_owner"}
	}
	sessionless := bus.Request{Operation: "linear_write", Project: "p"}
	issue := linearboard.ReceiptV2{Kind: "issue", Outcome: "accepted", ID: awIssue}
	for name, c := range map[string]struct {
		session, marker, operation string
		args                       map[string]any
		receipt                    any
		google                     bool
		want                       string
	}{
		"own terminal":         {operation: "update", args: map[string]any{"add_label_ids": []any{awNeedsAgent}}, receipt: linearboard.ReceiptV2{Kind: "issue", ID: awIssue, LabelsAdded: []string{awNeedsAgent}}, want: "credited"},
		"pointer receipt":      {operation: "update", args: map[string]any{}, receipt: &linearboard.ReceiptV2{Kind: "issue", ID: awIssue, LabelsAdded: []string{awNeedsAgent}}, want: "credited"},
		"create's labels":      {operation: "create", args: map[string]any{"label_ids": []string{awNeedsAgent}}, receipt: issue, want: "credited"},
		"replaced set":         {operation: "update", args: map[string]any{"label_ids": []any{awOtherLabel, awNeedsAgent}}, receipt: issue, want: "credited"},
		"session":              {session: awSession, operation: "update", args: map[string]any{"add_label_ids": []any{awNeedsAgent}}, receipt: issue, want: "attributed_untrusted"},
		"agent marker":         {marker: "CLAUDECODE", operation: "update", args: map[string]any{"add_label_ids": []any{awNeedsAgent}}, receipt: issue, want: "attributed_untrusted"},
		"older client":         {marker: bus.MarkerUnsent, operation: "update", args: map[string]any{"add_label_ids": []any{awNeedsAgent}}, receipt: issue, want: "attributed_untrusted"},
		"google sign-in":       {google: true, operation: "update", args: map[string]any{"add_label_ids": []any{awNeedsAgent}}, receipt: issue, want: "attributed_untrusted"},
		"no labels":            {operation: "update", args: map[string]any{"remove_label_ids": []any{awNeedsAgent}}, receipt: issue, want: ""},
		"relation":             {operation: "relation_add", args: map[string]any{}, receipt: linearboard.ReceiptV2{Kind: "relation"}, want: ""},
		"legacy issue receipt": {operation: "update", args: map[string]any{"add_label_ids": []any{awNeedsAgent}}, receipt: map[string]any{"id": awIssue}, want: "credited"},
		"not a board receipt":  {operation: "update", args: map[string]any{"add_label_ids": []any{awNeedsAgent}}, receipt: "accepted", want: ""},
		"comment on the issue": {operation: "comment", args: map[string]any{}, receipt: linearboard.ReceiptV2{Kind: "comment", ID: awComment, IssueID: awIssue}, want: ""},
	} {
		t.Run(name, func(t *testing.T) {
			f := newAWFixture(t, awOrg)
			f.must(bus.Request{Operation: "intake_trust_add", Workspace: f.workspace, Project: "p", Args: map[string]any{"linear_user_id": awAppUser, "principal": f.owner}})
			id := device(f)
			if c.google {
				id = f.identity()
			}
			r := sessionless
			r.Workspace, r.Session = f.workspace, c.session
			f.writer.attribute(ctx, id, r, c.operation, c.args, c.receipt, c.marker)
			if got := f.credit("label", awNeedsAgent); got.Result != c.want {
				t.Fatal(got)
			}
		})
	}
}

// L1: a record that fails is counted, and the app status reports it for
// doctor.
func TestPostgresAppWriteCountsAFailedRecord(t *testing.T) {
	f := newAWFixture(t, awOrg)
	id := bus.Identity{Principal: f.owner, Issuer: bus.DeviceIssuer, Subject: "dev_owner"}
	f.store.Close()
	f.writer.attribute(context.Background(), id, bus.Request{Operation: "linear_write", Workspace: f.workspace, Project: "p"}, "update", map[string]any{"add_label_ids": []any{awNeedsAgent}}, linearboard.ReceiptV2{Kind: "issue", ID: awIssue}, "")
	if n := f.writer.attributionFailures.Load(); n != 1 {
		t.Fatal("failed records counted:", n)
	}
}
