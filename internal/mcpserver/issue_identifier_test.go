package mcpserver

import (
	"context"
	"errors"
	"testing"
	"time"

	"clawbridge/internal/bus"
	"clawbridge/internal/linearprovider"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestLinearMCPV2IssueReadAcceptsAnIdentifierAndRefusesGarbage(t *testing.T) {
	p := &recordingBoardV2{recordingBoard: recordingBoard{data: map[string]any{"ok": true}}}
	ctx, cs := boardSession(t, p)
	for _, bad := range []string{"banana", "ENG 88", "ENG-", "-88", "ENG-88; x", "../etc", "", "eng-88", "ENG-0", "ENG-012", "ABCDEFGHIJK-1"} {
		if result, e := cs.CallTool(ctx, &mcp.CallToolParams{Name: "taskboard_issue_v2", Arguments: map[string]any{"id": bad}}); (e == nil && !result.IsError) || p.count() != 0 {
			t.Fatalf("%q reached the adapter", bad)
		}
	}
	for n, good := range []string{"ENG-88", "11111111-1111-4111-8111-111111111111"} {
		if _, e := cs.CallTool(ctx, &mcp.CallToolParams{Name: "taskboard_issue_v2", Arguments: map[string]any{"id": good}}); e != nil || p.count() != n+1 || p.calls[n].args["id"] != good {
			t.Fatalf("%q not forwarded: %v", good, e)
		}
	}
	// Writes keep the UUID-only schema.
	if result, e := cs.CallTool(ctx, &mcp.CallToolParams{Name: "taskboard_comment_v2", Arguments: map[string]any{"id": "ENG-88", "body": "b", "operation_id": "op"}}); e == nil && !result.IsError {
		t.Fatal("a write accepted an identifier")
	}
}

// issueBoard answers the issue read that resolves an identifier.
type issueBoard struct {
	recordingBoardV2
	issue linearprovider.IssueV2
	err   error
	asked []string
}

func (b *issueBoard) ExecuteV2(_ context.Context, _ bus.Request, op string, args map[string]any) (any, error) {
	b.asked = append(b.asked, op+":"+args["id"].(string))
	return b.issue, b.err
}

func proposeSession(t *testing.T, board TaskboardTools) (context.Context, *mcp.ClientSession, *recordingStore) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	store := &recordingStore{}
	server := NewServerWithOptions(store, "w", "p", "eng", "eng-i", Options{Context: ctx, Taskboard: board})
	st, ct := mcp.NewInMemoryTransports()
	ss, e := server.Connect(ctx, st, nil)
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { ss.Close() })
	cs, e := mcp.NewClient(&mcp.Implementation{Name: "t", Version: "1"}, nil).Connect(ctx, ct, nil)
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { cs.Close() })
	return ctx, cs, store
}

func proposed(store *recordingStore) map[string]any {
	store.mu.Lock()
	defer store.mu.Unlock()
	for _, r := range store.calls {
		if r.Operation == "assignment_propose" {
			return r.Args
		}
	}
	return nil
}

func TestMCPAssignmentProposeResolvesAnIdentifierAlone(t *testing.T) {
	const uuid = "0c0c0c0c-0c0c-4c0c-8c0c-0c0c0c0c0c0c"
	board := &issueBoard{issue: linearprovider.IssueV2{Issue: linearprovider.Issue{ID: uuid, Identifier: "ENG-88"}}}
	ctx, cs, store := proposeSession(t, board)
	if r, e := cs.CallTool(ctx, &mcp.CallToolParams{Name: "assignment_propose", Arguments: map[string]any{"issue_identifier": "ENG-88"}}); e != nil || r.IsError {
		t.Fatal(e, r)
	}
	args := proposed(store)
	if args["issue_uuid"] != uuid || args["issue_identifier"] != "ENG-88" || len(board.asked) != 1 || board.asked[0] != "issue:ENG-88" {
		t.Fatal(args, board.asked)
	}
}

func TestMCPAssignmentProposeWithAUUIDOrWithoutLinearIsUnchanged(t *testing.T) {
	const uuid = "0c0c0c0c-0c0c-4c0c-8c0c-0c0c0c0c0c0c"
	board := &issueBoard{err: errors.New("unexpected Linear read")}
	ctx, cs, store := proposeSession(t, board)
	if r, e := cs.CallTool(ctx, &mcp.CallToolParams{Name: "assignment_propose", Arguments: map[string]any{"issue_uuid": uuid, "issue_identifier": "ENG-88"}}); e != nil || r.IsError {
		t.Fatal(e, r)
	}
	if args := proposed(store); args["issue_uuid"] != uuid || len(board.asked) != 0 {
		t.Fatal(args, board.asked)
	}
	// No Linear service: the identifier passes through for the server to judge.
	ctx, cs, store = proposeSession(t, nil)
	if r, e := cs.CallTool(ctx, &mcp.CallToolParams{Name: "assignment_propose", Arguments: map[string]any{"issue_identifier": "ENG-88"}}); e != nil || r.IsError {
		t.Fatal(e, r)
	}
	if args := proposed(store); args["issue_uuid"] != nil || args["issue_identifier"] != "ENG-88" {
		t.Fatal(args)
	}
}

func TestMCPAssignmentProposeRefusesWhenTheIdentifierDoesNotResolve(t *testing.T) {
	board := &issueBoard{err: errors.New("ENG-88 now resolves to OPS-5; use that identifier or the UUID")}
	ctx, cs, store := proposeSession(t, board)
	if r, e := cs.CallTool(ctx, &mcp.CallToolParams{Name: "assignment_propose", Arguments: map[string]any{"issue_identifier": "ENG-88"}}); e == nil && !r.IsError {
		t.Fatal("a stale identifier was proposed")
	}
	if proposed(store) != nil {
		t.Fatal("the server was called")
	}
}
