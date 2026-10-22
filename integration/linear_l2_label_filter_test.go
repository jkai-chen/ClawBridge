package integration

import (
	"context"
	"net/http"
	"reflect"
	"testing"

	"clawbridge/internal/linearboard"
	"clawbridge/internal/linearprovider"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// l2NativeLabelListTool mirrors the native list_issue_labels contract: rows
// carry no team or group field, and eligibility rests on the documented
// includeArchived/includeGroups filters.
func l2NativeLabelListTool() *mcp.Tool {
	return &mcp.Tool{
		Name:        "list_issue_labels",
		Description: "List available issue labels in a Linear workspace or team. A retired label carries `retiredAt` and cannot be applied to new issues.",
		InputSchema: map[string]any{"$schema": "http://json-schema.org/draft-07/schema#", "type": "object", "additionalProperties": false, "properties": map[string]any{
			"cursor":          map[string]any{"type": "string", "description": "Omit for the first page. To continue, pass the previous response's cursor when hasNextPage is true"},
			"includeArchived": map[string]any{"type": "boolean", "default": false, "description": "Include archived items"},
			"includeGroups":   map[string]any{"type": "boolean", "default": false, "description": "Include label groups in the results, marked with `isGroup`. A group cannot be applied directly; retiring or restoring one cascades to its child labels"},
			"limit":           map[string]any{"type": "number", "default": 50, "maximum": 250, "description": "Maximum number of results to return per page"},
			"name":            map[string]any{"type": "string", "description": "Filter by name"},
			"orderBy":         map[string]any{"type": "string", "default": "updatedAt", "enum": []any{"createdAt", "updatedAt"}},
			"team":            map[string]any{"type": "string", "description": "Team name or ID"},
		}},
	}
}

// l2NativeLabelRow mirrors an observed active row: no team, group or
// retiredAt field, and an explicit null archivedAt.
func l2NativeLabelRow(id string) map[string]any {
	return map[string]any{"id": id, "name": "Synthetic label", "color": "#000000", "description": "", "archivedAt": nil}
}

func TestL2NativeFilterScopedLabelAppliesThroughPublicWrite(t *testing.T) {
	for _, mode := range []string{"active", "retired", "group", "stale_expectation"} {
		t.Run(mode, func(t *testing.T) {
			f := newFixture(t)
			f.setup("w", "p")
			f.call("taskboard_set_v2", "w", "p", "", l2MappingArgs(0))
			peer := newL1ProviderPeer()
			peer.tools = []*mcp.Tool{peer.tools[0], peer.tools[1], peer.tools[2], l2NativeLabelListTool(), l2ActualSaveIssueTool(t)}
			cfg := l1ServiceConfig(t, f.store, &l1SyntheticProvider{})
			cfg.Open = func(ctx context.Context, _ *http.Client, _ bool) (linearboard.Provider, error) {
				return linearprovider.NewWithCaller(ctx, peer)
			}
			saves := []map[string]any{}
			peer.result = func(name string, args map[string]any) (any, error) {
				switch name {
				case "get_team", "get_project":
					return l1PeerResult(name, args)
				case "get_issue":
					return map[string]any{"id": l1IssueID, "teamId": l1TeamA, "projectId": l1Project, "title": "Synthetic scoped issue", "labelIds": []any{l2LabelA}}, nil
				case "list_issue_labels":
					// Eligibility is only proven when both filters are sent as false
					// for the issue's own team.
					if args["team"] != l1TeamA || args["includeArchived"] != false || args["includeGroups"] != false {
						t.Fatalf("label list did not pin the issue team and filters: %+v", args)
					}
					target := l2NativeLabelRow(l2LabelB)
					switch mode {
					case "retired":
						target["retiredAt"] = "2026-01-01T00:00:00Z"
					case "group":
						target["isGroup"] = true
						target["groupType"] = "singleSelect"
					}
					return map[string]any{"labels": []any{l2NativeLabelRow(l2LabelA), target}, "hasNextPage": false}, nil
				case "save_issue":
					saves = append(saves, args)
					return map[string]any{"id": l1IssueID, "teamId": l1TeamA, "projectId": l1Project, "title": "Synthetic scoped issue", "labelIds": []any{l2LabelA, l2LabelB}}, nil
				default:
					t.Fatalf("unexpected native tool: %s", name)
					return nil, nil
				}
			}
			expected := []string{l2LabelA}
			if mode == "stale_expectation" {
				expected = []string{}
			}
			in := linearboard.WriteInputV2{
				WriteInput:     linearboard.WriteInput{Operation: "update", OperationID: "native-filter-label-" + mode, IssueID: l1IssueID},
				Labels:         linearprovider.Field[[]string]{Present: true, Value: []string{l2LabelA, l2LabelB}},
				ExpectedLabels: linearprovider.Field[[]string]{Present: true, Value: expected},
			}
			result, err := linearboard.New(cfg).WriteV2(context.Background(), f.request("", "w", "p", "worker", actor("worker", nil)), in)
			if mode != "active" {
				if err == nil || result != nil || len(saves) != 0 {
					t.Fatal("retired/group/stale native label reached mutation", err, saves)
				}
				return
			}
			if err != nil || len(saves) != 1 {
				t.Fatal("filter-proven native label was not applied exactly once", err, saves)
			}
			if !reflect.DeepEqual(saves[0], map[string]any{"id": l1IssueID, "labels": []any{l2LabelA, l2LabelB}}) {
				t.Fatalf("label replacement sent unrelated fields or a different set: %+v", saves[0])
			}
		})
	}
}
