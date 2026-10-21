package cli

import (
	"bytes"
	"clawbridge/internal/auth"
	"clawbridge/internal/remote"
	"clawbridge/internal/setup"
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/muesli/cancelreader"
)

type wizardReader struct {
	io.Reader
	closed   bool
	closeErr error
}

func (r *wizardReader) Cancel() bool { return true }
func (r *wizardReader) Close() error { r.closed = true; return r.closeErr }
func wizardHooks(t *testing.T, input string) *wizardReader {
	t.Helper()
	a, b, c, d, e := wizardTerminal, wizardInput, wizardKeyring, wizardJoin, wizardTUI
	t.Cleanup(func() { wizardTerminal, wizardInput, wizardKeyring, wizardJoin, wizardTUI = a, b, c, d, e })
	r := &wizardReader{Reader: strings.NewReader(input)}
	wizardTerminal = func(io.Writer) error { return nil }
	wizardInput = func() (cancelreader.CancelReader, error) { return r, nil }
	wizardKeyring = func(context.Context) error { t.Fatal("unexpected keyring access"); return nil }
	wizardJoin = func(context.Context, Config, []string, io.Writer, io.Writer) error {
		t.Fatal("unexpected join")
		return nil
	}
	wizardTUI = func(context.Context, Config, io.Writer, io.Writer) error { t.Fatal("unexpected TUI"); return nil }
	return r
}

func TestGuidedNonTTYAndHelpHaveNoEffects(t *testing.T) {
	isolated(t)
	wizardHooks(t, "")
	wizardTerminal = func(io.Writer) error { return errors.New("not terminal") }
	for _, args := range [][]string{nil, {"--help"}, {"help"}, {"-h"}} {
		var out bytes.Buffer
		if err := Run(context.Background(), args, &out, io.Discard); err != nil || !strings.Contains(out.String(), "clawbridge:") {
			t.Fatalf("usage: %v %s", err, &out)
		}
	}
	for _, args := range [][]string{{"--json"}, {"--mode", "cloud"}, {"--workspace", "x"}} {
		if err := Run(context.Background(), args, io.Discard, io.Discard); err == nil {
			t.Fatalf("accepted commandless globals: %v", args)
		}
	}
	if err := Run(context.Background(), []string{"setup", "wizard"}, io.Discard, io.Discard); err == nil || !strings.Contains(err.Error(), "setup join") {
		t.Fatalf("TTY gate: %v", err)
	}
}

func TestGuidedDeclineEOFAndInvalidTargetDoNotAcquire(t *testing.T) {
	for _, input := range []string{"", "https://service.test\nws\nprj\ndevice\nn\n", "https://user:secret@service.test\nws\nprj\ndevice\ny\n"} {
		t.Run(input, func(t *testing.T) {
			isolated(t)
			wizardHooks(t, input)
			var out bytes.Buffer
			_ = Run(context.Background(), []string{"setup", "wizard"}, &out, io.Discard)
			if strings.Contains(out.String(), "secret") {
				t.Fatal("invalid URL displayed")
			}
		})
	}
}

func TestGuidedHandoffClosesReaderAndPreservesPublishedRouteOnTUIFailure(t *testing.T) {
	isolated(t)
	path := filepath.Join(t.TempDir(), "client.toml")
	r := wizardHooks(t, "https://service.test\nws\nprj\ndevice\ny\nTUI-INPUT")
	calls := []string{}
	wizardKeyring = func(context.Context) error {
		if !r.closed {
			t.Fatal("input not released before acquisition")
		}
		calls = append(calls, "keyring")
		return nil
	}
	wizardJoin = func(ctx context.Context, c Config, args []string, out, errout io.Writer) error {
		calls = append(calls, "join")
		if c.Server != "https://service.test" || c.Session != "" || c.Instance != "" || c.CredentialSource != "oauth" || strings.Join(args, "") != "--device" {
			t.Fatalf("candidate: %+v %v", c, args)
		}
		if guided, _ := ctx.Value(guidedRouteKey{}).(bool); !guided {
			t.Fatal("missing isolation")
		}
		rest, _ := io.ReadAll(r.Reader)
		if string(rest) != "TUI-INPUT" {
			t.Fatalf("wizard consumed TUI input: %q", rest)
		}
		return os.WriteFile(c.ClientConfig, []byte("published-route"), 0600)
	}
	wizardTUI = func(_ context.Context, c Config, _, _ io.Writer) error {
		calls = append(calls, "tui")
		if c.ClientConfig != path {
			t.Fatal("selector lost")
		}
		return errors.New("synthetic TUI failure")
	}
	var out bytes.Buffer
	err := Run(context.Background(), []string{"--client-config", path, "setup", "wizard"}, &out, io.Discard)
	if strings.Contains(out.String(), "availability checked") || !strings.Contains(out.String(), "availability, unlock, and write readiness are not guaranteed") {
		t.Fatalf("misleading credential-store status: %q", out.String())
	}
	if err == nil || !strings.Contains(err.Error(), "join completed; saved route preserved") {
		t.Fatalf("untruthful failure: %v", err)
	}
	if strings.Join(calls, ",") != "keyring,join,tui" {
		t.Fatal(calls)
	}
	raw, _ := os.ReadFile(path)
	if string(raw) != "published-route" {
		t.Fatal("published route lost")
	}
}

func TestGuidedExistingConfigNeverReplacedOrRepaired(t *testing.T) {
	for _, body := range []string{"invalid = [", "mode='cloud'\n", "mode='local'\nworkspace='ws'\nproject='prj'\n"} {
		t.Run(body, func(t *testing.T) {
			isolated(t)
			wizardHooks(t, "")
			path := filepath.Join(t.TempDir(), "client.toml")
			os.WriteFile(path, []byte(body), 0600)
			called := false
			wizardTUI = func(context.Context, Config, io.Writer, io.Writer) error { called = true; return nil }
			err := Run(context.Background(), []string{"--client-config", path}, io.Discard, io.Discard)
			if strings.Contains(body, "project=") {
				if err != nil || !called {
					t.Fatalf("existing route: %v", err)
				}
			} else if err == nil || called {
				t.Fatal("invalid config reached TUI")
			}
			raw, _ := os.ReadFile(path)
			if string(raw) != body {
				t.Fatal("config changed")
			}
		})
	}
}

type observedWizardReader struct {
	cancelreader.CancelReader
	entered chan struct{}
	once    sync.Once
}

func (r *observedWizardReader) Read(p []byte) (int, error) {
	r.once.Do(func() { close(r.entered) })
	return r.CancelReader.Read(p)
}

func TestGuidedCancelReleasesBlockedInput(t *testing.T) {
	isolated(t)
	wizardHooks(t, "")
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	defer w.Close()
	entered := make(chan struct{})
	wizardInput = func() (cancelreader.CancelReader, error) {
		native, e := cancelreader.NewReader(r)
		if e != nil {
			return nil, e
		}
		return &observedWizardReader{CancelReader: native, entered: entered}, nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- Run(ctx, []string{"setup", "wizard"}, io.Discard, io.Discard) }()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("read never started")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("input reader prevented cancellation")
	}
}

func TestGuidedResolverIgnoresInheritedAuthorityAndActors(t *testing.T) {
	isolated(t)
	path := filepath.Join(t.TempDir(), "client.toml")
	for _, key := range []string{"MODE", "SERVER", "AUDIENCE", "ACCOUNT", "IMPERSONATE_SERVICE_ACCOUNT", "PRINCIPAL", "CACHE", "CREDENTIAL_SOURCE", "CREDENTIALS_FILE", "SESSION", "INSTANCE", "OAUTH_CLIENT_ID", "OAUTH_CLIENT_SECRET"} {
		t.Setenv("CLAWBRIDGE_"+key, "malicious")
	}
	ctx := context.WithValue(context.Background(), guidedRouteKey{}, true)
	c, _, err := resolveLoginConfig(ctx, Config{ClientConfig: path, Mode: "cloud", CredentialSource: "oauth", Server: "https://selected.test", Workspace: "ws", Project: "prj"}, false)
	if err != nil || c.Server != "https://selected.test" || c.Session != "" || c.Instance != "" || c.OAuthClientID != "" || c.Account != "" || c.Principal != "" || c.Cache != "" {
		t.Fatalf("isolation failed: %v %+v", err, c)
	}
}

func TestGuidedUsesExistingJoinTransaction(t *testing.T) {
	isolated(t)
	path := filepath.Join(t.TempDir(), "client.toml")
	wizardHooks(t, "https://service.test\nworkspace\nproject\ndevice\ny\n")
	wizardJoin = runJoin
	store := &loginMemoryStore{}
	installLoginTestHooks(t, store, auth.Credential{Principal: "human@example.test", RefreshToken: "synthetic-refresh"})
	discoverLoginOAuth = func(context.Context, string, *http.Client) (remote.OAuthDiscovery, error) {
		return remote.OAuthDiscovery{BrowserClientID: "desktop", BrowserClientSecret: "synthetic-desktop", DeviceClientID: "device", DeviceClientSecret: "synthetic-device"}, nil
	}
	old := newJoinExecutor
	t.Cleanup(func() { newJoinExecutor = old })
	newJoinExecutor = func(c Config) (setup.Executor, error) { return phase4Executor{c, path, false, t}, nil }
	wizardKeyring = func(context.Context) error { return nil }
	wizardTUI = func(_ context.Context, c Config, _, _ io.Writer) error {
		raw, err := os.ReadFile(c.ClientConfig)
		if err != nil || !strings.Contains(string(raw), "ws_join") || len(store.values) != 1 {
			t.Fatal("TUI before committed join")
		}
		return nil
	}
	var out bytes.Buffer
	if err := Run(context.Background(), []string{"--client-config", path, "setup", "wizard"}, &out, io.Discard); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), "synthetic-") || strings.Contains(out.String(), `"joined"`) {
		t.Fatal("wizard leaked credential or automation JSON")
	}
}

func TestGuidedKeyringFailurePrecedesAcquisitionAndSafeSummary(t *testing.T) {
	isolated(t)
	wizardHooks(t, "https://service.test\nws\x1b[2J\nprj\ndevice\ny\n")
	wizardKeyring = func(context.Context) error { return errors.New("native store unavailable") }
	var out bytes.Buffer
	err := Run(context.Background(), []string{"setup", "wizard"}, &out, io.Discard)
	if err == nil || strings.Contains(out.String(), "\x1b") {
		t.Fatalf("unsafe preflight: %v %q", err, out.String())
	}
}

type closeOnlyWizardReader struct {
	entered, release chan struct{}
	once             sync.Once
}

func (r *closeOnlyWizardReader) Read([]byte) (int, error) {
	close(r.entered)
	<-r.release
	return 0, io.EOF
}
func (r *closeOnlyWizardReader) Cancel() bool { return false }
func (r *closeOnlyWizardReader) Close() error { r.once.Do(func() { close(r.release) }); return nil }

func TestGuidedCancelFalseClosesReaderBeforeWaiting(t *testing.T) {
	isolated(t)
	wizardHooks(t, "")
	r := &closeOnlyWizardReader{entered: make(chan struct{}), release: make(chan struct{})}
	wizardInput = func() (cancelreader.CancelReader, error) { return r, nil }
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- Run(ctx, []string{"setup", "wizard"}, io.Discard, io.Discard) }()
	select {
	case <-r.entered:
	case <-time.After(time.Second):
		t.Fatal("reader not entered")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("Cancel false left blocked read")
	}
}

func TestGuidedCloseFailureBlocksCredentialAndAuthorityHandoff(t *testing.T) {
	for _, answer := range []string{"y", "n"} {
		t.Run(answer, func(t *testing.T) {
			isolated(t)
			path := filepath.Join(t.TempDir(), "client.toml")
			r := wizardHooks(t, "https://service.test\nws\nprj\ndevice\n"+answer+"\n")
			r.closeErr = errors.New("synthetic close failure with private-native-detail")
			var out bytes.Buffer
			err := Run(context.Background(), []string{"--client-config", path, "setup", "wizard"}, &out, io.Discard)
			if err == nil || !strings.Contains(err.Error(), "terminal input cleanup failed") || strings.Contains(err.Error(), "private-native-detail") {
				t.Fatalf("unsafe close result: %v", err)
			}
			if !r.closed {
				t.Fatal("close not attempted")
			}
			if _, e := os.Stat(path); !os.IsNotExist(e) {
				t.Fatal("route touched")
			}
			if strings.Contains(out.String(), "Join completed") {
				t.Fatal("success printed despite close failure")
			}
		})
	}
}
