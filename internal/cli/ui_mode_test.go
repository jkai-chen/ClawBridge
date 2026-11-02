package cli

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"clawbridge/internal/guibackend"
	"clawbridge/internal/webui"
)

// fakeUIMachine sets the seams: what is installed, whether a display
// exists, and what the system opener was asked to open. It never starts a
// browser.
func fakeUIMachine(t *testing.T, platform string, apps []string, env map[string]string) (systemOpened *[]string, probed *int) {
	t.Helper()
	oldO, oldP, oldG := uiOpener, uiProbe, uiGetenv
	t.Cleanup(func() { uiOpener, uiProbe, uiGetenv = oldO, oldP, oldG })
	var opened []string
	var n int
	uiOpener = func(string) webui.Opener {
		return func(_ context.Context, target string) (bool, error) {
			opened = append(opened, target)
			return true, nil
		}
	}
	have := map[string]bool{}
	for _, a := range apps {
		have[a] = true
	}
	uiProbe = func(string) webui.Probe {
		n++
		return webui.Probe{
			Platform: platform,
			Exists:   func(p string) bool { return have[p] },
			LookPath: func(name string) (string, error) {
				if have["/usr/bin/"+name] {
					return "/usr/bin/" + name, nil
				}
				return "", context.Canceled
			},
			EvalSymlinks: func(p string) (string, error) { return p, nil },
			Getenv:       func(k string) string { return env[k] },
		}
	}
	uiGetenv = func(k string) string { return env[k] }
	return &opened, &n
}

func TestPickUIMode(t *testing.T) {
	for _, tc := range []struct {
		name     string
		platform string
		apps     []string
		env      map[string]string
		browser  bool
		wantApp  bool
		wantNote bool
		probed   bool
	}{
		{"mac default is the app window", "darwin", []string{"/Applications/Google Chrome.app"}, nil, false, true, false, true},
		{"mac --browser is the tab", "darwin", []string{"/Applications/Google Chrome.app"}, nil, true, false, false, false},
		{"mac none found", "darwin", nil, nil, false, false, true, true},
		{"linux with DISPLAY", "linux", []string{"/usr/bin/chromium"}, map[string]string{"DISPLAY": ":0"}, false, true, false, true},
		{"linux with Wayland", "linux", []string{"/usr/bin/chromium"}, map[string]string{"WAYLAND_DISPLAY": "wayland-0"}, false, true, false, true},
		{"linux no display keeps the old path, no note", "linux", []string{"/usr/bin/chromium"}, nil, false, false, false, false},
		{"linux display, none found", "linux", nil, map[string]string{"DISPLAY": ":0"}, false, false, true, true},
	} {
		opened, probed := fakeUIMachine(t, tc.platform, tc.apps, tc.env)
		var errOut bytes.Buffer
		m := pickUIMode(tc.platform, tc.browser, "/state/ui", "/p/a.toml", &errOut)
		if (m.app != nil) != tc.wantApp {
			t.Errorf("%s: app=%v", tc.name, m.app != nil)
		}
		if got := strings.Count(errOut.String(), noBrowserNote); (got == 1) != tc.wantNote || got > 1 {
			t.Errorf("%s: note count %d (%q)", tc.name, got, errOut.String())
		}
		if (*probed > 0) != tc.probed {
			t.Errorf("%s: probed %d times", tc.name, *probed)
		}
		if !tc.wantApp {
			// The old path: the system opener, exactly as before.
			m.opener(context.Background(), "file:///x")
			if len(*opened) != 1 {
				t.Errorf("%s: the system opener was not used", tc.name)
			}
		}
	}
	if noBrowserNote != "No Chrome, Edge, Chromium or Brave found; opening your default browser instead, as clawbridge ui --browser does." {
		t.Error("the fallback line changed")
	}
}

// --no-open wins over the app window and over --browser: nothing is looked
// for or opened, and the link is printed.
func TestUINoOpenBeatsBrowserAndApp(t *testing.T) {
	for _, args := range [][]string{{"ui", "--no-open"}, {"ui", "--no-open", "--browser"}} {
		opened, probed := fakeUIMachine(t, "darwin", []string{"/Applications/Google Chrome.app"}, nil)
		localUIProject(t)
		old := closesEnv
		t.Cleanup(func() { closesEnv = old })
		closesEnv = func() []string { return []string{"PATH=/bin"} }
		ctx, cancel := context.WithCancel(context.Background())
		var out, errOut safeBuffer
		done := make(chan error, 1)
		go func() { done <- Run(ctx, args, &out, &errOut) }()
		deadline := time.Now().Add(10 * time.Second)
		for !strings.Contains(out.String(), "?t=") && time.Now().Before(deadline) {
			time.Sleep(20 * time.Millisecond)
		}
		cancel()
		runErr := <-done
		if !strings.Contains(out.String(), "Open this link in your browser") {
			t.Fatalf("%v: no link (%v): out %q err %q", args, runErr, out.String(), errOut.String())
		}
		if len(*opened) != 0 || *probed != 0 {
			t.Errorf("%v: opened %v, probed %d", args, *opened, *probed)
		}
		if strings.Contains(errOut.String(), noBrowserNote) {
			t.Errorf("%v: printed the fallback note", args)
		}
	}
}

func TestHintIfNoWindow(t *testing.T) {
	b := &guibackend.Server{}
	var out bytes.Buffer
	hintIfNoWindow(context.Background(), &out, b, 10*time.Millisecond)
	if out.String() != "If no ClawBridge window opened, run clawbridge ui --browser.\n" {
		t.Errorf("hint %q", out.String())
	}
	// A canceled run prints nothing.
	out.Reset()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	hintIfNoWindow(ctx, &out, b, time.Hour)
	if out.Len() != 0 {
		t.Errorf("printed %q", out.String())
	}
}

// M1: a browser app keeps running, so the opener reports (false, nil). That
// must not print any hint; only the 15 s connection check speaks.
func TestAppOpenIsSilentWhenAWindowConnects(t *testing.T) {
	b := &guibackend.Server{}
	ts := httptest.NewServer(b.Events())
	defer ts.Close()
	var errOut safeBuffer
	done := make(chan struct{})
	go func() {
		announceApp(context.Background(), &errOut, b, func(context.Context) (bool, error) { return false, nil }, 400*time.Millisecond)
		close(done)
	}()
	// The window connects within the wait.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, ts.URL, nil)
	go func() {
		if resp, err := http.DefaultClient.Do(req); err == nil {
			io.Copy(io.Discard, resp.Body)
		}
	}()
	for i := 0; i < 200 && b.Connected() == 0; i++ {
		time.Sleep(10 * time.Millisecond)
	}
	if b.Connected() != 1 {
		t.Fatal("the fake window did not connect")
	}
	<-done
	if errOut.String() != "" {
		t.Errorf("stderr %q, want empty", errOut.String())
	}
}

func TestAppOpenWithNoConnectionPrintsOnlyTheHint(t *testing.T) {
	var errOut safeBuffer
	announceApp(context.Background(), &errOut, &guibackend.Server{}, func(context.Context) (bool, error) { return false, nil }, 20*time.Millisecond)
	if errOut.String() != "If no ClawBridge window opened, run clawbridge ui --browser.\n" {
		t.Errorf("stderr %q", errOut.String())
	}
}

func TestAppOpenReportsAStartError(t *testing.T) {
	var errOut safeBuffer
	announceApp(context.Background(), &errOut, &guibackend.Server{}, func(context.Context) (bool, error) {
		return false, errors.New("cannot start the system browser opener")
	}, time.Hour)
	got := errOut.String()
	if !strings.Contains(got, "Could not open a browser (cannot start the system browser opener)") || strings.Contains(got, "If no ClawBridge window") || strings.Contains(got, "?t=") {
		t.Errorf("stderr %q", got)
	}
}

func TestUIHelpShowsBrowserFlag(t *testing.T) {
	var out, errOut bytes.Buffer
	Run(context.Background(), []string{"ui", "--help"}, &out, &errOut)
	if !strings.Contains(out.String()+errOut.String(), "-browser") {
		t.Errorf("help: %q %q", out.String(), errOut.String())
	}
}

type safeBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *safeBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *safeBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}
