package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"clawbridge/internal/bus"
	"github.com/pelletier/go-toml/v2"
)

func TestPhase4CLIJoinOnlinePublicationAndDenial(t *testing.T) {
	p := newProcessFixture(t)
	for _, outcome := range []string{"allow", "forbidden", "offline", "wrong-principal"} {
		t.Run(outcome, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "client.toml")
			state := filepath.Join(dir, "state")
			var mu sync.Mutex
			var operations []string
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/v1/execute" || r.Header.Get("Authorization") != "Bearer synthetic-join-token" {
					t.Error("unexpected request/auth (values suppressed)")
					w.WriteHeader(400)
					return
				}
				var req bus.Request
				if json.NewDecoder(r.Body).Decode(&req) != nil {
					w.WriteHeader(400)
					return
				}
				mu.Lock()
				operations = append(operations, req.Operation)
				mu.Unlock()
				if req.Session != "" || req.Args["instance"] != nil {
					t.Error("join adopted old actor")
				}
				w.Header().Set("Content-Type", "application/json")
				if req.Operation == "resolve_project" {
					_ = json.NewEncoder(w).Encode(bus.Result{Source: "server", Data: map[string]any{"workspace_id": "ws_joined", "project_id": "prj_joined"}})
					return
				}
				if req.Operation != "whoami" {
					t.Errorf("join requested application mutation %s", req.Operation)
					w.WriteHeader(400)
					return
				}
				if req.Workspace != "ws_joined" || req.Project != "prj_joined" {
					t.Error("whoami not pinned to resolved scope")
				}
				if outcome == "forbidden" {
					w.WriteHeader(403)
					_, _ = w.Write([]byte(`{"error":{"code":"forbidden","message":"synthetic-private-error-body-do-not-echo"}}`))
					return
				}
				principal := "agent@project.iam.gserviceaccount.com"
				if outcome == "wrong-principal" {
					principal = "other@example.test"
				}
				source := "server"
				if outcome == "offline" {
					source = "offline-mirror"
				}
				_ = json.NewEncoder(w).Encode(bus.Result{Source: source, Data: map[string]any{"online": outcome != "offline", "verified": true, "storage_mode": "cloud", "identity_provenance": "google-id-token", "principal": principal, "workspace": map[string]any{"id": "ws_joined"}, "project": map[string]any{"id": "prj_joined"}}})
			}))
			defer server.Close()
			original := []byte("mode='cloud'\nserver=" + quoteTOML(server.URL) + "\naudience='test-audience'\ncredential_source='gcloud'\naccount='human@example.test'\nimpersonate_service_account='agent@project.iam.gserviceaccount.com'\nprincipal='agent@project.iam.gserviceaccount.com'\nworkspace='old-workspace'\nproject='old-project'\nsession='old-actor'\ninstance='old-instance'\nunrelated='preserve'\n")
			if err := os.WriteFile(path, original, 0600); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, "gcloud"), []byte("#!/bin/sh\nprintf '%s' 'synthetic-join-token'\n"), 0700); err != nil {
				t.Fatal(err)
			}
			run := func() (map[string]any, error) {
				t.Helper()
				ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				defer cancel()
				cmd := exec.CommandContext(ctx, p.binary, "setup", "join", "--client-config", path, "--server", server.URL, "--workspace", "selected", "--project", "selected", "--cache", "relative-cache.db")
				cmd.Dir = dir
				cmd.Env = []string{"HOME=" + dir, "PATH=" + dir, "CLAWBRIDGE_HOME=" + state}
				var stdout, stderr bytes.Buffer
				cmd.Stdout = &stdout
				cmd.Stderr = &stderr
				err := cmd.Run()
				for _, text := range []string{stdout.String(), stderr.String()} {
					for _, secret := range []string{"synthetic-join-token", "synthetic-private-error-body-do-not-echo"} {
						if strings.Contains(text, secret) {
							t.Fatal("CLI echoed sensitive fixture data (values suppressed)")
						}
					}
				}
				if err != nil {
					if stdout.Len() != 0 {
						t.Error("failed join emitted success output")
					}
					return nil, err
				}
				var result map[string]any
				if json.Unmarshal(stdout.Bytes(), &result) != nil {
					t.Fatal("join did not emit a single JSON result")
				}
				return result, nil
			}
			result, err := run()
			after, readErr := os.ReadFile(path)
			if readErr != nil {
				t.Fatal(readErr)
			}
			if outcome != "allow" {
				if err == nil {
					t.Fatal("unauthorized/offline join succeeded")
				}
				if !bytes.Equal(original, after) {
					t.Error("failed join changed trusted bytes")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			data := object(t, result["data"])
			if result["source"] != "server" || data["joined"] != true || data["credential_source"] != "gcloud" || data["workspace"] != "ws_joined" || data["project"] != "prj_joined" || data["changed"] != true || len(data) != 7 {
				t.Fatalf("join output mismatch: %+v", result)
			}
			var config map[string]any
			if toml.Unmarshal(after, &config) != nil {
				t.Fatal("joined config invalid")
			}
			if config["session"] != nil || config["instance"] != nil || config["unrelated"] != "preserve" || config["cache"] != filepath.Join(state, "relative-cache.db") {
				t.Fatal("actor clearing/unrelated preservation/cache anchoring mismatch")
			}
			result, err = run()
			if err != nil || object(t, result["data"])["changed"] != false {
				t.Fatal("repeat join not idempotent")
			}
			again, _ := os.ReadFile(path)
			if !bytes.Equal(after, again) {
				t.Error("repeat join rewrote trusted bytes")
			}
			mu.Lock()
			defer mu.Unlock()
			for _, op := range operations {
				if op != "resolve_project" && op != "whoami" {
					t.Errorf("unexpected op %s", op)
				}
			}
		})
	}
}
