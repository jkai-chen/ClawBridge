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

// l2NativeIssueListTool mirrors the native list_issues filters the label
// membership proof relies on: label is documented as "Label name or ID".
func l2NativeIssueListTool() *mcp.Tool {
	return &mcp.Tool{
		Name: "list_issues",
		InputSchema: map[string]any{"type": "object", "additionalProperties": false, "properties": map[string]any{
			"cursor":          map[string]any{"type": "string"},
			"includeArchived": map[string]any{"type": "boolean", "default": false, "description": "Include archived items"},
			"label":           map[string]any{"type": "string", "description": "Label name or ID"},
			"limit":           map[string]any{"type": "number", "default": 50, "maximum": 250},
			"orderBy":         map[string]any{"type": "string", "enum": []any{"createdAt", "updatedAt"}},
			"project":         map[string]any{"type": "string", "description": "Project name, ID, identifier (e.g., P-ENG-123), or slug"},
			"team":            map[string]any{"type": "string", "description": "Team name or ID"},
		}},
	}
}

// Native issue reads return label names only. A clear must prove the label
// IDs (unique name in the full catalog, list_issues by the label UUID) before
// comparing them with the caller's expected set, and must never send a name.
func TestL2NativeLabelNamesProvenBeforePublicClear(t *testing.T) {
	const labelName = "Synthetic Alpha"
	for _, mode := range []string{"proven", "ambiguous_name", "not_listed"} {
		t.Run(mode, func(t *testing.T) {
			f := newFixture(t)
			f.setup("w", "p")
			f.call("taskboard_set_v2", "w", "p", "", l2MappingArgs(0))
			peer := newL1ProviderPeer()
			peer.tools = []*mcp.Tool{peer.tools[0], peer.tools[1], peer.tools[2], l2NativeLabelListTool(), l2NativeIssueListTool(), l2ActualSaveIssueTool(t)}
			cfg := l1ServiceConfig(t, f.store, &l1SyntheticProvider{})
			cfg.Open = func(ctx context.Context, _ *http.Client, _ bool) (linearboard.Provider, error) {
				return linearprovider.NewWithCaller(ctx, peer)
			}
			labels := []any{labelName}
			issue := func() map[string]any {
				return map[string]any{"id": "SYN-1", "uuid": l1IssueID, "identifier": "SYN-1", "teamId": l1TeamA, "projectId": l1Project, "title": "Synthetic scoped issue", "labels": labels}
			}
			saves := []map[string]any{}
			peer.result = func(name string, args map[string]any) (any, error) {
				switch name {
				case "get_team", "get_project":
					return l1PeerResult(name, args)
				case "get_issue":
					return issue(), nil
				case "list_issue_labels":
					// Name uniqueness is checked over the full catalog only.
					if args["team"] != l1TeamA || args["includeArchived"] != true || args["includeGroups"] != true {
						t.Fatalf("label catalog was not the full team listing: %+v", args)
					}
					rows := []any{map[string]any{"id": l2LabelA, "name": labelName, "archivedAt": nil}, map[string]any{"id": l2LabelB, "name": "Synthetic Beta", "archivedAt": nil}}
					if mode == "ambiguous_name" {
						// e.g. children of two groups sharing a name.
						rows[1] = map[string]any{"id": l2LabelB, "name": labelName, "archivedAt": nil}
					}
					return map[string]any{"labels": rows, "hasNextPage": false}, nil
				case "list_issues":
					if label, _ := args["label"].(string); !linearprovider.ValidID(label) {
						t.Fatalf("a label name or non-UUID was sent to list_issues: %+v", args)
					}
					if args["label"] != l2LabelA || args["team"] != l1TeamA || args["project"] != l1Project || args["includeArchived"] != true {
						t.Fatalf("membership query was not scoped to the issue and label: %+v", args)
					}
					rows := []any{}
					if mode != "not_listed" {
						rows = append(rows, map[string]any{"id": "SYN-1", "uuid": l1IssueID, "teamId": l1TeamA, "projectId": l1Project, "title": "Synthetic scoped issue"})
					}
					return map[string]any{"issues": rows, "hasNextPage": false}, nil
				case "save_issue":
					saves = append(saves, args)
					labels = []any{}
					return issue(), nil
				default:
					t.Fatalf("unexpected native tool: %s", name)
					return nil, nil
				}
			}
			scope := f.request("", "w", "p", "worker", actor("worker", nil))
			plain, err := linearboard.New(cfg).IssueV2(context.Background(), scope, l1IssueID)
			if err != nil || plain.LabelsKnown || !reflect.DeepEqual(plain.LabelNames, []string{labelName}) {
				t.Fatal("plain read did not keep names-only labels unknown", plain, err)
			}
			for _, call := range peer.calls {
				if call.Name == "list_issues" || call.Name == "list_issue_labels" {
					t.Fatal("plain read made label-proof calls")
				}
			}
			in := linearboard.WriteInputV2{
				WriteInput:     linearboard.WriteInput{Operation: "update", OperationID: "native-label-clear-" + mode, IssueID: l1IssueID},
				Labels:         linearprovider.Field[[]string]{Present: true, Value: []string{}},
				ExpectedLabels: linearprovider.Field[[]string]{Present: true, Value: []string{l2LabelA}},
			}
			result, err := linearboard.New(cfg).WriteV2(context.Background(), scope, in)
			if mode != "proven" {
				if err == nil || result != nil || len(saves) != 0 {
					t.Fatal("unproven native label set was replaced", err, saves)
				}
				return
			}
			if err != nil || len(saves) != 1 || !reflect.DeepEqual(saves[0], map[string]any{"id": l1IssueID, "labels": []any{}}) {
				t.Fatal("proven native label clear did not send exactly an empty label set", err, saves)
			}
		})
	}
}
