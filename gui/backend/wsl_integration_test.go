//go:build wslintegration

package backend

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"
)

// TestWSLLaunch runs the real launcher on a Windows host against a real WSL
// distro: Resolver -> wsl.exe -d <distro> --cd ~ -- sh -c <script> -> the
// distro user's login shell -> a stand-in clawbridge. The distro's
// ~/.profile and ~/.bashrc print noise and the only PATH entry for
// clawbridge comes from ~/.profile, so a pass shows the whole quoting chain,
// the login-shell lookup and the preamble sync together.
//
//	CB_WSL_DISTRO=Ubuntu-24.04 go test -tags wslintegration -run TestWSLLaunch ./backend/
func TestWSLLaunch(t *testing.T) {
	distro := os.Getenv("CB_WSL_DISTRO")
	if distro == "" {
		t.Skip("CB_WSL_DISTRO is not set")
	}
	list, err := ListWSLDistros()
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("wsl.exe -l -q: %q", list)
	r := &Resolver{
		GOOS:     "windows",
		Settings: func() (Settings, error) { return Settings{WSLDistro: distro}, nil },
		LookPath: func(string) (string, error) { return "", os.ErrNotExist },
		Distros:  func() ([]string, error) { return list, nil },
	}
	plan, err := r.Resolve()
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("argv: %q", plan.Cmd.Args)

	ev := newEvents()
	b := New(func() (*Plan, error) { return plan, nil }, ev.emit, "test", "windows")
	b.PreambleTimeout = 90 * time.Second
	b.CallTimeout = 30 * time.Second
	defer b.Stop()
	b.Start()
	st := ev.waitStateFor(t, StateRunning, 120*time.Second)
	t.Logf("status: %+v", st)
	t.Logf("logs: %q", b.Logs())

	if st.Distro != distro || st.ClientVersion != "0.0.0-fake" {
		t.Fatalf("status: %+v", st)
	}
	// The rc files' noise was printed before the preamble and was skipped.
	logs := strings.Join(b.Logs(), "\n")
	for _, want := range []string{"discarded: motd: welcome to the spike", "discarded: bashrc junk"} {
		if !strings.Contains(logs, want) {
			t.Errorf("missing %q in the discarded output:\n%s", want, logs)
		}
	}
	// A second call goes through the live pipe.
	out, err := b.Call("sessions.list", json.RawMessage(`{}`))
	if err != nil || string(out) != "{}" {
		t.Fatalf("sessions.list: %s %v", out, err)
	}
}
