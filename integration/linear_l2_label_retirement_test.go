package integration

import (
	"context"
	"testing"

	"clawbridge/internal/linearboard"
	"clawbridge/internal/linearprovider"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Preserve the public Service's scope/intent checks, while decoding label
// metadata through the actual provider. All provider responses and writes in
// this fixture are synthetic; no native credential or transport is opened.
type l2RetirementProvider struct {
	*l2IntentProvider
	catalog *linearprovider.Provider
}

func (p *l2RetirementProvider) Labels(ctx context.Context, team string, opts linearprovider.PageOptions) (linearprovider.Page[linearprovider.Label], error) {
	return p.catalog.Labels(ctx, team, opts)
}

func TestL2LabelRetiredAtPreventsApplication(t *testing.T) {
	for _, mode := range []string{"active_control", "retired_archived_false", "retired_archivedAt_null", "retired_and_archived"} {
		t.Run(mode, func(t *testing.T) {
			row := map[string]any{
				"id": l2LabelB, "name": "Synthetic team label", "teamId": l1TeamA,
				"isGroup": false, "archived": false, "retiredAt": nil,
			}
			if mode != "active_control" {
				row["retiredAt"] = "2026-10-21T00:00:00Z"
			}
			if mode == "retired_archivedAt_null" {
				row["archivedAt"] = nil
			}
			if mode == "retired_and_archived" {
				row["archived"] = true
				row["archivedAt"] = "2026-10-21T00:00:00Z"
			}
			tool := l1SchemaTool("list_issue_labels", map[string]string{"team": "string", "limit": "integer"}, "team")
			tool.Description = "A retired label carries retiredAt and cannot be applied."
			peer := &l1ProviderPeer{tools: []*mcp.Tool{tool}, result: func(name string, args map[string]any) (any, error) {
				if name != "list_issue_labels" || args["team"] != l1TeamA {
					t.Fatal("unscoped or unrelated catalog operation")
				}
				return map[string]any{"labels": []any{row}, "hasNextPage": false}, nil
			}}
			catalog, err := linearprovider.NewWithCaller(context.Background(), peer)
			if err != nil {
				t.Fatal(err)
			}
			defer catalog.Close()
			page, parseErr := catalog.Labels(context.Background(), l1TeamA, linearprovider.PageOptions{Limit: 100})
			eligible := parseErr == nil && len(page.Items) == 1 && page.Items[0].EligibilityKnown && !page.Items[0].Archived && !page.Items[0].Group && !page.Items[0].Global
			if mode == "active_control" && !eligible {
				t.Fatal("active, explicitly team-scoped non-group control was refused", parseErr)
			}
			if mode != "active_control" && eligible {
				t.Errorf("retiredAt metadata was ignored: provider declared retired label eligible: %+v", page.Items[0])
			}

			f := newFixture(t)
			f.setup("w", "p")
			f.call("taskboard_set_v2", "w", "p", "", l2MappingArgs(0))
			p := &l2RetirementProvider{l2IntentProvider: l2IntentFixture(), catalog: catalog}
			in := linearboard.WriteInputV2{
				WriteInput:     linearboard.WriteInput{Operation: "update", OperationID: "label-retirement-" + mode, IssueID: l1IssueID},
				Labels:         linearprovider.Field[[]string]{Present: true, Value: []string{l2LabelB}},
				ExpectedLabels: linearprovider.Field[[]string]{Present: true, Value: []string{l2LabelA, l2LabelB}},
			}
			result, writeErr := l1ServiceFor(t, f.store, p).WriteV2(context.Background(), f.request("", "w", "p", "worker", actor("worker", nil)), in)
			if mode == "active_control" {
				if writeErr != nil || len(p.updates) != 1 {
					t.Fatal("active control did not reach exactly one synthetic update", writeErr)
				}
			} else if writeErr == nil || result != nil || len(p.updates) != 0 {
				t.Fatalf("retired label reached mutation: synthetic_updates=%d err=%v", len(p.updates), writeErr)
			}
		})
	}
}
