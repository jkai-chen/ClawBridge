package linearboard

import (
	"context"
	"errors"
	"testing"

	"clawbridge/internal/bus"
	"clawbridge/internal/linearprovider"
)

// textExecutor answers intake_text_status as the bus does, and passes every
// other operation on.
type textExecutor struct {
	Executor
	answer map[string]any
	err    error
	sent   []map[string]any
}

func (x *textExecutor) Execute(ctx context.Context, r bus.Request) (bus.Result, error) {
	if r.Operation != "intake_text_status" {
		return x.Executor.Execute(ctx, r)
	}
	x.sent = append(x.sent, r.Args)
	if x.err != nil {
		return bus.Result{}, x.err
	}
	return bus.Result{Data: x.answer}, nil
}

func TestExecuteIssueShowsTheStoredVerdictAndSendsNoText(t *testing.T) {
	s, _, p, scope := testV2(t)
	i := p.issues[issueID]
	i.Title = "a title"
	i.DescriptionKnown = true
	i.Description = "a description that renders a mention <issue> differently for this profile"
	p.issues[issueID] = i
	x := &textExecutor{Executor: s.cfg.Executor}
	s.cfg.Executor = x
	ctx := context.Background()
	read := func() linearprovider.IssueV2 {
		t.Helper()
		out, e := s.ExecuteV2(ctx, scope, "issue", map[string]any{"id": issueID})
		if e != nil {
			t.Fatal(e)
		}
		return out.(linearprovider.IssueV2)
	}
	x.answer = map[string]any{"tracked": true, "changed": true, "signal_at": "2026-10-23T10:00:00Z"}
	if got := read(); got.Intake == nil || !got.Intake.TextChangedSinceSignal || got.Intake.SignalAt != "2026-10-23T10:00:00Z" {
		t.Fatal("changed:", got.Intake)
	}
	// The client's read is the profile's rendering of the text, which differs
	// from the app's: it sends the bus nothing to compare, only the issue.
	for key := range x.sent[0] {
		if key != "issue_uuid" && key != "instance" {
			t.Fatal("the read sent", key, x.sent[0])
		}
	}
	if x.sent[0]["issue_uuid"] != issueID {
		t.Fatal(x.sent[0])
	}
	// Whatever the profile's description, an unchanged verdict is unchanged.
	x.answer = map[string]any{"tracked": true, "changed": false, "signal_at": "2026-10-23T10:00:00Z"}
	if got := read(); got.Intake == nil || got.Intake.TextChangedSinceSignal {
		t.Fatal("unchanged:", got.Intake)
	}
	x.answer = map[string]any{"tracked": false, "changed": false}
	if got := read(); got.Intake != nil {
		t.Fatal("untracked issue carries the field:", got.Intake)
	}
}

func TestAFailingTextCheckDoesNotFailTheRead(t *testing.T) {
	s, _, _, scope := testV2(t)
	x := &textExecutor{Executor: s.cfg.Executor, err: errors.New("bus unavailable")}
	s.cfg.Executor = x
	out, e := s.ExecuteV2(context.Background(), scope, "issue", map[string]any{"id": issueID})
	if e != nil {
		t.Fatal("the read failed with its check:", e)
	}
	if got := out.(linearprovider.IssueV2); got.Intake != nil || got.ID != issueID {
		t.Fatal(got.Intake)
	}
	if len(x.sent) != 1 {
		t.Fatal("the check was not tried:", x.sent)
	}
}

func TestTheCommentsReadCarriesTheVerdictToo(t *testing.T) {
	s, _, _, _ := commentingTestV2(t)
	scope := bus.Request{Workspace: "ws", Project: "prj", Session: "ses", Args: map[string]any{"instance": "secret-instance"}}
	x := &textExecutor{Executor: s.cfg.Executor, answer: map[string]any{"tracked": true, "changed": true, "signal_at": "2026-10-23T10:00:00Z"}}
	s.cfg.Executor = x
	out, e := s.ExecuteV2(context.Background(), scope, "issue", map[string]any{"id": issueID, "comments": true, "comments_since": "2026-10-22T07:00:00Z"})
	if e != nil {
		t.Fatal(e)
	}
	if got := out.(IssueCommentsV2); got.Intake == nil || !got.Intake.TextChangedSinceSignal || len(x.sent) != 1 {
		t.Fatal(got.Intake, x.sent)
	}
}
