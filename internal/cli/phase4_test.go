package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"clawbridge/internal/auth"
	"clawbridge/internal/bus"
	"clawbridge/internal/remote"
	"clawbridge/internal/setup"
	"clawbridge/internal/tui"
)

type phase4Executor struct {
	config Config
	path   string
	deny   bool
	t      *testing.T
}

func (e phase4Executor) ResolveProject(context.Context, string, string, string) (string, string, error) {
	if _, err := os.Stat(e.path); !os.IsNotExist(err) {
		e.t.Fatal("route published before authorization")
	}
	return "ws_join", "prj_join", nil
}
func (e phase4Executor) Execute(_ context.Context, r bus.Request) (bus.Result, error) {
	if r.Operation != "whoami" || r.Workspace != "ws_join" || r.Project != "prj_join" || r.Session != "" {
		e.t.Fatalf("unexpected request: %+v", r)
	}
	if e.deny {
		return bus.Result{}, errors.New("forbidden")
	}
	return bus.Result{Source: "server", Data: map[string]any{"online": true, "verified": true, "storage_mode": "cloud", "identity_provenance": "google-id-token", "principal": e.config.Principal, "workspace": map[string]any{"id": "ws_join"}, "project": map[string]any{"id": "prj_join"}}}, nil
}
func TestPhase4JoinStagesUntilAuthoritativePublication(t *testing.T) {
	for _, device := range []bool{false, true} {
		for _, deny := range []bool{false, true} {
			t.Run(strings.Join([]string{map[bool]string{false: "browser", true: "device"}[device], map[bool]string{false: "allow", true: "deny"}[deny]}, "-"), func(t *testing.T) {
				isolated(t)
				path := filepath.Join(t.TempDir(), "client.toml")
				store := &loginMemoryStore{}
				credential := auth.Credential{Principal: "human@example.test", RefreshToken: "synthetic-refresh"}
				installLoginTestHooks(t, store, credential)
				discoverLoginOAuth = func(context.Context, string, *http.Client) (remote.OAuthDiscovery, error) {
					return remote.OAuthDiscovery{BrowserClientID: "desktop", BrowserClientSecret: "desktop-synthetic", DeviceClientID: "device", DeviceClientSecret: "device-synthetic"}, nil
				}
				old := newJoinExecutor
				t.Cleanup(func() { newJoinExecutor = old })
				newJoinExecutor = func(c Config) (setup.Executor, error) {
					want := "desktop"
					if device {
						want = "device"
					}
					if c.OAuthClientID != want || c.Audience != want {
						t.Fatal("wrong promoted route")
					}
					if len(store.values) != 1 {
						t.Fatal("credential not staged before authorization")
					}
					return phase4Executor{c, path, deny, t}, nil
				}
				args := []string{}
				if device {
					args = append(args, "--device")
				}
				var out, errout bytes.Buffer
				err := runJoin(context.Background(), Config{ClientConfig: path, Server: "https://service.example.test", Workspace: "workspace", Project: "project"}, args, &out, &errout)
				if deny {
					if err == nil || out.Len() != 0 || len(store.values) != 0 {
						t.Fatalf("denial failed rollback: %v, %s", err, out.String())
					}
					if _, err = os.Stat(path); !os.IsNotExist(err) {
						t.Fatal("denial published config")
					}
				} else {
					if err != nil {
						t.Fatal(err)
					}
					raw, err := os.ReadFile(path)
					if err != nil {
						t.Fatal(err)
					}
					if !strings.Contains(string(raw), "workspace = 'ws_join'") || !strings.Contains(out.String(), `"joined":true`) || len(store.values) != 1 {
						t.Fatal("join not committed")
					}
				}
			})
		}
	}
}

func TestPhase4AdapterPreflightHasNoSideEffects(t *testing.T) {
	isolated(t)
	for _, args := range [][]string{
		{"--client-config", "relative", "whoami"},
		{"--client-config", "/a", "--client-config", "/b", "whoami"},
		{"doctor", "--mcp-name", "clawbridge"},
		{"doctor", "--mcp-agent", "codex"},
		{"tui", "--screen", "invalid"},
		{"setup", "join", "--workspace", "w", "--project", "p"},
	} {
		var out, errs bytes.Buffer
		if err := Run(context.Background(), args, &out, &errs); err == nil {
			t.Fatalf("accepted %v", args)
		}
	}
	for _, args := range [][]string{{"tui", "--help"}, {"setup", "join", "--help"}, {"mcp", "install", "--help"}} {
		var out, errs bytes.Buffer
		if err := Run(context.Background(), args, &out, &errs); err != nil {
			t.Fatalf("help %v: %v", args, err)
		}
	}
}

func TestPhase4ClientConfigSelectorOverridesEnvironment(t *testing.T) {
	dir := isolated(t)
	selected := filepath.Join(dir, "selected.toml")
	if err := os.WriteFile(selected, []byte("mode='local'\nworkspace='selected'\n"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CLAWBRIDGE_CONFIG", filepath.Join(dir, "missing-other.toml"))
	c, path, err := resolveConfig(context.Background(), Config{ClientConfig: selected})
	if err != nil || path != selected || c.Workspace != "selected" {
		t.Fatalf("selector not pinned: %v", err)
	}
}

func TestPhase4InstallerInheritsCloudRouteAndPinsConfig(t *testing.T) {
	dir := isolated(t)
	path := filepath.Join(dir, "trusted.toml")
	raw := "mode='cloud'\nserver='https://service.example.test'\naudience='desktop'\ncredential_source='oauth'\nprincipal='human@example.test'\noauth_client_id='desktop'\noauth_client_secret='synthetic-private-app'\nworkspace='ws_join'\nproject='prj_join'\nsession='operator'\ninstance='operator-instance'\ncache='cache.db'\n"
	if err := os.WriteFile(path, []byte(raw), 0600); err != nil {
		t.Fatal(err)
	}
	var out, errs bytes.Buffer
	args := []string{"mcp", "install", "--client-config", path, "--session", "agent", "--agent", "claude-code", "--config", filepath.Join(dir, "agent.json"), "--dry-run"}
	if err := Run(context.Background(), args, &out, &errs); err != nil {
		t.Fatal(err)
	}
	value := out.String()
	for _, want := range []string{"--client-config", path, "--credential-source", "oauth", "--session", "agent"} {
		if !strings.Contains(value, want) {
			t.Fatalf("missing safe route field %s", want)
		}
	}
	for _, bad := range []string{"synthetic-private-app", "operator-instance", "oauth-client-secret"} {
		if strings.Contains(value, bad) {
			t.Fatal("private/operator data installed")
		}
	}
	out.Reset()
	if err := Run(context.Background(), []string{"mcp", "install", "--client-config", path, "--agent", "claude-code"}, &out, &errs); err == nil {
		t.Fatal("inherited operator actor")
	}
}

type phase4JoinProbe struct {
	resolve func() (string, string, error)
	execute func(bus.Request) (bus.Result, error)
}

func (p phase4JoinProbe) ResolveProject(context.Context, string, string, string) (string, string, error) {
	return p.resolve()
}
func (p phase4JoinProbe) Execute(_ context.Context, r bus.Request) (bus.Result, error) {
	return p.execute(r)
}

func TestPhase4JoinRestoresReplacedKeychainOnCancellationOrPublicationFailure(t *testing.T) {
	for _, cancelled := range []bool{false, true} {
		t.Run(map[bool]string{false: "publication-failure", true: "cancelled"}[cancelled], func(t *testing.T) {
			isolated(t)
			dir := t.TempDir()
			path := filepath.Join(dir, "client.toml")
			prior := []byte("mode='cloud'\nserver='https://service.example.test'\ncredential_source='oauth'\nprincipal='human@example.test'\noauth_client_id='desktop'\noauth_client_secret='synthetic-app'\naudience='desktop'\n")
			if err := os.WriteFile(path, prior, 0600); err != nil {
				t.Fatal(err)
			}
			oldCredential := auth.Credential{Principal: "human@example.test", ClientID: "desktop", RefreshToken: "old-refresh"}
			store := &loginMemoryStore{values: map[string]auth.Credential{"desktop": oldCredential}}
			installLoginTestHooks(t, store, auth.Credential{Principal: oldCredential.Principal, RefreshToken: "new-refresh"})
			old := newJoinExecutor
			t.Cleanup(func() { newJoinExecutor = old })
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			newJoinExecutor = func(c Config) (setup.Executor, error) {
				return phase4JoinProbe{resolve: func() (string, string, error) { return "ws_join", "prj_join", nil }, execute: func(r bus.Request) (bus.Result, error) {
					if cancelled {
						cancel()
					} else {
						// Replace the containing directory with a file to simulate publication failure.
						if err := os.Rename(dir, dir+"-saved"); err != nil {
							t.Fatal(err)
						}
						t.Cleanup(func() { os.Remove(dir); os.Rename(dir+"-saved", dir) })
						if err := os.WriteFile(dir, []byte("blocked"), 0600); err != nil {
							t.Fatal(err)
						}
					}
					return bus.Result{Source: "server", Data: map[string]any{"online": true, "verified": true, "storage_mode": "cloud", "identity_provenance": "google-id-token", "principal": c.Principal, "workspace": map[string]any{"id": "ws_join"}, "project": map[string]any{"id": "prj_join"}}}, nil
				}}, nil
			}
			var out, errs bytes.Buffer
			err := runJoin(ctx, Config{ClientConfig: path, Server: "https://service.example.test", Workspace: "w", Project: "p"}, []string{"--login"}, &out, &errs)
			if err == nil || out.Len() != 0 || store.values["desktop"].RefreshToken != "old-refresh" {
				t.Fatalf("transaction not restored: %v", err)
			}
			check := path
			if !cancelled {
				check = filepath.Join(dir+"-saved", "client.toml")
			}
			raw, e := os.ReadFile(check)
			if e != nil || !bytes.Equal(raw, prior) {
				t.Fatal("previous route changed")
			}
		})
	}
}

func TestPhase4TUIUsesAdapterAndSelectedScreenWithoutProvisioning(t *testing.T) {
	isolated(t)
	invoke(t, "setup", "--local", "--workspace", "w")
	invoke(t, "project", "create", "p")
	oldCheck, oldRun := checkTUITerminal, runTUIProgram
	t.Cleanup(func() { checkTUITerminal, runTUIProgram = oldCheck, oldRun })
	checkTUITerminal = func(io.Reader, io.Writer) error { return nil }
	called := false
	runTUIProgram = func(ctx context.Context, m tui.Model, _ io.Reader, _ io.Writer) error {
		called = true
		if m.CurrentScreen() != tui.Cloud {
			t.Fatal("initial screen not selected")
		}
		if cmd := m.Init(); cmd == nil {
			t.Fatal("model not connected")
		}
		return nil
	}
	var out, errs bytes.Buffer
	if err := Run(context.Background(), []string{"tui", "--project", "p", "--screen", "cloud"}, &out, &errs); err != nil {
		t.Fatal(err)
	}
	if !called {
		t.Fatal("TUI adapter not invoked")
	}
}

type cleanupProbeStore struct {
	loginMemoryStore
	cleanup          bool
	fail             bool
	cleanupCalls     int
	cleanupContextOK bool
}

func (s *cleanupProbeStore) Save(ctx context.Context, key string, c auth.Credential) error {
	if s.cleanup {
		return s.cleanupResult(ctx, func() error { return s.loginMemoryStore.Save(ctx, key, c) })
	}
	return s.loginMemoryStore.Save(ctx, key, c)
}
func (s *cleanupProbeStore) Delete(ctx context.Context, key string) error {
	return s.cleanupResult(ctx, func() error { return s.loginMemoryStore.Delete(ctx, key) })
}
func (s *cleanupProbeStore) cleanupResult(ctx context.Context, apply func() error) error {
	s.cleanupCalls++
	_, bounded := ctx.Deadline()
	s.cleanupContextOK = ctx.Err() == nil && bounded
	if !s.cleanupContextOK {
		return errors.New("cleanup used canceled or unbounded context")
	}
	if s.fail {
		return errors.New("synthetic-secret-do-not-echo")
	}
	return apply()
}

func TestPhase4JoinRollbackBoundedIndependentContextAndTruthfulFailures(t *testing.T) {
	for _, replace := range []bool{false, true} {
		for _, fail := range []bool{false, true} {
			for _, construction := range []bool{false, true} {
				name := fmt.Sprintf("replace-%t-cleanup-fail-%t-construction-%t", replace, fail, construction)
				t.Run(name, func(t *testing.T) {
					isolated(t)
					path := filepath.Join(t.TempDir(), "client.toml")
					store := &cleanupProbeStore{fail: fail}
					if replace {
						store.values = map[string]auth.Credential{auth.Key("desktop"): {ClientID: "desktop", Principal: "human@example.test", RefreshToken: "prior-refresh"}}
					}
					installLoginTestHooks(t, store, auth.Credential{Principal: "human@example.test", RefreshToken: "new-refresh"})
					ctx, cancel := context.WithCancel(context.Background())
					defer cancel()
					old := newJoinExecutor
					t.Cleanup(func() { newJoinExecutor = old })
					newJoinExecutor = func(Config) (setup.Executor, error) {
						store.cleanup = true
						cancel()
						if construction {
							return nil, errors.New("synthetic-constructor-secret")
						}
						return phase4JoinProbe{resolve: func() (string, string, error) { return "", "", errors.New("resolution unavailable") }, execute: func(bus.Request) (bus.Result, error) {
							t.Fatal("whoami after failed resolution")
							return bus.Result{}, nil
						}}, nil
					}
					var out, errs bytes.Buffer
					err := runJoin(ctx, Config{ClientConfig: path, Server: "https://service.example.test", Workspace: "w", Project: "p", CredentialSource: "oauth", OAuthClientID: "desktop", OAuthClientSecret: "synthetic-app"}, []string{"--login"}, &out, &errs)
					if err == nil || out.Len() != 0 || store.cleanupCalls != 1 || !store.cleanupContextOK {
						t.Fatalf("cleanup not attempted safely: %v calls=%d", err, store.cleanupCalls)
					}
					if fail && !strings.Contains(err.Error(), "partial keychain cleanup") {
						t.Fatal("cleanup failure falsely claimed restoration")
					}
					if strings.Contains(err.Error()+out.String()+errs.String(), "synthetic-secret-do-not-echo") || strings.Contains(err.Error(), "synthetic-constructor-secret") {
						t.Fatal("secret leaked")
					}
					if !fail {
						if replace && store.values[auth.Key("desktop")].RefreshToken != "prior-refresh" {
							t.Fatal("prior entry not restored")
						}
						if !replace && len(store.values) != 0 {
							t.Fatal("new entry not removed")
						}
					}
					if _, e := os.Stat(path); !os.IsNotExist(e) {
						t.Fatal("failed join published route")
					}
				})
			}
		}
	}
}

func TestPhase4JoinReusedCredentialFailureDoesNotAcquireOrRollback(t *testing.T) {
	isolated(t)
	path := filepath.Join(t.TempDir(), "client.toml")
	prior := []byte("mode='cloud'\nserver='https://service.example.test'\ncredential_source='oauth'\nprincipal='human@example.test'\noauth_client_id='desktop'\noauth_client_secret='synthetic-app'\naudience='desktop'\n")
	if err := os.WriteFile(path, prior, 0600); err != nil {
		t.Fatal(err)
	}
	store := &cleanupProbeStore{}
	installLoginTestHooks(t, store, auth.Credential{})
	failLoginAcquisition(t)
	loginCredentialStore = func(string) auth.Store { t.Fatal("reused route invoked acquisition store"); return store }
	old := newJoinExecutor
	t.Cleanup(func() { newJoinExecutor = old })
	newJoinExecutor = func(Config) (setup.Executor, error) { return nil, errors.New("reused credential unavailable") }
	var out, errs bytes.Buffer
	err := runJoin(context.Background(), Config{ClientConfig: path, Server: "https://service.example.test", Workspace: "w", Project: "p"}, nil, &out, &errs)
	if err == nil || out.Len() != 0 || store.saves != 0 || store.cleanupCalls != 0 {
		t.Fatal("reused failure changed credentials")
	}
	raw, e := os.ReadFile(path)
	if e != nil || !bytes.Equal(raw, prior) {
		t.Fatal("reused failure changed config")
	}
}
