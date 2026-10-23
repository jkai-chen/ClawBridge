package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"clawbridge/internal/faketool"
	"clawbridge/internal/linearconn"
	"clawbridge/internal/linearprovider"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// meCaller answers get_user "me" with user.
type meCaller struct {
	tool *mcp.Tool
	user map[string]any
}

func (c meCaller) ListTools(context.Context, *mcp.ListToolsParams) (*mcp.ListToolsResult, error) {
	return &mcp.ListToolsResult{Tools: []*mcp.Tool{c.tool}}, nil
}
func (c meCaller) CallTool(_ context.Context, p *mcp.CallToolParams) (*mcp.CallToolResult, error) {
	raw, _ := json.Marshal(c.user)
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: string(raw)}}}, nil
}

// The engineer's name for agent markers is the member's own, set on the
// hosted server: by hand, cleared, captured from Linear, or captured at
// connect without replacing a name already set.
func TestLinearDisplayNameRecordsTheMembersName(t *testing.T) {
	raw, err := os.ReadFile("../linearprovider/testdata/get-user-20261023.json")
	if err != nil {
		t.Fatal(err)
	}
	var getUser mcp.Tool
	if err = json.Unmarshal(raw, &getUser); err != nil {
		t.Fatal(err)
	}
	dir := isolated(t)
	faketool.Install(t, dir, "gcloud", "print:synthetic-cli-token")
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	principal := "service@example.test"
	var mu sync.Mutex
	var sets []map[string]any
	host := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Operation, Workspace string
			Args                 map[string]any
		}
		if e := json.NewDecoder(r.Body).Decode(&req); e != nil {
			t.Error(e)
		}
		data := map[string]any{}
		switch req.Operation {
		case "resolve_project":
			data = map[string]any{"workspace_id": "ws-cloud", "project_id": "prj-cloud"}
		case "member_display_name_set":
			if req.Workspace != "ws-cloud" {
				t.Error("workspace", req.Workspace)
			}
			mu.Lock()
			sets = append(sets, req.Args)
			mu.Unlock()
			name, _ := req.Args["display_name"].(string)
			data = map[string]any{"display_name": name, "engineer": name, "kept": false}
		default:
			t.Errorf("unexpected cloud operation %s", req.Operation)
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"source": "server", "data": data})
	}))
	defer host.Close()
	for k, v := range map[string]string{"MODE": "cloud", "SERVER": host.URL, "AUDIENCE": "test-audience", "ACCOUNT": "human@example.test", "IMPERSONATE_SERVICE_ACCOUNT": principal, "PRINCIPAL": principal, "CACHE": filepath.Join(dir, "mirror.db"), "WORKSPACE": "w", "PROJECT": "p"} {
		t.Setenv("CLAWBRIDGE_"+k, v)
	}
	secrets := &statusSecrets{values: map[string]string{}}
	previousManager, previousOpen := newLinearManager, openLinearProvider
	t.Cleanup(func() { newLinearManager, openLinearProvider = previousManager, previousOpen })
	newLinearManager = func(directory string) *linearconn.Manager {
		return linearconn.New(linearconn.Config{Directory: directory, Store: secrets, Verify: func(context.Context, *http.Client, linearconn.Access) (linearconn.Identity, error) {
			return linearconn.Identity{}, nil
		}})
	}
	user := map[string]any{"id": "d1000000-0000-4000-8000-000000000001", "name": "Ada Lovelace", "displayName": "ada"}
	openLinearProvider = func(ctx context.Context, _ *http.Client, _ bool) (*linearprovider.Provider, error) {
		return linearprovider.NewWithCaller(ctx, meCaller{&getUser, user})
	}
	directory, err := linearDirectory()
	if err != nil {
		t.Fatal(err)
	}
	manager := newLinearManager(directory)
	profile, err := manager.Import(context.Background(), "name test", linearconn.Read, strings.NewReader("lin_api_syntheticnametoken00000000000000000000"))
	if err != nil {
		t.Fatal(err)
	}
	run := func(args ...string) error {
		var out, diag bytes.Buffer
		return Run(context.Background(), append([]string{"linear", "display-name"}, args...), &out, &diag)
	}
	last := func() map[string]any {
		mu.Lock()
		defer mu.Unlock()
		return sets[len(sets)-1]
	}
	for _, args := range [][]string{{"set", "Grace Hopper"}, {"clear"}, {"capture", "--profile", profile.ID}} {
		if e := run(args...); e != nil {
			t.Fatal(args, e)
		}
	}
	mu.Lock()
	got := append([]map[string]any{}, sets...)
	mu.Unlock()
	if len(got) != 3 || got[0]["display_name"] != "Grace Hopper" || got[1]["display_name"] != "" || got[2]["display_name"] != "Ada Lovelace" || got[2]["if_unset"] != nil {
		t.Fatal("recorded", got)
	}
	for _, args := range [][]string{nil, {"rename", "x"}, {"set"}, {"set", "  "}, {"set", "a", "b"}, {"clear", "x"}, {"capture"}} {
		if e := run(args...); e == nil {
			t.Fatal("accepted", args)
		}
	}
	// At connect the name fills only an empty one, and the note says so.
	note := captureEngineerAtConnect(context.Background(), Config{}, manager, profile.ID)
	if a := last(); a["display_name"] != "Ada Lovelace" || a["if_unset"] != float64(1) || !strings.Contains(note, `"Ada Lovelace"`) {
		t.Fatal("connect capture", a, note)
	}
	// The handle stands in for a missing full name.
	user["name"] = nil
	if e := run("capture", "--profile", profile.ID); e != nil || last()["display_name"] != "ada" {
		t.Fatal("handle fallback", e, last())
	}
	// A directory without a hosted project captures nothing.
	t.Setenv("CLAWBRIDGE_MODE", "local")
	before := len(sets)
	if note := captureEngineerAtConnect(context.Background(), Config{}, manager, profile.ID); note != "" || len(sets) != before {
		t.Fatal("captured without a hosted project", note)
	}
}
