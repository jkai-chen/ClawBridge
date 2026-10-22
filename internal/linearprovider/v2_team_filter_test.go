package linearprovider

import (
	"context"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Native status rows carry no team; they belong to the queried team only
// while list_issue_statuses documents itself as one team's statuses, and a
// partial list is refused because the tool cannot page.
func TestV2StatusTeamFilterIsPinnedAndListsAreComplete(t *testing.T) {
	row := map[string]any{"id": uid, "name": "Todo"}
	f := &fake{tools: []*mcp.Tool{nativeStatusList()}, result: map[string]any{"statuses": []any{row}}}
	p, _ := NewWithCaller(context.Background(), f)
	states, e := p.Statuses(context.Background(), tid)
	if e != nil || len(states) != 1 || states[0].TeamID != tid {
		t.Fatal("documented team filter refused", states, e)
	}
	f.result = map[string]any{"statuses": []any{row}, "hasNextPage": true, "nextCursor": "more"}
	if _, e = p.Statuses(context.Background(), tid); e == nil {
		t.Fatal("partial status list treated as the whole workflow")
	}
	foreign := map[string]any{"id": uid, "name": "Todo", "teamId": iid}
	f.result = map[string]any{"statuses": []any{foreign}}
	if _, e = p.Statuses(context.Background(), tid); e == nil {
		t.Fatal("another team's status accepted")
	}
	drifted := nativeStatusList()
	drifted.Description = "List issue statuses"
	f = &fake{tools: []*mcp.Tool{drifted}, result: map[string]any{"statuses": []any{row}}}
	p, _ = NewWithCaller(context.Background(), f)
	caps, _ := p.Capabilities(context.Background())
	if _, e = p.Statuses(context.Background(), tid); e == nil || caps.StatusTeamFilter || caps.Reasons["status_team_filter"] == "" {
		t.Fatal("team-less status accepted without the documented filter", e, caps)
	}
	own := map[string]any{"id": uid, "name": "Todo", "teamId": tid}
	f.result = map[string]any{"statuses": []any{own}}
	if states, e = p.Statuses(context.Background(), tid); e != nil || len(states) != 1 {
		t.Fatal("status naming its own team refused", states, e)
	}
}

func TestV2MemberTeamFilterCapabilityExplainsDrift(t *testing.T) {
	p, _ := NewWithCaller(context.Background(), &fake{tools: []*mcp.Tool{nativeUserList()}})
	caps, _ := p.Capabilities(context.Background())
	if !caps.MemberTeamFilter {
		t.Fatal("documented member team filter not recognized", caps.Reasons)
	}
	p, _ = NewWithCaller(context.Background(), &fake{tools: []*mcp.Tool{tool("list_users", map[string]string{"team": "string"})}})
	caps, _ = p.Capabilities(context.Background())
	if caps.MemberTeamFilter || caps.Reasons["member_team_filter"] == "" {
		t.Fatal("undocumented member team filter trusted", caps)
	}
}
