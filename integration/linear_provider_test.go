package integration

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"

	"clawbridge/internal/linearboard"
	"clawbridge/internal/linearprovider"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type l1ProviderCall struct {
	Name string
	Args map[string]any
}
type l1ProviderPeer struct {
	mu     sync.Mutex
	calls  []l1ProviderCall
	result func(string, map[string]any) (any, error)
	tools  []*mcp.Tool
}

func l1SchemaTool(name string, fields map[string]string, required ...string) *mcp.Tool {
	p := map[string]any{}
	for k, kind := range fields {
		p[k] = map[string]any{"type": kind}
	}
	return &mcp.Tool{Name: name, InputSchema: map[string]any{"type": "object", "properties": p, "required": required, "additionalProperties": false}}
}
func newL1ProviderPeer() *l1ProviderPeer {
	return &l1ProviderPeer{tools: []*mcp.Tool{
		l1SchemaTool("get_team", map[string]string{"id": "string"}, "id"),
		l1SchemaTool("get_project", map[string]string{"id": "string"}, "id"),
		l1SchemaTool("get_issue", map[string]string{"id": "string"}, "id"),
		l1SchemaTool("list_issues", map[string]string{"teamId": "string", "projectId": "string", "limit": "integer", "cursor": "string"}, "teamId", "limit"),
		l1SchemaTool("list_issue_statuses", map[string]string{"teamId": "string"}, "teamId"),
		l1SchemaTool("create_issue", map[string]string{"teamId": "string", "projectId": "string", "title": "string", "description": "string", "stateId": "string"}, "teamId", "title"),
		l1SchemaTool("update_issue", map[string]string{"id": "string", "title": "string", "description": "string", "stateId": "string"}, "id"),
		l1SchemaTool("create_comment", map[string]string{"issueId": "string", "body": "string"}, "issueId", "body"),
		l1SchemaTool("delete_issue", map[string]string{"id": "string"}, "id"),
	}}
}
func (p *l1ProviderPeer) ListTools(context.Context, *mcp.ListToolsParams) (*mcp.ListToolsResult, error) {
	return &mcp.ListToolsResult{Tools: p.tools}, nil
}
func (p *l1ProviderPeer) CallTool(ctx context.Context, in *mcp.CallToolParams) (*mcp.CallToolResult, error) {
	b, _ := json.Marshal(in.Arguments)
	args := map[string]any{}
	if err := json.Unmarshal(b, &args); err != nil {
		return nil, err
	}
	p.mu.Lock()
	p.calls = append(p.calls, l1ProviderCall{in.Name, args})
	p.mu.Unlock()
	v, err := p.result(in.Name, args)
	if err != nil {
		return nil, err
	}
	return &mcp.CallToolResult{StructuredContent: v}, nil
}
func l1PeerResult(name string, args map[string]any) (any, error) {
	switch name {
	case "get_team":
		return map[string]any{"id": args["id"], "name": "duplicate team name"}, nil
	case "get_project":
		return map[string]any{"id": args["id"], "name": "duplicate project name", "teams": []any{map[string]any{"id": l1TeamA}, map[string]any{"id": l1TeamB}}}, nil
	case "get_issue":
		return map[string]any{"id": l1IssueID, "teamId": l1TeamA, "projectId": l1Project, "title": "synthetic title", "description": "scope-protected synthetic body"}, nil
	default:
		return nil, fmt.Errorf("unexpected synthetic provider operation %s", name)
	}
}

func TestL1ProviderConflictingScopeCannotExposeOrMutate(t *testing.T) {
	const foreign = "88888888-8888-4888-8888-888888888888"
	for _, kind := range []string{"team", "project", "team-type", "project-type", "project-null-conflict", "state-team"} {
		t.Run(kind, func(t *testing.T) {
			f := newFixture(t)
			f.setup("w", "p")
			args := l1MappingArgs(0)
			args["linear_project_id"] = l1Project
			f.call("taskboard_set", "w", "p", "", args)
			peer := newL1ProviderPeer()
			peer.result = func(name string, args map[string]any) (any, error) {
				if name == "get_issue" {
					v, _ := l1PeerResult(name, args)
					issue := v.(map[string]any)
					if kind == "team" {
						issue["teamId"] = foreign
						issue["team"] = map[string]any{"id": l1TeamA}
					}
					if kind == "project" {
						issue["projectId"] = foreign
						issue["project"] = map[string]any{"id": l1Project}
					}
					if kind == "team-type" {
						issue["team"] = 42
					}
					if kind == "project-type" {
						issue["project"] = 42
					}
					if kind == "project-null-conflict" {
						issue["project"] = nil
					}
					return issue, nil
				}
				if name == "list_issue_statuses" {
					return map[string]any{"statuses": []any{map[string]any{"id": l1StateID, "teamId": foreign, "team": map[string]any{"id": l1TeamA}, "name": "conflicting state"}}, "hasNextPage": false}, nil
				}
				if name == "update_issue" {
					return l1PeerResult("get_issue", args)
				}
				return l1PeerResult(name, args)
			}
			provider, err := linearprovider.NewWithCaller(context.Background(), peer)
			if err != nil {
				t.Fatal(err)
			}
			cfg := l1ServiceConfig(t, f.store, provider)
			cfg.Open = func(context.Context, *http.Client, bool) (linearboard.Provider, error) { return provider, nil }
			service := linearboard.New(cfg)
			scope := f.request("", "w", "p", "worker", actor("worker", nil))
			if kind != "state-team" {
				if _, err := service.Issue(context.Background(), scope, l1IssueID); err == nil {
					t.Error("ambiguous flat/nested immutable scope returned an authorized body")
				}
			}
			state := l1StateID
			description := "must not dispatch from ambiguous context"
			if _, err := service.Write(context.Background(), scope, linearboard.WriteInput{Operation: "update", OperationID: "scope-conflict", IssueID: l1IssueID, Description: &description, StateID: &state}); err == nil {
				t.Error("ambiguous scope accepted provider mutation")
			}
			for _, call := range peer.calls {
				if call.Name == "update_issue" {
					t.Error("ambiguous immutable scope reached actual curated provider mutation")
				}
			}
		})
	}
}

func TestL1ProviderDescriptiveLabelsNeverSupplyOrOverrideScope(t *testing.T) {
	for _, mode := range []string{"valid-ids", "team-label-only", "project-label-only", "null-team-with-label", "null-project-with-label"} {
		t.Run(mode, func(t *testing.T) {
			f := newFixture(t)
			f.setup("w", "p")
			args := l1MappingArgs(0)
			args["linear_project_id"] = l1Project
			f.call("taskboard_set", "w", "p", "", args)
			peer := newL1ProviderPeer()
			peer.result = func(name string, args map[string]any) (any, error) {
				v, err := l1PeerResult(name, args)
				if err != nil {
					return nil, err
				}
				if name == "get_issue" {
					issue := v.(map[string]any)
					issue["team"] = "same-name entirely unrelated team label"
					issue["project"] = "a different arbitrary project label"
					switch mode {
					case "team-label-only":
						delete(issue, "teamId")
					case "project-label-only":
						delete(issue, "projectId")
					case "null-team-with-label":
						issue["teamId"] = nil
					case "null-project-with-label":
						issue["projectId"] = nil
					}
				}
				return v, nil
			}
			provider, err := linearprovider.NewWithCaller(context.Background(), peer)
			if err != nil {
				t.Fatal(err)
			}
			service := l1ServiceFor(t, f.store, provider)
			scope := f.request("", "w", "p", "worker", actor("worker", nil))
			issue, err := service.Issue(context.Background(), scope, l1IssueID)
			if mode == "valid-ids" {
				if err != nil || issue.TeamID != l1TeamA || issue.ProjectID != l1Project {
					t.Fatal("descriptive labels incorrectly replaced/rejected verified immutable IDs", err)
				}
			} else if err == nil {
				t.Fatal("unverified descriptive label supplied missing immutable scope")
			}
		})
	}
}

func TestL1ProviderCuratedAliasesNarrowWritesAndContextRedaction(t *testing.T) {
	peer := newL1ProviderPeer()
	peer.result = func(name string, args map[string]any) (any, error) {
		if name == "get_issue" || name == "update_issue" || name == "create_issue" {
			v, _ := l1PeerResult("get_issue", args)
			issue := v.(map[string]any)
			issue["parent"] = map[string]any{"id": "88888888-8888-4888-8888-888888888888", "identifier": "PARENT-1", "description": "FOREIGN-NESTED-BODY-MUST-NOT-LEAK", "comments": []any{map[string]any{"body": "FOREIGN-NESTED-COMMENT"}}}
			issue["assignee"] = map[string]any{"name": "human owner"}
			return issue, nil
		}
		return l1PeerResult(name, args)
	}
	p, err := linearprovider.NewWithCaller(context.Background(), peer)
	if err != nil {
		t.Fatal(err)
	}
	issue, err := p.GetIssue(context.Background(), l1IssueID)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(issue)
	if stringsContainsAny(string(b), "FOREIGN-NESTED-BODY-MUST-NOT-LEAK", "FOREIGN-NESTED-COMMENT") {
		t.Fatal("unverified linked body/comment leaked through parent context")
	}
	if issue.ContextComplete || issue.Assignee != "human owner" {
		t.Fatal("context completeness or human assignee was fabricated")
	}
	description := "explicit replacement description"
	if _, err := p.Update(context.Background(), linearprovider.UpdateInput{ID: l1IssueID, Description: &description}); err != nil {
		t.Fatal(err)
	}
	last := peer.calls[len(peer.calls)-1]
	if last.Name != "update_issue" || len(last.Args) != 2 || last.Args["id"] != l1IssueID || last.Args["description"] != description {
		t.Fatalf("narrow update invented fields/assignee/passthrough: %v", last)
	}
	if _, err := p.Create(context.Background(), linearprovider.CreateInput{TeamID: l1TeamA, ProjectID: l1Project, Title: "new", Description: "body"}); err != nil {
		t.Fatal(err)
	}
	for _, call := range peer.calls {
		if call.Name == "delete_issue" {
			t.Fatal("untrusted advertised capability used")
		}
	}
	// Removing curated aliases cannot fall through to an advertised admin tool.
	peer.tools = []*mcp.Tool{l1SchemaTool("delete_issue", map[string]string{"id": "string"}, "id")}
	p, err = linearprovider.NewWithCaller(context.Background(), peer)
	if err != nil {
		t.Fatal(err)
	}
	before := len(peer.calls)
	if _, err := p.Update(context.Background(), linearprovider.UpdateInput{ID: l1IssueID, Description: &description}); err == nil || len(peer.calls) != before {
		t.Fatal("missing curated capability did not fail before dispatch")
	}
}

func stringsContainsAny(text string, values ...string) bool {
	for _, v := range values {
		if strings.Contains(text, v) {
			return true
		}
	}
	return false
}
