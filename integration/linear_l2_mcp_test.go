package integration

import (
	"context"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"clawbridge/internal/bus"
	"clawbridge/internal/linearboard"
	"clawbridge/internal/mcpserver"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Deliberately expose only the original method: embedding *Service would
// accidentally opt a historical exact-seven-tool fixture into the V2 surface.
type l2LegacyOnlyTools struct{ service *linearboard.Service }

func (p l2LegacyOnlyTools) Execute(ctx context.Context, r bus.Request, op string, args map[string]any) (any, error) {
	return p.service.Execute(ctx, r, op, args)
}

type l2ToolInvocation struct {
	Scope     bus.Request
	Operation string
	Args      map[string]any
}
type l2MCPV1 struct {
	mu    sync.Mutex
	calls []l2ToolInvocation
}

func (p *l2MCPV1) Execute(_ context.Context, r bus.Request, op string, args map[string]any) (any, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls = append(p.calls, l2ToolInvocation{r, op, args})
	return map[string]any{"accepted_test_intent": true}, nil
}
func (p *l2MCPV1) count() int { p.mu.Lock(); defer p.mu.Unlock(); return len(p.calls) }

type l2MCPV2 struct{ *l2MCPV1 }

func (p *l2MCPV2) ExecuteV2(ctx context.Context, r bus.Request, op string, args map[string]any) (any, error) {
	return p.Execute(ctx, r, op, args)
}

func TestL2MCPDefaultLegacyAndV2ExactSurfaceWithoutIO(t *testing.T) {
	f := newFixture(t)
	f.setup("w", "p")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	v1 := &l2MCPV1{}
	v2 := &l2MCPV2{&l2MCPV1{}}
	var oldSchemas map[string]any
	for _, mode := range []string{"default", "v1_only", "v2_native"} {
		t.Run(mode, func(t *testing.T) {
			options := mcpserver.Options{Context: ctx}
			want := 20
			if mode == "v1_only" {
				options.Taskboard = v1
				want = 27
			}
			if mode == "v2_native" {
				options.Taskboard = v2
				want = 46
			}
			server := mcpserver.NewServerWithOptions(f.store, "w", "p", "worker", "worker-instance", options)
			client := notificationMCPConnect(t, ctx, server, make(chan string, 1))
			listed, err := client.ListTools(ctx, nil)
			if err != nil {
				t.Fatal(err)
			}
			if len(listed.Tools) != want {
				t.Fatalf("tool count=%d want%d", len(listed.Tools), want)
			}
			if v1.count() != 0 || v2.count() != 0 {
				t.Fatal("initialize/tools-list performed taskboard IO")
			}
			legacy := map[string]any{}
			v2Names := map[string]bool{}
			for _, tool := range listed.Tools {
				if !strings.HasPrefix(tool.Name, "taskboard_") {
					continue
				}
				if !strings.HasSuffix(tool.Name, "_v2") {
					legacy[tool.Name] = jsonData(t, tool.InputSchema)
					continue
				}
				v2Names[tool.Name] = true
				schema := object(t, jsonData(t, tool.InputSchema))
				if schema["additionalProperties"] != false {
					t.Fatal("V2 tool accepts unknown routing/args", tool.Name)
				}
				if strings.Contains(tool.Name, "create") || strings.Contains(tool.Name, "update") {
					if !strings.Contains(strings.ToLower(tool.Description), "replace") {
						t.Fatal("label mutation help omits replacement semantics")
					}
				}
			}
			if mode == "v1_only" {
				oldSchemas = legacy
				if len(legacy) != 7 {
					t.Fatal("legacy adapter lost exact7tools")
				}
			}
			if mode == "v2_native" {
				if !reflect.DeepEqual(legacy, oldSchemas) {
					t.Fatal("V2 opt-in changed seven legacy schemas")
				}
				wantNames := map[string]bool{}
				for _, op := range []string{"binding", "context", "issues", "issue", "create", "update", "comment", "labels", "members", "views", "relation_add", "relation_remove", "project_status", "project_status_set", "issue_move", "project_add_team", "project_plan"} {
					wantNames["taskboard_"+op+"_v2"] = true
				}
				if !reflect.DeepEqual(v2Names, wantNames) {
					t.Fatalf("unexpected V2 tools: %v", v2Names)
				}
			}
		})
	}
}

func TestL2MCPV2ClosedTriStateAndPinnedScope(t *testing.T) {
	f := newFixture(t)
	f.setup("w", "p")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	p := &l2MCPV2{&l2MCPV1{}}
	server := mcpserver.NewServerWithOptions(f.store, "w", "p", "worker", "worker-instance", mcpserver.Options{Context: ctx, Taskboard: p})
	client := notificationMCPConnect(t, ctx, server, make(chan string, 1))
	cases := []struct {
		name  string
		args  map[string]any
		valid bool
	}{
		{"omit", map[string]any{"id": l1IssueID, "operation_id": "omit", "title": "synthetic title"}, true},
		{"explicit_nulls", map[string]any{"id": l1IssueID, "operation_id": "nulls", "project_id": nil, "parent_id": nil, "assignee_id": nil, "label_ids": []string{}, "expected_label_ids": []string{l2LabelA, l2LabelB}}, true},
		{"duplicate_expected_labels", map[string]any{"id": l1IssueID, "operation_id": "bad", "label_ids": []string{}, "expected_label_ids": []string{l2LabelA, l2LabelA}}, false},
		{"null_labels", map[string]any{"id": l1IssueID, "operation_id": "bad", "label_ids": nil}, false},
		{"duplicate_labels", map[string]any{"id": l1IssueID, "operation_id": "bad", "label_ids": []string{l2LabelA, l2LabelA}}, false},
		{"wrong_parent_type", map[string]any{"id": l1IssueID, "operation_id": "bad", "parent_id": true}, false},
		{"team_move", map[string]any{"id": l1IssueID, "operation_id": "bad", "team_id": l1TeamB}, false},
	}
	for _, field := range []string{"workspace", "project", "session", "instance", "principal", "server", "profile_id", "provider", "expected_revision"} {
		cases = append(cases, struct {
			name  string
			args  map[string]any
			valid bool
		}{"override_" + field, map[string]any{"id": l1IssueID, "operation_id": "bad", "title": "synthetic", field: "foreign"}, false})
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			before := p.count()
			result, err := client.CallTool(ctx, &mcp.CallToolParams{Name: "taskboard_update_v2", Arguments: tc.args})
			if !tc.valid {
				if err == nil && !result.IsError {
					t.Fatal("invalid/overriding args accepted")
				}
				if p.count() != before {
					t.Fatal("invalid arguments reached adapter")
				}
				return
			}
			if err != nil || result.IsError || p.count() != before+1 {
				t.Fatal("valid tri-state argument shape rejected", err)
			}
			p.mu.Lock()
			got := p.calls[len(p.calls)-1]
			p.mu.Unlock()
			if got.Scope.Workspace != "w" || got.Scope.Project != "p" || got.Scope.Session != "worker" || got.Scope.Args["instance"] != "worker-instance" || len(got.Scope.Args) != 1 {
				t.Fatal("MCP adapter did not receive immutable process scope")
			}
			if tc.name == "omit" {
				if _, ok := got.Args["label_ids"]; ok {
					t.Fatal("omitted labels invented")
				}
				if _, ok := got.Args["parent_id"]; ok {
					t.Fatal("omitted parent invented")
				}
			}
			if tc.name == "explicit_nulls" {
				for _, key := range []string{"project_id", "parent_id", "assignee_id"} {
					if value, ok := got.Args[key]; !ok || value != nil {
						t.Fatal("explicit null lost", key)
					}
				}
				if labels, ok := got.Args["label_ids"].([]any); !ok || len(labels) != 0 {
					t.Fatal("explicit empty labels lost")
				}
				if expected, ok := got.Args["expected_label_ids"].([]any); !ok || len(expected) != 2 {
					t.Fatal("expected label set lost")
				}
			}
		})
	}
}
