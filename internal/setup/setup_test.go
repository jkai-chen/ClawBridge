package setup

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"clawbridge/internal/bus"
)

type joinExecutor struct {
	in JoinInput
}

func (e joinExecutor) Execute(_ context.Context, request bus.Request) (bus.Result, error) {
	switch request.Operation {
	case "resolve_project":
		return bus.Result{Source: "server", Data: map[string]any{
			"workspace_id": "ws_1", "project_id": "prj_1",
		}}, nil
	case "whoami":
		return bus.Result{Source: "server", Data: map[string]any{
			"principal": strings.ToLower(e.in.Principal), "verified": true,
			"storage_mode": "cloud", "identity_provenance": "google-id-token",
			"online": true, "workspace": map[string]any{"id": "ws_1"},
			"project": map[string]any{"id": "prj_1"},
			"session": map[string]any{"instance": e.in.Instance},
		}}, nil
	default:
		return bus.Result{}, errors.New("unexpected operation")
	}
}

func (e joinExecutor) ResolveProject(context.Context, string, string, string) (string, string, error) {
	return "ws_1", "prj_1", nil
}

func oauthJoin(t *testing.T) JoinInput {
	t.Helper()
	dir := t.TempDir()
	return JoinInput{
		ConfigPath: filepath.Join(dir, "config.toml"),
		Workspace:  "w", Project: "p", GitRemote: "https://example.test/org/repo.git",
		Session: "alice", Instance: "instance-1",
		Server: "https://server.example.test", Audience: "desktop-client",
		Principal: "alice@example.test", Cache: filepath.Join(dir, "cache.db"),
		CredentialSource: "oauth", OAuthClientID: "desktop-client",
		OAuthClientSecret: "synthetic-desktop-app", OAuthDeviceClientSecret: "synthetic-device-app",
		OAuthDeviceClientID: "device-client", OAuthKeyringService: "review-keyring",
	}
}

func TestJoinPersistsOnlySafeOAuthRouteAfterVerification(t *testing.T) {
	in := oauthJoin(t)
	result, err := Join(context.Background(), in, joinExecutor{in: in})
	if err != nil {
		t.Fatal(err)
	}
	if !result.Changed || result.Principal != in.Principal {
		t.Fatalf("unexpected join result: %+v", result)
	}
	raw, err := os.ReadFile(in.ConfigPath)
	if err != nil {
		t.Fatal(err)
	}
	config := string(raw)
	for _, wanted := range []string{
		"mode = 'cloud'", "credential_source = 'oauth'", "oauth_client_id = 'desktop-client'",
		"oauth_device_client_id = 'device-client'", "oauth_keyring_service = 'review-keyring'",
		"server = 'https://server.example.test'", "audience = 'desktop-client'",
		"oauth_client_secret = 'synthetic-desktop-app'", "oauth_device_client_secret = 'synthetic-device-app'",
	} {
		if !strings.Contains(config, wanted) {
			t.Fatalf("trusted OAuth join omitted %q: %s", wanted, config)
		}
	}
	for _, forbidden := range []string{"refresh_token", "id_token", "access_token"} {
		if strings.Contains(config, forbidden) {
			t.Fatalf("trusted join persisted secret-like field %q: %s", forbidden, config)
		}
	}
}

func TestJoinRejectsMixedAndImplicitCredentialRoutesWithoutWriting(t *testing.T) {
	for name, mutate := range map[string]func(*JoinInput){
		"implicit": func(in *JoinInput) { in.CredentialSource = "" },
		"mixed":    func(in *JoinInput) { in.Account = "human@example.test" },
		"unknown":  func(in *JoinInput) { in.CredentialSource = "metadata" },
	} {
		t.Run(name, func(t *testing.T) {
			in := oauthJoin(t)
			mutate(&in)
			if _, err := Join(context.Background(), in, joinExecutor{in: in}); err == nil {
				t.Fatal("invalid join route was accepted")
			}
			if _, err := os.Stat(in.ConfigPath); !os.IsNotExist(err) {
				t.Fatal("invalid join wrote trusted configuration")
			}
		})
	}
}

func TestGCloudRunnerConsumesFullCommandExactlyOnce(t *testing.T) {
	fake := filepath.Join(t.TempDir(), "fake-gcloud")
	if err := os.WriteFile(fake, []byte("#!/bin/sh\nprintf '%s\\n' \"$@\"\n"), 0700); err != nil {
		t.Fatal(err)
	}
	runner := GCloudRunner{Path: fake}
	command := []string{"gcloud", "projects", "describe", "review-project", "--quiet"}
	output, err := runner.Run(context.Background(), command)
	if err != nil {
		t.Fatal(err)
	}
	if string(output) != "projects\ndescribe\nreview-project\n--quiet\n" {
		t.Fatal("full command arguments were not passed exactly once")
	}
	if command[0] != "gcloud" || len(command) != 5 {
		t.Fatal("reviewed command was mutated")
	}
	for _, command := range [][]string{nil, {"gcloud"}, {"other", "projects"}, {"/tmp/gcloud", "projects"}} {
		if _, err := runner.Run(context.Background(), command); err == nil {
			t.Fatal("invalid executable token accepted")
		}
	}
}

type emptyJoinExecutor struct {
	joinExecutor
	workspace, project string
	calls              *int
}

func (e emptyJoinExecutor) ResolveProject(context.Context, string, string, string) (string, string, error) {
	return e.workspace, e.project, nil
}
func (e emptyJoinExecutor) Execute(context.Context, bus.Request) (bus.Result, error) {
	*e.calls++
	return bus.Result{}, nil
}
func TestJoinRejectsEmptyResolvedIDsBeforeWhoami(t *testing.T) {
	for _, ids := range [][2]string{{"", "prj"}, {"ws", ""}, {" ", "prj"}} {
		in := oauthJoin(t)
		calls := 0
		_, err := Join(context.Background(), in, emptyJoinExecutor{joinExecutor{in}, ids[0], ids[1], &calls})
		if err == nil || calls != 0 {
			t.Fatal("empty resolution reached authorization")
		}
		if _, err = os.Stat(in.ConfigPath); !os.IsNotExist(err) {
			t.Fatal("published empty scope")
		}
	}
}
func TestJoinInputDiagnosticsRedactPrivatePairs(t *testing.T) {
	in := oauthJoin(t)
	in.OAuthClientSecret = "synthetic-desktop-secret"
	in.OAuthDeviceClientSecret = "synthetic-device-secret"
	for _, format := range []string{"%v", "%+v", "%#v"} {
		value := fmt.Sprintf(format, in)
		if strings.Contains(value, in.OAuthClientSecret) || strings.Contains(value, in.OAuthDeviceClientSecret) {
			t.Fatal("private pair in diagnostics")
		}
	}
}

type forbiddenJoinExecutor struct{ t *testing.T }

func (e forbiddenJoinExecutor) ResolveProject(context.Context, string, string, string) (string, string, error) {
	e.t.Fatal("incomplete credential reached authority resolution")
	return "", "", nil
}
func (e forbiddenJoinExecutor) Execute(context.Context, bus.Request) (bus.Result, error) {
	e.t.Fatal("incomplete credential reached authority execution")
	return bus.Result{}, nil
}
func TestJoinRejectsIncompleteOAuthPairsBeforeAuthority(t *testing.T) {
	for name, mutate := range map[string]func(*JoinInput){
		"active-secret-missing": func(in *JoinInput) { in.OAuthClientSecret = "" },
		"active-secret-blank":   func(in *JoinInput) { in.OAuthClientSecret = " " },
		"device-secret-missing": func(in *JoinInput) { in.OAuthDeviceClientSecret = "" },
		"device-id-missing":     func(in *JoinInput) { in.OAuthDeviceClientID = "" },
	} {
		t.Run(name, func(t *testing.T) {
			in := oauthJoin(t)
			mutate(&in)
			if _, err := Join(context.Background(), in, forbiddenJoinExecutor{t}); err == nil {
				t.Fatal("incomplete OAuth pair accepted")
			}
			if _, err := os.Stat(in.ConfigPath); !os.IsNotExist(err) {
				t.Fatal("incomplete route published")
			}
		})
	}
	in := oauthJoin(t)
	in.OAuthDeviceClientID = ""
	in.OAuthDeviceClientSecret = ""
	if _, err := Join(context.Background(), in, joinExecutor{in}); err != nil {
		t.Fatalf("optional device pair required: %v", err)
	}
}
