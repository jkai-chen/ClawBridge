package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// linear write-status asks the server for your writes, one by its
// operation_id, or with --project-wide the project's whole queue.
func TestLinearWriteStatusAsksTheServer(t *testing.T) {
	dir := isolated(t)
	principal := "service@example.test"
	var mu sync.Mutex
	var asked []map[string]any
	host := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Operation string
			Args      map[string]any
		}
		if e := json.NewDecoder(r.Body).Decode(&req); e != nil {
			t.Error(e)
		}
		data := map[string]any{}
		switch req.Operation {
		case "resolve_project":
			data = map[string]any{"workspace_id": "ws-cloud", "project_id": "prj-cloud"}
		case "linear_write_status":
			mu.Lock()
			asked = append(asked, req.Args)
			mu.Unlock()
			data = map[string]any{"writes": []any{map[string]any{"operation_id": "op-1", "state": "queued"}}}
		default:
			t.Errorf("unexpected cloud operation %s", req.Operation)
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"source": "server", "data": data})
	}))
	defer host.Close()
	for k, v := range map[string]string{"MODE": "cloud", "SERVER": host.URL, "PRINCIPAL": principal, "CACHE": filepath.Join(dir, "mirror.db"), "WORKSPACE": "w", "PROJECT": "p"} {
		t.Setenv("CLAWBRIDGE_"+k, v)
	}
	useDeviceRoute(t, host.URL)
	for _, args := range [][]string{{}, {"op-1"}, {"--project-wide"}} {
		var out, diag bytes.Buffer
		if err := Run(context.Background(), append([]string{"linear", "write-status"}, args...), &out, &diag); err != nil || !strings.Contains(out.String(), "op-1") {
			t.Fatal(args, err, out.String(), diag.String())
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if len(asked) != 3 || asked[0]["scope"] != nil || asked[0]["operation_id"] != nil || asked[1]["operation_id"] != "op-1" || asked[1]["scope"] != nil || asked[2]["scope"] != "project" {
		t.Fatal("asked", asked)
	}
}
