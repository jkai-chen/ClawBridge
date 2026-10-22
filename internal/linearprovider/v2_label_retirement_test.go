package linearprovider

import (
	"context"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestV2LabelRetirementDisqualifiesIndependentArchiveState(t *testing.T) {
	for _, mode := range []string{"active", "retired", "retired and archive null", "retired no archive proof", "retirement null no archive proof", "retirement wrong type", "retirement empty"} {
		t.Run(mode, func(t *testing.T) {
			row := map[string]any{"id": tid, "teamId": tid, "isGroup": false, "archived": false, "retiredAt": nil}
			switch mode {
			case "retired":
				row["retiredAt"] = "2026-10-22T12:00:00Z"
			case "retired and archive null":
				row["retiredAt"] = "2026-10-22T12:00:00Z"
				row["archivedAt"] = nil
			case "retired no archive proof":
				delete(row, "archived")
				row["retiredAt"] = "2026-10-22T12:00:00Z"
			case "retirement null no archive proof":
				delete(row, "archived")
			case "retirement wrong type":
				row["retiredAt"] = false
			case "retirement empty":
				row["retiredAt"] = ""
			}
			f := &fake{tools: []*mcp.Tool{tool("list_issue_labels", map[string]string{"team": "string", "limit": "number"})}, result: map[string]any{"labels": []any{row}, "hasNextPage": false}}
			p, err := NewWithCaller(context.Background(), f)
			if err != nil {
				t.Fatal(err)
			}
			page, err := p.Labels(context.Background(), tid, PageOptions{})
			if mode == "retirement wrong type" || mode == "retirement empty" {
				if err == nil || len(page.Items) != 0 {
					t.Fatal("malformed retirement accepted", page, err)
				}
				return
			}
			if err != nil || len(page.Items) != 1 {
				t.Fatal(page, err)
			}
			label := page.Items[0]
			switch mode {
			case "active":
				if label.Archived || !label.EligibilityKnown {
					t.Fatal("active control rejected", label)
				}
			case "retirement null no archive proof":
				if label.EligibilityKnown {
					t.Fatal("null retirement invented archive proof", label)
				}
			default:
				if !label.Archived {
					t.Fatal("retired label eligible", label)
				}
			}
		})
	}
}
