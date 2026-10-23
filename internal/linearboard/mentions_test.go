package linearboard

import (
	"context"
	"errors"
	"testing"
)

// Every @handle is a mention anywhere, code included, as are profile and
// mention:// links; markdown structure is never parsed. Email addresses pass.
func TestMentionGuardRefusesMentionsAnywhere(t *testing.T) {
	for _, text := range []string{
		"@Linear please fix this",
		"ping @linear",
		"PING @LINEAR now",
		"thanks (@Linear)",
		"hi\n@ann, can you look",
		"**@ann** said so",
		"[@Linear](https://linear.app/acme/profiles/linear)",
		"see [Ann](https://linear.app/acme/profiles/ann)",
		"see https://LINEAR.APP/profiles/ann",
		"see linear.app/acme/profiles",
		"see <mention://user/123>",
		"@日本",
		"Done.@bob",
		"ok-@bob",
		"x_@bob and y+@bob",
		// Code is no exemption.
		"inline `@Linear` code",
		"use `@Override` on it",
		"```\n@Linear do it\n```",
		"```\nimport fs from '@types/node'\n```",
		"~~~css\n@media print {}\n~~~",
		"    @ann in an indented block",
		// Earlier structural bypasses are refused as plain text.
		"see `a\n\n@bob please look `",
		"# heading `x\n\n- @bob `",
		"\\` @bob \\`",
		"```\ncode\n````\n@bob",
		"```ts\nx\n```\n@bob",
		"~~~\n```\n~~~\n@bob",
		"````\n```\n````\n@bob",
		"see `a\r\r@bob `",
	} {
		if e := mentionGuard(&text); !errors.Is(e, errMention) {
			t.Errorf("%q not refused: %v", text, e)
		}
	}
	for _, text := range []string{
		"mail a@linear.app or first.last+x@linear.app",
		"in code: `ops@example.com`",
		"https://linear.app/acme/issue/SYN-6/title",
		"Linear and linear.app are fine to name",
		"price @ 5 each",
		"install types/node",
		"",
	} {
		if e := mentionGuard(&text); e != nil {
			t.Errorf("%q refused", text)
		}
	}
	if mentionGuard(nil, nil) != nil {
		t.Fatal("absent fields refused")
	}
}

// A mention is refused in validation, before the ledger and Linear: nothing
// is sent and the operation ID stays free.
func TestAgentWrittenTextWithAMentionIsNotSent(t *testing.T) {
	ctx := context.Background()
	s, _, p, scope := testV2(t)
	mention := "ask @Linear"
	for name, in := range map[string]WriteInputV2{
		"comment":     {WriteInput: WriteInput{Operation: "comment", OperationID: "m-1", IssueID: issueID, Body: mention}},
		"title":       {WriteInput: WriteInput{Operation: "update", OperationID: "m-1", IssueID: issueID, Title: &mention}},
		"description": {WriteInput: WriteInput{Operation: "update", OperationID: "m-1", IssueID: issueID, Description: &mention}},
		"create":      {WriteInput: WriteInput{Operation: "create", OperationID: "m-1", Title: ptrV2("t"), Description: &mention}},
	} {
		if _, e := s.WriteV2(ctx, scope, in); !errors.Is(e, errMention) {
			t.Fatal(name, "not refused", e)
		}
	}
	if p.writes != 0 || len(p.created) != 0 || len(p.updated) != 0 {
		t.Fatal("a refused write reached Linear", p.writes, p.created, p.updated)
	}
	if _, e := s.WriteV2(ctx, scope, WriteInputV2{WriteInput: WriteInput{Operation: "comment", OperationID: "m-1", IssueID: issueID, Body: "ask the coordinator"}}); e != nil || p.writes != 1 {
		t.Fatal("the refused operation ID was not free", e, p.writes)
	}
	v1, _, p1, scope1 := testService(t)
	if _, e := v1.Write(ctx, scope1, WriteInput{Operation: "comment", OperationID: "m-2", IssueID: issueID, Body: mention}); !errors.Is(e, errMention) || p1.writes != 0 {
		t.Fatal("V1 comment with a mention", e, p1.writes)
	}
}
