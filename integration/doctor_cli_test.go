package integration

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"clawbridge/internal/bus"
	"clawbridge/internal/doctor"
)

func TestDoctorCLIValidatesTrustedRouteReadOnly(t *testing.T) {
	var executes atomic.Int32
	authority := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		executes.Add(1)
		if r.URL.Path != "/v1/execute" {
			http.NotFound(w, r)
			return
		}
		if !strings.HasPrefix(r.Header.Get("Authorization"), "ClawBridge-Device ") {
			t.Errorf("doctor's request is not device-signed")
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		var request bus.Request
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Errorf("doctor sent invalid request JSON: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		data := any(map[string]any{"projects": []any{map[string]any{"id": "project-id", "slug": "project"}}})
		if request.Operation == "resolve_project" {
			data = map[string]any{"workspace_id": "workspace-id", "project_id": "project-id"}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(bus.Result{Source: "server", Data: data})
	}))
	defer authority.Close()

	p := newProcessFixture(t)
	gitAt(t, p.dir, "init", "--quiet")
	home := filepath.Join(p.dir, "doctor-state")
	if err := os.MkdirAll(home, 0700); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(home, "config.toml")
	const unrelatedSentinel = "synthetic-unrelated-config-secret"
	configText := "mode = 'cloud'\n" +
		"server = " + quoteTOML(authority.URL) + "\n" +
		fileDeviceRoute(t, home, authority.URL) +
		"principal = 'agent@example.test'\n" +
		"cache = " + quoteTOML(filepath.Join(home, "missing-mirror.db")) + "\n" +
		"workspace = 'workspace'\nproject = 'project'\n" +
		"unrelated_secret = " + quoteTOML(unrelatedSentinel) + "\n"
	if err := os.WriteFile(configPath, []byte(configText), 0600); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}

	// An agent is set up (its configuration directory exists), so with no
	// entry selected mcp.entries warns rather than notes: not the machine's.
	agentHome := filepath.Join(p.dir, "agent-home")
	if err := os.MkdirAll(agentHome, 0700); err != nil {
		t.Fatal(err)
	}
	env := append(append([]string{}, p.env...), "CLAUDE_CONFIG_DIR="+agentHome, "CODEX_HOME="+agentHome, "CLAWBRIDGE_HOME="+home)
	cmd := exec.Command(p.binary, "doctor")
	cmd.Dir = p.dir
	cmd.Env = env
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("doctor command failed: %v stdout=%s stderr=%s", err, stdout.String(), stderr.String())
	}
	var report doctor.Report
	if err := json.Unmarshal(stdout.Bytes(), &report); err != nil {
		t.Fatalf("doctor stdout is not one JSON report: %v: %s", err, stdout.String())
	}
	for _, tc := range []struct {
		id   string
		want string
	}{
		{"config.trusted", doctor.Pass},
		{"authority.health", doctor.Pass},
		{"auth.device", doctor.Pass},
		{"project.resolution", doctor.Pass},
		{"mirror.freshness", doctor.Warn},
		{"mcp.entries", doctor.Warn},
	} {
		var found bool
		for _, check := range report.Checks {
			if check.ID == tc.id {
				found = true
				if check.Status != tc.want {
					t.Errorf("doctor %s status=%s want=%s message=%s", check.ID, check.Status, tc.want, check.Message)
				}
				break
			}
		}
		if !found {
			t.Errorf("doctor omitted %s", tc.id)
		}
	}
	if report.Status != doctor.Warn {
		t.Errorf("doctor aggregate status=%s want warn", report.Status)
	}
	if executes.Load() != 2 {
		t.Errorf("doctor made %d authority calls, want list and resolve only", executes.Load())
	}
	after, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("doctor modified trusted configuration")
	}
	combined := stdout.String() + stderr.String()
	for _, sensitive := range []string{unrelatedSentinel} {
		if strings.Contains(combined, sensitive) {
			t.Fatalf("doctor output echoed sensitive sentinel %q: %s", sensitive, combined)
		}
	}
}

func quoteTOML(value string) string {
	encoded, _ := json.Marshal(value)
	return string(encoded)
}
