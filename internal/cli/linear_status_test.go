package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"

	"clawbridge/internal/linearconn"
	"clawbridge/internal/linearprovider"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type statusSecrets struct {
	mu     sync.Mutex
	values map[string]string
}

func (s *statusSecrets) Get(service, key string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, ok := s.values[service+key]
	if !ok {
		return "", fmt.Errorf("missing secret")
	}
	return v, nil
}
func (s *statusSecrets) Set(service, key, value string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.values[service+key] = value
	return nil
}
func (s *statusSecrets) Delete(service, key string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.values, service+key)
	return nil
}

type emptyToolCaller struct{}

func (emptyToolCaller) ListTools(context.Context, *mcp.ListToolsParams) (*mcp.ListToolsResult, error) {
	return &mcp.ListToolsResult{}, nil
}
func (emptyToolCaller) CallTool(context.Context, *mcp.CallToolParams) (*mcp.CallToolResult, error) {
	return nil, fmt.Errorf("unexpected Linear call")
}

func TestLinearStatusReportsCapabilitiesBestEffort(t *testing.T) {
	isolated(t)
	secrets := &statusSecrets{values: map[string]string{}}
	previousManager, previousOpen := newLinearManager, openLinearProvider
	t.Cleanup(func() { newLinearManager, openLinearProvider = previousManager, previousOpen })
	newLinearManager = func(directory string) *linearconn.Manager {
		return linearconn.New(linearconn.Config{Directory: directory, Store: secrets, Verify: func(context.Context, *http.Client, linearconn.Access) (linearconn.Identity, error) {
			return linearconn.Identity{}, nil
		}})
	}
	directory, err := linearDirectory()
	if err != nil {
		t.Fatal(err)
	}
	profile, err := newLinearManager(directory).Import(context.Background(), "status test", linearconn.Read, strings.NewReader("lin_api_syntheticstatustoken0000000000000000"))
	if err != nil {
		t.Fatal(err)
	}
	status := func() map[string]any {
		t.Helper()
		var out, diag bytes.Buffer
		if e := Run(context.Background(), []string{"linear", "status", "--profile", profile.ID}, &out, &diag); e != nil {
			t.Fatal("status failed because capabilities were unavailable", e)
		}
		if strings.Contains(out.String(), "secret transport") || strings.Contains(out.String(), "syntheticstatustoken") {
			t.Fatal("status leaked transport or credential text", out.String())
		}
		var value map[string]any
		if e := json.Unmarshal(out.Bytes(), &value); e != nil {
			t.Fatal(e, out.String())
		}
		data, _ := value["data"].(map[string]any)
		if data == nil {
			data = value
		}
		return data
	}
	openLinearProvider = func(context.Context, *http.Client, bool) (*linearprovider.Provider, error) {
		return nil, fmt.Errorf("dial secret transport detail: %w", &linearprovider.Error{Kind: "unavailable"})
	}
	data := status()
	if data["id"] != profile.ID || data["capabilities_error"] != "capabilities unavailable: unavailable" || data["capabilities"] != nil {
		t.Fatal("unreachable provider not reported best effort", data)
	}
	openLinearProvider = func(ctx context.Context, _ *http.Client, _ bool) (*linearprovider.Provider, error) {
		return linearprovider.NewWithCaller(ctx, emptyToolCaller{})
	}
	data = status()
	caps, _ := data["capabilities"].(map[string]any)
	reasons, _ := caps["reasons"].(map[string]any)
	if data["capabilities_error"] != nil || caps == nil || caps["label_eligibility_by_filter"] != false || reasons["label_eligibility_by_filter"] != "label listing is unavailable" {
		t.Fatal("capability state missing from status", data)
	}
}
