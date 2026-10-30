package integration

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"clawbridge/internal/bus"
	"clawbridge/internal/mirror"
	"clawbridge/internal/remote"
)

// Google sign-in's OAuth flows were removed in v1.0; what stays here is the
// client's credential boundary and its refusal of the old Google routes.

func TestPhase3CredentialFailureStopsBeforeTransportAndMirror(t *testing.T) {
	cachePath := filepath.Join(t.TempDir(), "mirror.db")
	binding := mirror.Binding{
		Server: "http://127.0.0.1:1", Audience: "review-audience",
		Principal: "reviewer@example.test", Workspace: "workspace-id",
		Project: "project-id", Session: "reviewer", Instance: "reviewer-instance",
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	if err := mirror.Save(context.Background(), cachePath, mirror.Snapshot{
		Binding: binding, UpdatedAt: now,
		Data: map[string]any{
			"workspace": map[string]any{"id": "workspace-id"},
			"project":   map[string]any{"id": "project-id"},
			"session":   map[string]any{"id": "reviewer", "name": "reviewer", "instance": "reviewer-instance"},
			"principal": binding.Principal, "verified": true,
			"identity_provenance": "google-id-token", "storage_mode": "cloud",
			"snapshot_at":   now,
			"sessions":      []any{map[string]any{"id": "reviewer", "name": "reviewer", "instance": "reviewer-instance", "status": "active"}},
			"messages":      []any{map[string]any{"id": "must-not-be-returned", "sequence": 1, "body": "cached-private-body", "expires_at": time.Now().Add(time.Hour).UTC().Format(time.RFC3339Nano)}},
			"sent_messages": []any{},
			"events":        []any{},
		},
	}); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(cachePath)
	if err != nil {
		t.Fatal(err)
	}
	config := remote.Config{
		URL: "http://127.0.0.1:1", Audience: "review-audience",
		Principal: binding.Principal, CachePath: cachePath,
	}
	request := bus.Request{Operation: "inbox", Workspace: "workspace-id", Project: "project-id", Session: "reviewer", Args: map[string]any{"instance": "reviewer-instance"}}
	for name, source := range map[string]remote.TokenSource{
		"credential-read": remote.TokenSourceFunc(func(context.Context) (string, error) {
			return "", errors.New("synthetic-keychain-failure-secret")
		}),
		"refresh-or-mint": remote.TokenSourceFunc(func(context.Context) (string, error) {
			return "", errors.New("synthetic-refresh-failure-secret")
		}),
	} {
		t.Run(name, func(t *testing.T) {
			client, err := remote.NewWithTokenSource(config, source)
			if err != nil {
				t.Fatal(err)
			}
			result, err := client.Execute(context.Background(), request)
			if err == nil {
				t.Fatalf("credential failure returned result source %q with cached body", result.Source)
			}
			if strings.Contains(err.Error(), "secret") {
				t.Fatalf("credential failure echoed injected secret: %v", err)
			}
		})
	}
	after, err := os.ReadFile(cachePath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("credential failure changed mirror bytes")
	}
}

func TestPhase3CredentialSourcesAreExclusiveAndRepositoryCannotInject(t *testing.T) {
	p := newProcessFixture(t)
	gitAt(t, p.dir, "init", "--quiet")
	home := filepath.Join(p.dir, "phase3-state")
	if err := os.MkdirAll(home, 0700); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(home, "config.toml")
	env := append(append([]string{}, p.env...), "CLAWBRIDGE_HOME="+home)
	runConfig := func(config string, args ...string) ([]byte, []byte, error) {
		t.Helper()
		if err := os.WriteFile(configPath, []byte(config), 0600); err != nil {
			t.Fatal(err)
		}
		cmd := exec.Command(p.binary, args...)
		cmd.Dir = p.dir
		cmd.Env = env
		var stdout, stderr bytes.Buffer
		cmd.Stdout = &stdout
		cmd.Stderr = &stderr
		err := cmd.Run()
		return stdout.Bytes(), stderr.Bytes(), err
	}

	// v1.0 removed Google sign-in: a profile that still names one of its
	// sources is refused, saying how to join, and never echoes its routing.
	for _, tc := range []struct {
		name   string
		config string
		want   string
	}{
		{
			name:   "oauth",
			config: "mode='cloud'\nserver='https://clawbridge.example.test'\naudience='desktop-client'\nprincipal='human@example.test'\ncredential_source='oauth'\n",
			want:   "removed in v1.0",
		},
		{
			name:   "gcloud",
			config: "mode='cloud'\nserver='https://clawbridge.example.test'\naudience='audience'\nprincipal='human@example.test'\ncredential_source='gcloud'\naccount='human@example.test'\nimpersonate_service_account='agent@example.test'\n",
			want:   "removed in v1.0",
		},
		{
			name:   "adc",
			config: "mode='cloud'\nserver='https://clawbridge.example.test'\naudience='audience'\nprincipal='human@example.test'\ncredential_source='adc'\naccount='human@example.test'\nimpersonate_service_account='agent@example.test'\n",
			want:   "removed in v1.0",
		},
		{
			name:   "none-was-gcloud",
			config: "mode='cloud'\nserver='https://clawbridge.example.test'\nprincipal='human@example.test'\n",
			want:   "removed in v1.0",
		},
		{
			name:   "unknown-backend-is-not-inferred",
			config: "mode='cloud'\nserver='https://clawbridge.example.test'\naudience='audience'\ncredential_source='metadata'\n",
			want:   "removed in v1.0",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stdout, stderr, err := runConfig(tc.config, "whoami")
			if err == nil {
				t.Fatalf("exclusive backend configuration succeeded: stdout=%s", stdout)
			}
			combined := string(stdout) + string(stderr)
			if !strings.Contains(combined, tc.want) {
				t.Fatalf("failure did not say Google sign-in was removed: %s", combined)
			}
			for _, sentinel := range []string{"desktop-client", "agent@example.test", "human@example.test"} {
				if strings.Contains(combined, sentinel) {
					t.Fatalf("configuration failure echoed credential-routing sentinel %q: %s", sentinel, combined)
				}
			}
		})
	}

	// A repository file may select scope, but cannot inject a trusted cloud route
	// or switch authority mode. A malicious cloud mode must leave the trusted
	// local route effective.
	localConfig := "mode='local'\nworkspace='review-workspace'\ndb='" + p.db + "'\n"
	if err := os.WriteFile(configPath, []byte(localConfig), 0600); err != nil {
		t.Fatal(err)
	}
	repo := "mode='cloud'\nserver='https://attacker.example.test'\naudience='attacker-audience'\ncredential_source='oauth'\noauth_client_id='attacker-client'\nworkspace='attacker-workspace'\nproject='attacker-project'\n"
	if err := os.WriteFile(filepath.Join(p.dir, ".clawbridge.toml"), []byte(repo), 0600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(p.binary, "setup", "--local", "--workspace", "review-workspace")
	cmd.Dir = p.dir
	cmd.Env = env
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("repository cloud mode overrode trusted local authority: %v stdout=%s stderr=%s", err, stdout.String(), stderr.String())
	}
	combined := stdout.String() + stderr.String()
	for _, injected := range []string{"attacker.example.test", "attacker-audience", "attacker-client", "attacker-workspace", "attacker-project"} {
		if strings.Contains(combined, injected) {
			t.Fatalf("repository route was accepted or echoed: %q in %s", injected, combined)
		}
	}
	after, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(after), "mode='local'") && !strings.Contains(string(after), "mode = 'local'") && !strings.Contains(string(after), "mode = \"local\"") {
		t.Fatalf("trusted local mode was not preserved: %s", after)
	}
	for _, injected := range []string{"attacker.example.test", "attacker-audience", "attacker-client", "credential_source"} {
		if strings.Contains(string(after), injected) {
			t.Fatalf("repository injected %q into trusted configuration: %s", injected, after)
		}
	}

	// With no repository mode override, untrusted scope can overlay trusted local
	// routing and must not change its mode, DB, or credential-related bytes.
	repo = "workspace='review-workspace'\nproject='repo-project'\nserver='https://attacker.example.test'\naudience='attacker-audience'\ncredential_source='oauth'\noauth_client_id='attacker-client'\n"
	if err := os.WriteFile(filepath.Join(p.dir, ".clawbridge.toml"), []byte(repo), 0600); err != nil {
		t.Fatal(err)
	}
	cmd = exec.Command(p.binary, "setup", "--local", "--workspace", "review-workspace")
	cmd.Dir = p.dir
	cmd.Env = env
	stdout.Reset()
	stderr.Reset()
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("safe repository scope could not overlay trusted local routing: %v stdout=%s stderr=%s", err, stdout.String(), stderr.String())
	}
	after, err = os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(after), "mode='local'") && !strings.Contains(string(after), "mode = 'local'") && !strings.Contains(string(after), "mode = \"local\"") {
		t.Fatalf("trusted local mode was not preserved after safe repository overlay: %s", after)
	}
	for _, injected := range []string{"attacker.example.test", "attacker-audience", "attacker-client", "credential_source"} {
		if strings.Contains(string(after), injected) {
			t.Fatalf("repository injected %q into trusted configuration: %s", injected, after)
		}
	}

	// The reverse direction is equally authoritative: a repository mode=local
	// cannot downgrade a trusted cloud route.
	cloudConfig := "mode='cloud'\nserver='https://trusted.example.test'\nprincipal='reviewer@example.test'\ncache='" + filepath.Join(home, "trusted-cache.db") + "'\nworkspace='review-workspace'\ncredential_source='device'\ndevice_id='dev_abcdefghjkmnpqrstvwxyz0123'\nkey_store='file'\n"
	if err := os.WriteFile(configPath, []byte(cloudConfig), 0600); err != nil {
		t.Fatal(err)
	}
	repo = "mode='local'\nworkspace='review-workspace'\nproject='repo-project'\ndb='" + filepath.Join(p.dir, "attacker.db") + "'\n"
	if err := os.WriteFile(filepath.Join(p.dir, ".clawbridge.toml"), []byte(repo), 0600); err != nil {
		t.Fatal(err)
	}
	// The device key is read before any HTTP request, so a missing test key
	// safely fails closed. The diagnostic must name the device key, proving
	// the trusted cloud route survived repository mode=local.
	stdout.Reset()
	stderr.Reset()
	cmd = exec.Command(p.binary, "whoami")
	cmd.Dir = p.dir
	cmd.Env = env
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err = cmd.Run()
	if err == nil {
		t.Fatalf("cloud route unexpectedly succeeded without a test device key: stdout=%s", stdout.String())
	}
	combined = stdout.String() + stderr.String()
	if strings.Contains(combined, "local-authority") || strings.Contains(strings.ToLower(combined), "sqlite") {
		t.Fatalf("repository downgraded trusted cloud routing to local authority: %s", combined)
	}
	if !strings.Contains(strings.ToLower(combined), "device") {
		t.Fatalf("trusted cloud mode did not remain effective: %s", combined)
	}
	for _, injected := range []string{"attacker.db"} {
		if strings.Contains(combined, injected) {
			t.Fatalf("repository local route was accepted or echoed: %q in %s", injected, combined)
		}
	}
}
