package cli

import (
	"bytes"
	"clawbridge/internal/doctor"
	"clawbridge/internal/install"
	"clawbridge/internal/skill"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
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
	a, b, e := wizardTerminal, wizardInput, wizardTUI
	t.Cleanup(func() { wizardTerminal, wizardInput, wizardTUI = a, b, e })
	r := &wizardReader{Reader: strings.NewReader(input)}
	wizardTerminal = func(io.Writer) error { return nil }
	wizardInput = func() (cancelreader.CancelReader, error) { return r, nil }
	wizardTUI = func(context.Context, Config, io.Writer, io.Writer) error { t.Fatal("unexpected TUI"); return nil }
	k, l, m, n, o := wizardMethods, wizardKeyStore, wizardInviteCode, wizardDeviceJoin, wizardProjects
	t.Cleanup(func() {
		wizardMethods, wizardKeyStore, wizardInviteCode, wizardDeviceJoin, wizardProjects = k, l, m, n, o
	})
	// A server that advertises no methods is one before device sign-in,
	// which the wizard can't join.
	wizardMethods = func(context.Context, string) ([]string, error) { return nil, nil }
	wizardKeyStore = func(context.Context) error { return nil }
	wizardInviteCode = func() (string, error) { t.Fatal("unexpected invite code read"); return "", nil }
	wizardDeviceJoin = func(context.Context, JoinParams) (JoinResult, error) {
		t.Fatal("unexpected device join")
		return JoinResult{}, nil
	}
	wizardProjects = func(context.Context, string) ([]string, error) { t.Fatal("unexpected project read"); return nil, nil }
	f, g, h, i, j := wizardSkillTargets, wizardSkillInstall, wizardLinearConnect, wizardMCPInstall, wizardDoctor
	t.Cleanup(func() {
		wizardSkillTargets, wizardSkillInstall, wizardLinearConnect, wizardMCPInstall, wizardDoctor = f, g, h, i, j
	})
	// After a join, onboarding sees no configured agent and a passing doctor
	// unless a test says otherwise; it never writes without a stub.
	wizardSkillTargets = func() ([]skill.Target, error) { return nil, errors.New("no agents") }
	wizardSkillInstall = func([]skill.Target) ([]skill.Result, error) { t.Fatal("unexpected skill install"); return nil, nil }
	wizardLinearConnect = func(context.Context, string, string, string, string, io.Writer) (string, error) {
		t.Fatal("unexpected Linear connection")
		return "", nil
	}
	wizardMCPInstall = func(context.Context, string, string, string, string, bool, io.Writer) (install.Result, error) {
		t.Fatal("unexpected MCP install")
		return install.Result{}, nil
	}
	wizardDoctor = func(context.Context, string, string, []string) (doctor.Report, error) {
		return doctor.Report{Status: doctor.Pass}, nil
	}
	return r
}

func TestGuidedNonTTYAndHelpHaveNoEffects(t *testing.T) {
	isolated(t)
	wizardHooks(t, "")
	wizardTerminal = func(io.Writer) error { return errors.New("not terminal") }
	for _, args := range [][]string{{"--help"}, {"help"}, {"-h"}} {
		var out bytes.Buffer
		if err := Run(context.Background(), args, &out, io.Discard); err != nil || !strings.Contains(out.String(), "clawbridge:") {
			t.Fatalf("usage: %v %s", err, &out)
		}
	}
	// With no profile, bare clawbridge prints Start and its commands.
	var out bytes.Buffer
	if err := Run(context.Background(), nil, &out, io.Discard); err != nil || out.String() != startText+startCommands {
		t.Fatalf("start: %v %s", err, &out)
	}
	for _, args := range [][]string{{"--json"}, {"--mode", "cloud"}, {"--workspace", "x"}} {
		if err := Run(context.Background(), args, io.Discard, io.Discard); err == nil {
			t.Fatalf("accepted commandless globals: %v", args)
		}
	}
	if err := Run(context.Background(), []string{"setup", "wizard"}, io.Discard, io.Discard); err == nil || !strings.Contains(err.Error(), "clawbridge join") {
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
	// On Windows, a pipe (like the console wizard input) gets cancelreader's
	// fallback, which cannot interrupt a blocked read: the contract there is
	// that ask gives up after its own second and says cleanup is unconfirmed.
	bound := time.Second
	if runtime.GOOS == "windows" {
		bound = 3 * time.Second
	}
	select {
	case err := <-done:
		if runtime.GOOS == "windows" {
			if err == nil || !strings.Contains(err.Error(), "terminal input cleanup could not be confirmed") {
				t.Fatal(err)
			}
		} else if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-time.After(bound):
		t.Fatal("input reader prevented cancellation")
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
