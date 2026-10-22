package integration

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"clawbridge/internal/linearboard"
	"clawbridge/internal/linearprovider"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func l2ActualSaveIssueTool(t *testing.T) *mcp.Tool {
	t.Helper()
	raw, err := os.ReadFile("testdata/linear-l2-official-save-issue.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Tool       mcp.Tool `json:"tool"`
		Provenance struct {
			Catalog string `json:"catalog_sha256"`
		} `json:"provenance"`
	}
	if err := json.Unmarshal(raw, &fixture); err != nil {
		t.Fatal(err)
	}
	if fixture.Provenance.Catalog != "c735d67583bb462a7433b79584aeae35934557c6300fff83be3e39f7ba776836" {
		t.Fatal("official schema fixture attribution changed")
	}
	return &fixture.Tool
}

func TestL2OfficialSaveIssueOneEdgeUsesOnlyCanonicalEndpointArray(t *testing.T) {
	for _, action := range []string{"add", "remove"} {
		for _, kind := range []string{"blocks", "related"} {
			t.Run(action+"/"+kind, func(t *testing.T) {
				peer := &l1ProviderPeer{tools: []*mcp.Tool{l2ActualSaveIssueTool(t)}}
				peer.result = func(name string, args map[string]any) (any, error) {
					return map[string]any{"id": "SYN-EDGE-1", "uuid": l1IssueID, "description": "UNTRUSTED_ISSUE_BODY_NOT_AN_EDGE_RECEIPT"}, nil
				}
				p, err := linearprovider.NewWithCaller(context.Background(), peer)
				if err != nil {
					t.Fatal(err)
				}
				defer p.Close()
				caps, err := p.Capabilities(context.Background())
				if err != nil || !caps.RelationAdd || !caps.RelationRemove || caps.ParentPreservesDestination || len(peer.calls) != 0 {
					t.Fatal("actual schema negotiation claimed unsafe parent semantics or invoked provider", caps, err)
				}
				r, err := p.Relation(context.Background(), linearprovider.RelationInput{Action: action, Type: kind, FromID: l1IssueID, ToID: l1StateID})
				if err != nil || r.ID != "" || r.Action != action || r.Type != kind || r.FromID != l1IssueID || r.ToID != l1StateID {
					t.Fatal("single-edge receipt fabricated ID/body or lost canonical endpoints", r, err)
				}
				key := "blocks"
				if kind == "related" {
					key = "relatedTo"
				}
				if action == "remove" {
					key = "removeBlocks"
					if kind == "related" {
						key = "removeRelatedTo"
					}
				}
				want := map[string]any{"id": l1IssueID, key: []any{l1StateID}}
				if len(peer.calls) != 1 || peer.calls[0].Name != "save_issue" || !reflect.DeepEqual(peer.calls[0].Args, want) {
					t.Fatalf("must mutate exactly one endpoint, without relation-list reads/replacement or unrelated fields: %+v", peer.calls)
				}
			})
		}
	}
}

func TestL2OfficialSaveIssueUnsafeSemanticsRefusedBeforeDispatch(t *testing.T) {
	for _, mode := range []string{"replacement", "missing_semantics", "wrong_items", "required_unrelated", "remove_replacement"} {
		t.Run(mode, func(t *testing.T) {
			tool := l2ActualSaveIssueTool(t)
			schema := object(t, jsonData(t, tool.InputSchema))
			props := object(t, schema["properties"])
			action := "add"
			switch mode {
			case "replacement":
				object(t, props["blocks"])["description"] = "Replace every relationship with this full list"
			case "missing_semantics":
				delete(object(t, props["blocks"]), "description")
			case "wrong_items":
				object(t, props["blocks"])["items"] = map[string]any{"type": "object"}
			case "required_unrelated":
				schema["required"] = []string{"title"}
			case "remove_replacement":
				action = "remove"
				object(t, props["removeBlocks"])["description"] = "Replace the full relationship list"
			}
			tool.InputSchema = schema
			peer := &l1ProviderPeer{tools: []*mcp.Tool{tool}, result: func(string, map[string]any) (any, error) { return nil, errors.New("should never dispatch") }}
			p, err := linearprovider.NewWithCaller(context.Background(), peer)
			if err != nil {
				t.Fatal(err)
			}
			defer p.Close()
			if _, err := p.Relation(context.Background(), linearprovider.RelationInput{Action: action, Type: "blocks", FromID: l1IssueID, ToID: l1StateID}); err == nil || len(peer.calls) != 0 {
				t.Fatal("unsafe schema sent a mutation", err, peer.calls)
			}
		})
	}
}

func TestL2OfficialSchemaParentSetNeedsProofAndClearUsesExactNull(t *testing.T) {
	peer := &l1ProviderPeer{tools: []*mcp.Tool{l2ActualSaveIssueTool(t)}, result: func(string, map[string]any) (any, error) {
		return map[string]any{"id": l1IssueID, "teamId": l1TeamA, "projectId": l1Project, "parentId": nil}, nil
	}}
	p, err := linearprovider.NewWithCaller(context.Background(), peer)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	// The same real schema supports nullable ordinary intent, so refusing a
	// parent is specifically a missing semantic guarantee, not a dead fixture.
	control := linearprovider.UpdateInputV2{UpdateInput: linearprovider.UpdateInput{ID: l1IssueID}, Assignee: linearprovider.Field[*string]{Present: true}}
	if err := p.ValidateUpdateV2(control); err != nil {
		t.Fatal("actual nullable assignee schema control did not negotiate", err)
	}
	parent := l1StateID
	in := linearprovider.UpdateInputV2{UpdateInput: linearprovider.UpdateInput{ID: l1IssueID}, Parent: linearprovider.Field[*string]{Present: true, Value: &parent}}
	if err := p.ValidateUpdateV2(in); err == nil {
		t.Fatal("ordinary parent/project fields manufactured destination-preservation proof")
	}
	if issue, err := p.UpdateV2(context.Background(), in); err == nil || !reflect.DeepEqual(issue, linearprovider.IssueV2{}) || len(peer.calls) != 0 {
		t.Fatal("unproved native parent set dispatched", err, peer.calls)
	}
	// Explicitly removing a parent has no new parent from which to inherit.
	// The actual schema's documented null-removal form is checked separately;
	// this is adapter-shape evidence, not a native mutation/preservation claim.
	in.Parent.Value = nil
	if err := p.ValidateUpdateV2(in); err != nil {
		t.Fatal("documented explicit parent-null form did not negotiate", err)
	}
	if _, err := p.UpdateV2(context.Background(), in); err != nil || len(peer.calls) != 1 || !reflect.DeepEqual(peer.calls[0].Args, map[string]any{"id": l1IssueID, "parentId": nil}) {
		t.Fatal("parent clear sent inheritance or unrelated fields", err, peer.calls)
	}
}

func TestL2OfficialSaveIssueServicePendingPrecedesDispatchAndNeverReplays(t *testing.T) {
	f := newFixture(t)
	f.setup("w", "p")
	f.call("taskboard_set_v2", "w", "p", "", l2MappingArgs(0))
	peer := newL1ProviderPeer()
	peer.tools = []*mcp.Tool{peer.tools[0], peer.tools[1], peer.tools[2], l2ActualSaveIssueTool(t)}
	cfg := l1ServiceConfig(t, f.store, &l1SyntheticProvider{})
	cfg.Open = func(ctx context.Context, _ *http.Client, _ bool) (linearboard.Provider, error) {
		return linearprovider.NewWithCaller(ctx, peer)
	}
	dispatches := 0
	peer.result = func(name string, args map[string]any) (any, error) {
		switch name {
		case "get_team", "get_project":
			return l1PeerResult(name, args)
		case "get_issue":
			id := args["id"].(string)
			team, project := l1TeamA, l1Project
			if id == l1StateID {
				team, project = l1TeamB, l2ProjectB
			}
			return map[string]any{"id": id, "teamId": team, "projectId": project, "title": "Synthetic scoped issue", "description": "SYNTHETIC_PRIVATE_SCOPE_BODY"}, nil
		case "save_issue":
			dispatches++
			if !reflect.DeepEqual(args, map[string]any{"id": l1StateID, "blocks": []any{l1IssueID}}) {
				t.Fatalf("inverse direction or unrelated mutation fields: %+v", args)
			}
			// The operation lock is legitimately live inside dispatch. Inspect
			// only the persisted record here; the post-return helper separately
			// asserts that no lock or staging files remain.
			files, globErr := filepath.Glob(filepath.Join(cfg.LedgerDirectory, "*", "*.json"))
			if globErr != nil || len(files) != 1 {
				t.Fatal("durable ledger absent before fake acceptance")
			}
			for _, path := range files {
				raw, readErr := os.ReadFile(path)
				var state map[string]any
				if readErr != nil || json.Unmarshal(raw, &state) != nil || state["state"] != "pending" {
					t.Fatal("not durably pending before provider mutation")
				}
			}
			return nil, errors.New("SYNTHETIC_SECRET_ERROR lost response AFTER fake acceptance")
		default:
			t.Fatalf("arbitrary native tool invoked: %s", name)
			return nil, nil
		}
	}
	scope := f.request("", "w", "p", "worker", actor("worker", nil))
	in := linearboard.RelationInputV2{OperationID: "native-one-edge-lost", Action: "add", Type: "blocked-by", FromID: l1IssueID, ToID: l1StateID}
	result, err := linearboard.New(cfg).RelationV2(context.Background(), scope, in)
	if err == nil || result != nil || !strings.Contains(err.Error(), "uncertain") || strings.Contains(err.Error(), "SYNTHETIC_SECRET_ERROR") || dispatches != 1 {
		t.Fatal("lost response was leaked/replayed or claimed accepted", err, dispatches)
	}
	before := l1LedgerBytes(t, cfg.LedgerDirectory)
	beforeCalls := len(peer.calls)
	forbidden := new(l2ForbiddenConnections)
	cfg.Connections = forbidden
	if _, err := linearboard.New(cfg).RelationV2(context.Background(), scope, in); err == nil || !strings.Contains(err.Error(), "uncertain") {
		t.Fatal("pending native edge retry not refused", err)
	}
	if dispatches != 1 || len(peer.calls) != beforeCalls || forbidden.calls.Load() != 0 || !reflect.DeepEqual(before, l1LedgerBytes(t, cfg.LedgerDirectory)) {
		t.Fatal("pending retry touched native route or rewrote ledger")
	}
}
