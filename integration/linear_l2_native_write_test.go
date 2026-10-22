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
				// save_issue alone can express a parent beside a project, but
				// without get_issue nothing can verify a same-destination parent.
				if err != nil || !caps.RelationAdd || !caps.RelationRemove || caps.ParentSameDestination || len(peer.calls) != 0 {
					t.Fatal("actual schema negotiation claimed parent support without issue reads or invoked provider", caps, err)
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
		t.Fatal("ordinary parent/project fields allowed a parent set without the board's same-destination verification")
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

// Native Linear reports issue ids and parentId as display identifiers, with
// the canonical UUID beside them. A same-destination parent set and a clear
// must go through the public write, and parented reads must keep working.
func TestL2NativeIdentifierParentSetClearAndRead(t *testing.T) {
	const parentUUID = "dddddddd-dddd-4ddd-8ddd-dddddddddddd"
	f := newFixture(t)
	f.setup("w", "p")
	f.call("taskboard_set_v2", "w", "p", "", l2MappingArgs(0))
	peer := newL1ProviderPeer()
	peer.tools = []*mcp.Tool{peer.tools[0], peer.tools[1], peer.tools[2], l2ActualSaveIssueTool(t)}
	cfg := l1ServiceConfig(t, f.store, &l1SyntheticProvider{})
	cfg.Open = func(ctx context.Context, _ *http.Client, _ bool) (linearboard.Provider, error) {
		return linearprovider.NewWithCaller(ctx, peer)
	}
	identifiers := map[string]string{l1IssueID: "SYN-1", parentUUID: "SYN-2"}
	parents := map[string]any{l1IssueID: nil, parentUUID: nil}
	native := func(id string) map[string]any {
		return map[string]any{"id": identifiers[id], "uuid": id, "identifier": identifiers[id], "teamId": l1TeamA, "projectId": l1Project, "title": "Synthetic scoped issue", "parentId": parents[id]}
	}
	lookup := func(ref string) string {
		for id, identifier := range identifiers {
			if ref == id || ref == identifier {
				return id
			}
		}
		t.Fatalf("unknown synthetic issue reference %q", ref)
		return ""
	}
	saves := []map[string]any{}
	peer.result = func(name string, args map[string]any) (any, error) {
		switch name {
		case "get_team", "get_project":
			return l1PeerResult(name, args)
		case "get_issue":
			return native(lookup(args["id"].(string))), nil
		case "save_issue":
			saves = append(saves, args)
			id := lookup(args["id"].(string))
			if parent, ok := args["parentId"]; ok {
				parents[id] = nil
				if parent != nil {
					parents[id] = identifiers[lookup(parent.(string))]
				}
			}
			return native(id), nil
		default:
			t.Fatalf("unexpected native tool: %s", name)
			return nil, nil
		}
	}
	scope := f.request("", "w", "p", "worker", actor("worker", nil))
	parent := parentUUID
	set := linearboard.WriteInputV2{WriteInput: linearboard.WriteInput{Operation: "update", OperationID: "native-parent-set", IssueID: l1IssueID}, Parent: linearprovider.Field[*string]{Present: true, Value: &parent}}
	if _, err := linearboard.New(cfg).WriteV2(context.Background(), scope, set); err != nil {
		t.Fatal("same-destination parent with an identifier receipt refused", err)
	}
	if len(saves) != 1 || !reflect.DeepEqual(saves[0], map[string]any{"id": l1IssueID, "parentId": parentUUID}) {
		t.Fatalf("parent set sent unrelated fields or a non-canonical parent: %+v", saves)
	}
	read, err := linearboard.New(cfg).ExecuteV2(context.Background(), scope, "issue", map[string]any{"id": l1IssueID})
	if err != nil {
		t.Fatal("identifier-form parentId broke the parented issue read", err)
	}
	if issue := object(t, jsonData(t, read)); !strings.Contains(string(mustJSON(t, issue)), `"SYN-2"`) {
		t.Fatalf("parented read lost the parent identifier: %v", issue)
	}
	clear := linearboard.WriteInputV2{WriteInput: linearboard.WriteInput{Operation: "update", OperationID: "native-parent-clear", IssueID: l1IssueID}, Parent: linearprovider.Field[*string]{Present: true}}
	if _, err := linearboard.New(cfg).WriteV2(context.Background(), scope, clear); err != nil {
		t.Fatal("parent clear refused", err)
	}
	if len(saves) != 2 || !reflect.DeepEqual(saves[1], map[string]any{"id": l1IssueID, "parentId": nil}) {
		t.Fatalf("parent clear sent inheritance or unrelated fields: %+v", saves)
	}
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}
