package integration

import (
	"context"
	"net/http"
	"reflect"
	"strings"
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
			peer.tools = []*mcp.Tool{peer.tools[0], peer.tools[1], peer.tools[2], l2NativeLabelListTool(), l2NativeIssueListTool(), l2ActualSaveIssueTool(t)}
			cfg := l1ServiceConfig(t, f.store, &l1SyntheticProvider{})
			cfg.Open = func(ctx context.Context, _ *http.Client, _ bool) (linearboard.Provider, error) {
				return linearprovider.NewWithCaller(ctx, peer)
			}
			target := l2NativeLabelRow(l2LabelB)
			target["name"] = "Synthetic label B"
			switch mode {
			case "retired":
				target["retiredAt"] = "2026-01-01T00:00:00Z"
			case "group":
				target["isGroup"] = true
				target["groupType"] = "singleSelect"
			}
			saves := []map[string]any{}
			peer.result = func(name string, args map[string]any) (any, error) {
				switch name {
				case "get_team", "get_project":
					return l1PeerResult(name, args)
				case "get_issue":
					// Native get_issue: display id, canonical uuid, labels by name.
					return map[string]any{"id": "SYN-1", "uuid": l1IssueID, "teamId": l1TeamA, "projectId": l1Project, "title": "Synthetic scoped issue", "labels": []any{"Synthetic label"}}, nil
				case "list_issue_labels":
					if args["team"] != l1TeamA {
						t.Fatalf("label list was not pinned to the issue team: %+v", args)
					}
					// The full catalog (both filters true) proves the current label
					// names; eligibility is only proven when both are sent as false.
					full := args["includeArchived"] == true && args["includeGroups"] == true
					if !full && (args["includeArchived"] != false || args["includeGroups"] != false) {
						t.Fatalf("label list did not pin the eligibility filters: %+v", args)
					}
					return map[string]any{"labels": []any{l2NativeLabelRow(l2LabelA), target}, "hasNextPage": false}, nil
				case "list_issues":
					// Membership proof for the issue's current label, by UUID.
					if args["label"] != l2LabelA || args["team"] != l1TeamA || args["project"] != l1Project {
						t.Fatalf("membership query was not scoped to the issue and label: %+v", args)
					}
					return map[string]any{"issues": []any{map[string]any{"id": "SYN-1", "uuid": l1IssueID, "teamId": l1TeamA, "projectId": l1Project, "title": "Synthetic scoped issue", "labels": []any{"Synthetic label"}}}, "hasNextPage": false}, nil
				case "save_issue":
					saves = append(saves, args)
					return map[string]any{"id": "SYN-1", "uuid": l1IssueID, "teamId": l1TeamA, "projectId": l1Project, "title": "Synthetic scoped issue", "labels": []any{"Synthetic label", "Synthetic label B"}}, nil
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
				// Each refusal names its own reason, so a refusal for an unrelated
				// cause cannot stand in for the one under test.
				want := map[string]string{
					"retired":           "label is not proven eligible for application",
					"group":             "Linear provider: scope_mismatch",
					"stale_expectation": "issue labels differ from expected_label_ids",
				}[mode]
				if err == nil || !strings.Contains(err.Error(), want) || result != nil || len(saves) != 0 {
					t.Fatal("retired/group/stale native label reached mutation or was refused for another reason", want, err, saves)
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
				return map[string]any{"id": "SYN-1", "uuid": l1IssueID, "teamId": l1TeamA, "projectId": l1Project, "title": "Synthetic scoped issue", "labels": labels}
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
				if err == nil || !strings.Contains(err.Error(), "existing canonical label context is unavailable; replacement refused") || result != nil || len(saves) != 0 {
					t.Fatal("unproven native label set was replaced or refused for another reason", err, saves)
				}
				return
			}
			if err != nil || len(saves) != 1 || !reflect.DeepEqual(saves[0], map[string]any{"id": l1IssueID, "labels": []any{}}) {
				t.Fatal("proven native label clear did not send exactly an empty label set", err, saves)
			}
		})
	}
}

// Native list rows name their labels. A label-filtered list is accepted only
// when every names-only row carries the selected label's name, unique in the
// team's full catalog, so a filter Linear silently ignored cannot pass rows
// with other labels.
func TestL2NativeLabelFilteredIssueListProvesNames(t *testing.T) {
	const labelName = "Synthetic label"
	for _, mode := range []string{"filtered", "filter_ignored", "ambiguous_name"} {
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
			peer.result = func(name string, args map[string]any) (any, error) {
				switch name {
				case "get_team", "get_project":
					return l1PeerResult(name, args)
				case "list_issue_labels":
					rows := []any{l2NativeLabelRow(l2LabelA)}
					if mode == "ambiguous_name" && args["includeGroups"] == true {
						// Only the full catalog shows a second label with the name.
						rows = append(rows, l2NativeLabelRow(l2LabelB))
					}
					return map[string]any{"labels": rows, "hasNextPage": false}, nil
				case "list_issues":
					if args["label"] != l2LabelA {
						t.Fatalf("label filter was not sent as the UUID: %+v", args)
					}
					rows := []any{map[string]any{"id": "SYN-1", "uuid": l1IssueID, "teamId": l1TeamA, "projectId": l1Project, "title": "Synthetic labelled issue", "labels": []any{labelName}}}
					if mode == "filter_ignored" {
						rows = append(rows, map[string]any{"id": "SYN-2", "uuid": "dddddddd-dddd-4ddd-8ddd-dddddddddddd", "teamId": l1TeamA, "projectId": l1Project, "title": "Synthetic other issue", "labels": []any{"Synthetic other"}})
					}
					return map[string]any{"issues": rows, "hasNextPage": false}, nil
				default:
					t.Fatalf("unexpected native tool: %s", name)
					return nil, nil
				}
			}
			page, err := linearboard.New(cfg).IssuesV2(context.Background(), f.request("", "w", "p", "worker", actor("worker", nil)), linearboard.IssuesInputV2{Limit: 50, LabelID: l2LabelA})
			if mode != "filtered" {
				// The board's own proof must refuse, not an unrelated failure.
				if err == nil || !strings.Contains(err.Error(), "provider did not prove the selected label filter") || len(page.Items) != 0 {
					t.Fatal("unproven label-filtered list was not refused by the label proof", page.Items, err)
				}
				return
			}
			if err != nil || len(page.Items) != 1 || page.Items[0].ID != l1IssueID || page.Items[0].LabelsKnown {
				t.Fatal("proven names-only label filter was refused or overclaimed", page, err)
			}
		})
	}
}
