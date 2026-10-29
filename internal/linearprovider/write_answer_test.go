package linearprovider

import (
	"context"
	"reflect"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Linear's answer to a write on an issue in no project leaves `project`
// out (a team_board binding's issues). The write isn't uncertain for it:
// the issue is read again by UUID and the read is what's checked.
func TestWriteAnswerWithoutProjectReadsBack(t *testing.T) {
	answer := map[string]any{"id": "TB-4", "uuid": iid, "teamId": tid, "title": "x"}
	read := map[string]any{"id": "TB-4", "uuid": iid, "teamId": tid, "projectId": nil, "title": "x"}
	getIssue := tool("get_issue", map[string]string{"id": "string"})
	t.Run("create", func(t *testing.T) {
		f := &fake{tools: []*mcp.Tool{tool("create_issue", map[string]string{"team": "string", "title": "string", "description": "string"}), getIssue}, results: map[string]any{"create_issue": answer, "get_issue": read}}
		p, _ := NewWithCaller(context.Background(), f)
		x, e := p.Create(context.Background(), CreateInput{TeamID: tid, Title: "x"})
		if e != nil || x.ID != iid || x.ProjectID != "" || !reflect.DeepEqual(f.calls, []string{"create_issue", "get_issue"}) {
			t.Fatal(x, e, f.calls)
		}
	})
	t.Run("update", func(t *testing.T) {
		f := &fake{tools: []*mcp.Tool{tool("update_issue", map[string]string{"id": "string", "title": "string", "description": "string", "state": "string"}), getIssue}, results: map[string]any{"update_issue": answer, "get_issue": read}}
		p, _ := NewWithCaller(context.Background(), f)
		title := "y"
		x, e := p.Update(context.Background(), UpdateInput{ID: iid, Title: &title})
		if e != nil || x.ID != iid || !reflect.DeepEqual(f.calls, []string{"update_issue", "get_issue"}) {
			t.Fatal(x, e, f.calls)
		}
	})
	for _, name := range []string{"create", "update"} {
		t.Run("v2 "+name, func(t *testing.T) {
			f := &fake{tools: []*mcp.Tool{officialSaveIssue(t), getIssue}, results: map[string]any{"save_issue": answer, "get_issue": read}}
			p, _ := NewWithCaller(context.Background(), f)
			var x IssueV2
			var e error
			if name == "create" {
				x, e = p.CreateV2(context.Background(), CreateInputV2{CreateInput: CreateInput{TeamID: tid, Title: "x"}})
			} else {
				title := "y"
				x, e = p.UpdateV2(context.Background(), UpdateInputV2{UpdateInput: UpdateInput{ID: iid, Title: &title}})
			}
			if e != nil || x.ID != iid || !x.ProjectKnown || x.ProjectID != "" || !reflect.DeepEqual(f.calls, []string{"save_issue", "get_issue"}) {
				t.Fatal(x, e, f.calls)
			}
		})
	}
	t.Run("move", func(t *testing.T) {
		f := &fake{tools: []*mcp.Tool{officialSaveIssue(t)}, result: map[string]any{"id": "NEW-1", "uuid": iid, "teamId": moveTeam}}
		p, _ := NewWithCaller(context.Background(), f)
		x, e := p.MoveV2(context.Background(), MoveInputV2{ID: iid, TeamID: moveTeam, StateID: moveState})
		if e != nil || x.ID != iid || x.Identifier != "NEW-1" || x.ProjectKnown || !reflect.DeepEqual(f.calls, []string{"save_issue"}) {
			t.Fatal("the caller reads a move back; its answer only names the issue", x, e, f.calls)
		}
		f.result = map[string]any{"id": "NEW-1", "uuid": uid, "teamId": moveTeam}
		if _, e = p.MoveV2(context.Background(), MoveInputV2{ID: iid, TeamID: moveTeam, StateID: moveState}); e == nil {
			t.Fatal("an answer naming another issue")
		}
	})
	// The read is checked as any write's answer is: another team, or a
	// project the write didn't ask for, is a mismatch, never accepted.
	t.Run("read checked", func(t *testing.T) {
		foreign := map[string]any{"id": "TB-4", "uuid": iid, "teamId": tid, "projectId": uid}
		f := &fake{tools: []*mcp.Tool{tool("create_issue", map[string]string{"team": "string", "title": "string", "description": "string"}), getIssue}, results: map[string]any{"create_issue": answer, "get_issue": foreign}}
		p, _ := NewWithCaller(context.Background(), f)
		if _, e := p.Create(context.Background(), CreateInput{TeamID: tid, Title: "x"}); e == nil {
			t.Fatal("a read in another project accepted")
		}
		f2 := &fake{tools: []*mcp.Tool{officialSaveIssue(t), getIssue}, results: map[string]any{"save_issue": answer, "get_issue": foreign}}
		p2, _ := NewWithCaller(context.Background(), f2)
		if _, e := p2.CreateV2(context.Background(), CreateInputV2{CreateInput: CreateInput{TeamID: tid, Title: "x"}}); e == nil {
			t.Fatal("a V2 read in another project accepted")
		}
	})
}
