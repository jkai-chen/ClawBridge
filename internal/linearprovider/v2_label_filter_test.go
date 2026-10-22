package linearprovider

import (
	"context"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// nativeLabelList mirrors the documented native list_issue_labels contract.
func nativeLabelList() *mcp.Tool {
	return &mcp.Tool{
		Name:        "list_issue_labels",
		Description: "List available issue labels in a Linear workspace or team. A retired label carries `retiredAt` and cannot be applied to new issues.",
		InputSchema: map[string]any{"type": "object", "properties": map[string]any{
			"team":            map[string]any{"type": "string", "description": "Team name or ID"},
			"limit":           map[string]any{"type": "number", "default": 50, "maximum": 250},
			"cursor":          map[string]any{"type": "string"},
			"includeArchived": map[string]any{"type": "boolean", "default": false, "description": "Include archived items"},
			"includeGroups":   map[string]any{"type": "boolean", "default": false, "description": "Include label groups in the results, marked with `isGroup`. A group cannot be applied directly; retiring or restoring one cascades to its child labels"},
		}},
	}
}

// nativeLabelRow mirrors an observed active row: no team, group or retiredAt
// field, and an explicit null archivedAt.
func nativeLabelRow(id string) map[string]any {
	return map[string]any{"id": id, "name": "Synthetic label", "color": "#000000", "description": nil, "archivedAt": nil}
}

func TestV2LabelEligibilityByPinnedFilter(t *testing.T) {
	retired, archived := nativeLabelRow(iid), nativeLabelRow(uid)
	retired["retiredAt"] = "2026-10-22T12:00:00Z"
	archived["archivedAt"] = "2026-10-22T12:00:00Z"
	f := &fake{tools: []*mcp.Tool{nativeLabelList()}, result: map[string]any{"labels": []any{nativeLabelRow(tid), retired, archived}, "hasNextPage": false}}
	p, _ := NewWithCaller(context.Background(), f)
	caps, err := p.Capabilities(context.Background())
	if err != nil || !caps.Labels || !caps.LabelEligibilityByFilter || len(f.calls) != 0 {
		t.Fatal("pinned filter capability not advertised", caps, err)
	}
	page, err := p.Labels(context.Background(), tid, PageOptions{})
	if err != nil || len(page.Items) != 3 {
		t.Fatal(page, err)
	}
	if f.args[0]["team"] != tid || f.args[0]["includeArchived"] != false || f.args[0]["includeGroups"] != false {
		t.Fatal("filters were not sent explicitly", f.args[0])
	}
	active := page.Items[0]
	if !active.EligibilityKnown || !active.FilterScoped || active.TeamID != "" || active.Global || active.Archived || active.Group {
		t.Fatal("active filtered row not proven eligible without inventing a team", active)
	}
	for _, label := range page.Items[1:] {
		if !label.Archived {
			t.Fatal("retired or archived row not disqualified", label)
		}
	}
	group := nativeLabelRow(tid)
	group["isGroup"] = true
	group["groupType"] = "singleSelect"
	f.result = map[string]any{"labels": []any{group}, "hasNextPage": false}
	if page, err = p.Labels(context.Background(), tid, PageOptions{}); err == nil || len(page.Items) != 0 {
		t.Fatal("group row contradicting includeGroups=false accepted", page, err)
	}
}

func TestV2LabelFilterSchemaDriftDisablesEligibility(t *testing.T) {
	drift := map[string]func(props map[string]any, tool *mcp.Tool){
		"groups default true":      func(p map[string]any, _ *mcp.Tool) { p["includeGroups"].(map[string]any)["default"] = true },
		"groups untyped":           func(p map[string]any, _ *mcp.Tool) { p["includeGroups"].(map[string]any)["type"] = "string" },
		"archived missing":         func(p map[string]any, _ *mcp.Tool) { delete(p, "includeArchived") },
		"archived default missing": func(p map[string]any, _ *mcp.Tool) { delete(p["includeArchived"].(map[string]any), "default") },
		"groups meaning changed": func(p map[string]any, _ *mcp.Tool) {
			p["includeGroups"].(map[string]any)["description"] = "Include groups"
		},
		"retirement undocumented": func(_ map[string]any, t *mcp.Tool) { t.Description = "List available issue labels." },
	}
	for name, change := range drift {
		t.Run(name, func(t *testing.T) {
			tool := nativeLabelList()
			change(tool.InputSchema.(map[string]any)["properties"].(map[string]any), tool)
			f := &fake{tools: []*mcp.Tool{tool}, result: map[string]any{"labels": []any{nativeLabelRow(tid)}, "hasNextPage": false}}
			p, _ := NewWithCaller(context.Background(), f)
			caps, _ := p.Capabilities(context.Background())
			page, err := p.Labels(context.Background(), tid, PageOptions{})
			if caps.LabelEligibilityByFilter || err != nil || len(page.Items) != 1 || page.Items[0].EligibilityKnown || page.Items[0].FilterScoped {
				t.Fatal("drifted schema still proved eligibility", caps, page, err)
			}
			if _, sent := f.args[0]["includeGroups"]; sent {
				t.Fatal("drifted filter argument was relied upon", f.args[0])
			}
		})
	}
}

func TestV2CapabilityReasonsExplainSwitchOff(t *testing.T) {
	f := &fake{tools: []*mcp.Tool{nativeLabelList(), nativeIssueList(), nativeUserList(), nativeStatusList(), v2MutationTool("update_issue", true), tool("get_issue", map[string]string{"id": "string"}), nativeCommentList()}}
	p, _ := NewWithCaller(context.Background(), f)
	caps, err := p.Capabilities(context.Background())
	if err != nil || !caps.LabelEligibilityByFilter || !caps.ParentSameDestination || !caps.LabelMembershipProof || caps.Reasons != nil {
		t.Fatal("healthy capabilities reported a reason", caps, err)
	}
	drifted := nativeLabelList()
	drifted.InputSchema.(map[string]any)["properties"].(map[string]any)["includeGroups"].(map[string]any)["default"] = true
	f = &fake{tools: []*mcp.Tool{drifted, v2MutationTool("update_issue", true)}}
	p, _ = NewWithCaller(context.Background(), f)
	caps, _ = p.Capabilities(context.Background())
	if caps.LabelEligibilityByFilter || caps.Reasons["label_eligibility_by_filter"] != "list_issue_labels schema changed: includeGroups missing or no longer a boolean defaulting to false" {
		t.Fatal("label switch-off reason missing", caps)
	}
	if caps.ParentSameDestination || caps.Reasons["parent_same_destination"] != "get_issue is not advertised" || len(f.calls) != 0 {
		t.Fatal("parent switch-off reason missing or I/O performed", caps)
	}
	p, _ = NewWithCaller(context.Background(), &fake{})
	caps, _ = p.Capabilities(context.Background())
	if caps.Reasons["label_eligibility_by_filter"] != "label listing is unavailable" || caps.Reasons["parent_same_destination"] != "no issue mutation tool accepts parentId beside project" {
		t.Fatal("absent tools not explained", caps)
	}
}
