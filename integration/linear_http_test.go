package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"clawbridge/internal/linearboard"
	"clawbridge/internal/linearconn"
	"clawbridge/internal/linearprovider"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"golang.org/x/oauth2"
)

type l1HTTPConnections struct {
	client *http.Client
	access linearconn.Access
}

func (c l1HTTPConnections) Status(context.Context, string) (linearconn.Profile, error) {
	return linearconn.Profile{ID: l1ProfileID, Access: c.access}, nil
}
func (c l1HTTPConnections) HTTPClient(context.Context, string) (*http.Client, error) {
	return c.client, nil
}

// This is the actual SDK Streamable-HTTP client and server, not ToolCaller.
// Only the injected transport can rewrite the fixed official origin, and its
// dialer refuses every address except this test's owned loopback listener.
func TestL1NativeStreamableHTTPAcceptedWriteResponseLossNeverReplays(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	f := newFixture(t)
	f.setup("w", "p")
	mapping := l1MappingArgs(0)
	mapping["linear_project_id"] = l1Project
	f.call("taskboard_set", "w", "p", "", mapping)
	scope := f.request("", "w", "p", "worker", actor("worker", nil))
	ledgerDir := filepath.Join(t.TempDir(), "ledger")
	var accepted atomic.Int64
	var pendingBeforeAcceptance atomic.Bool
	server := mcp.NewServer(&mcp.Implementation{Name: "synthetic-linear-http-peer", Version: "1"}, nil)
	for _, tool := range newL1ProviderPeer().tools {
		if tool.Name != "get_team" && tool.Name != "get_project" && tool.Name != "get_issue" && tool.Name != "create_issue" {
			continue
		}
		name := tool.Name
		server.AddTool(tool, func(_ context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			args := map[string]any{}
			if err := json.Unmarshal(req.Params.Arguments, &args); err != nil {
				return nil, err
			}
			if name == "create_issue" {
				if args["teamId"] != l1TeamA || args["projectId"] != l1Project || args["title"] != "synthetic isolated task" {
					return nil, errors.New("synthetic scoped write argument mismatch")
				}
				_ = filepath.WalkDir(ledgerDir, func(path string, entry os.DirEntry, err error) error {
					if err == nil && !entry.IsDir() && strings.HasSuffix(path, ".json") {
						raw, _ := os.ReadFile(path)
						var record struct{ State string }
						if json.Unmarshal(raw, &record) == nil && record.State == "pending" {
							pendingBeforeAcceptance.Store(true)
						}
					}
					return err
				})
				accepted.Add(1)
				return &mcp.CallToolResult{StructuredContent: map[string]any{"id": l1IssueID, "teamId": l1TeamA, "projectId": l1Project, "title": args["title"]}}, nil
			}
			result, err := l1PeerResult(name, args)
			return &mcp.CallToolResult{StructuredContent: result}, err
		})
	}
	peer := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, &mcp.StreamableHTTPOptions{JSONResponse: true}))
	defer peer.Close()
	target, err := url.Parse(peer.URL)
	if err != nil {
		t.Fatal(err)
	}
	base := &http.Transport{Proxy: nil, DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
		if address != target.Host {
			return nil, errors.New("fixture refuses non-owned network address")
		}
		return (&net.Dialer{}).DialContext(ctx, network, address)
	}}
	defer base.CloseIdleConnections()
	type observation struct{ Path, Method, RPC, Tool string }
	var mu sync.Mutex
	var trace []observation
	var lost atomic.Int64
	const syntheticToken = "synthetic-http-fixture-no-real-credential"
	rewrite := transportFunc(func(r *http.Request) (*http.Response, error) {
		// Assert the ORIGINAL URL and auth before changing either destination or
		// Host. Do not expose even this fake bearer value in failure diagnostics.
		if r.URL.Scheme != "https" || r.URL.Host != "mcp.linear.app" || (r.URL.Path != "/mcp" && r.URL.Path != "/mcp/readonly") || r.URL.RawQuery != "" || r.Header.Get("Authorization") != "Bearer "+syntheticToken {
			return nil, errors.New("original fixed-origin/auth invariant failed")
		}
		var raw []byte
		if r.Body != nil {
			var readErr error
			raw, readErr = io.ReadAll(r.Body)
			if readErr != nil {
				return nil, readErr
			}
			r.Body = io.NopCloser(bytes.NewReader(raw))
		}
		var rpc struct {
			Method string
			Params struct{ Name string }
		}
		_ = json.Unmarshal(raw, &rpc)
		mu.Lock()
		trace = append(trace, observation{r.URL.Path, r.Method, rpc.Method, rpc.Params.Name})
		mu.Unlock()
		local := r.Clone(r.Context())
		local.URL.Scheme, local.URL.Host, local.Host = target.Scheme, target.Host, target.Host
		local.Body = io.NopCloser(bytes.NewReader(raw))
		response, e := base.RoundTrip(local)
		if e == nil && rpc.Method == "tools/call" && rpc.Params.Name == "create_issue" {
			// The real fake-peer handler has accepted/completed the write, but
			// the native SDK must receive no response and must not replay it.
			_, _ = io.Copy(io.Discard, response.Body)
			_ = response.Body.Close()
			lost.Add(1)
			return nil, io.ErrUnexpectedEOF
		}
		return response, e
	})
	client := &http.Client{Transport: &oauth2.Transport{Source: oauth2.StaticTokenSource(&oauth2.Token{AccessToken: syntheticToken}), Base: rewrite}, Timeout: 3 * time.Second}
	var opens atomic.Int64
	cfg := linearboard.Config{Authority: "synthetic-owned-http-authority", Executor: f.store, Connections: l1HTTPConnections{client, linearconn.Read}, ProfileID: l1ProfileID, LedgerDirectory: ledgerDir, Open: func(ctx context.Context, h *http.Client, readonly bool) (linearboard.Provider, error) {
		opens.Add(1)
		return linearprovider.New(ctx, h, readonly)
	}}
	issue, err := linearboard.New(cfg).Issue(ctx, scope, l1IssueID)
	if err != nil || issue.ID != l1IssueID || issue.TeamID != l1TeamA || issue.ProjectID != l1Project || issue.Description != "scope-protected synthetic body" {
		t.Fatalf("native HTTP scoped read failed: %v", err)
	}
	cfg.Connections = l1HTTPConnections{client, linearconn.Write}
	input := l1Create("native-http-lost-response")
	if receipt, err := linearboard.New(cfg).Write(ctx, scope, input); err == nil || receipt != nil || !strings.Contains(err.Error(), "uncertain") {
		t.Fatalf("lost response fabricated success or certainty: receipt=%v err=%v", receipt, err)
	}
	if accepted.Load() != 1 || lost.Load() != 1 || !pendingBeforeAcceptance.Load() {
		t.Fatalf("SDK dispatched/replayed write or ledger was not pending before acceptance: accepted=%d lost=%d pending=%v", accepted.Load(), lost.Load(), pendingBeforeAcceptance.Load())
	}
	before := l1LedgerBytes(t, ledgerDir)
	if len(before) != 1 {
		t.Fatalf("expected one durable pending record, got %d", len(before))
	}
	for _, raw := range before {
		var record struct{ State string }
		if json.Unmarshal([]byte(raw), &record) != nil || record.State != "pending" {
			t.Fatal("unknown write outcome did not retain pending ledger")
		}
	}
	mu.Lock()
	beforeRequests := len(trace)
	mu.Unlock()
	beforeOpens := opens.Load()
	if _, err := linearboard.New(cfg).Write(ctx, scope, input); err == nil || !strings.Contains(err.Error(), "outcome uncertain") {
		t.Fatalf("restart retry did not refuse pending operation: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(trace) != beforeRequests || opens.Load() != beforeOpens || accepted.Load() != 1 || !reflect.DeepEqual(before, l1LedgerBytes(t, ledgerDir)) {
		t.Fatal("pending retry reopened provider, performed HTTP, replayed write or changed ledger")
	}
	for _, path := range []string{"/mcp/readonly", "/mcp"} {
		methods := map[string]bool{}
		for _, seen := range trace {
			if seen.Path == path && seen.Method == http.MethodPost {
				methods[seen.RPC] = true
			}
		}
		for _, method := range []string{"initialize", "tools/list", "tools/call"} {
			if !methods[method] {
				t.Errorf("native SDK omitted %s at original %s", method, path)
			}
		}
	}
}
