package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"clawbridge/internal/guibackend"
	"clawbridge/internal/webui"
)

// R1's acceptance list: start-up under an agent marker is refused, before
// anything is opened, bound or written.
func TestUIRefusesUnderAnAgentMarker(t *testing.T) {
	home := t.TempDir()
	t.Setenv("CLAWBRIDGE_HOME", home)
	old := closesEnv
	t.Cleanup(func() { closesEnv = old })
	for _, marker := range []string{"CLAUDECODE=1", "CODEX_THREAD_ID=x", "CLAWBRIDGE_SESSION=s"} {
		closesEnv = func() []string { return []string{"PATH=/bin", marker} }
		var out, errOut bytes.Buffer
		err := Run(context.Background(), []string{"ui", "--no-open"}, &out, &errOut)
		if err == nil || !strings.Contains(err.Error(), "an agent runs this command") {
			t.Errorf("%s: %v", marker, err)
		}
		if out.Len() != 0 {
			t.Errorf("%s: printed %q", marker, out.String())
		}
	}
	if entries, _ := os.ReadDir(home); len(entries) != 0 {
		t.Errorf("a refused start wrote %v", entries)
	}
}

func TestUIRefusesWithSessionFlag(t *testing.T) {
	t.Setenv("CLAWBRIDGE_HOME", t.TempDir())
	old := closesEnv
	t.Cleanup(func() { closesEnv = old })
	closesEnv = func() []string { return []string{"PATH=/bin"} }
	err := Run(context.Background(), []string{"--session", "worker", "ui"}, &bytes.Buffer{}, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "--session") {
		t.Errorf("%v", err)
	}
}

func TestUsageNamesTheUICommand(t *testing.T) {
	if !strings.Contains(Usage, "ui [--no-open] [--port N]") {
		t.Error("usage does not name clawbridge ui")
	}
}

const fileRouteNote = "If the browser didn't open ClawBridge, run `clawbridge ui --no-open` for a link."

// U2: on the file route the note is printed even when the opener says it
// opened, and it carries neither the path nor a token.
func TestUIFileRoutePrintsTheNoOpenNote(t *testing.T) {
	var out bytes.Buffer
	openUI(context.Background(), &out, true, func(context.Context) (bool, error) { return true, nil })
	if !strings.Contains(out.String(), fileRouteNote) {
		t.Errorf("no note: %q", out.String())
	}
	for _, bad := range []string{"file://", "?t=", ".html", "http"} {
		if strings.Contains(out.String(), bad) {
			t.Errorf("note holds %q", bad)
		}
	}
	// Off the file route, an opener that succeeded says nothing.
	out.Reset()
	openUI(context.Background(), &out, false, func(context.Context) (bool, error) { return true, nil })
	if out.Len() != 0 {
		t.Errorf("printed %q", out.String())
	}
}

// The second instance's hand-off opens through the same rule and prints the
// note, and the token and the launch file's path go only to the opener.
func TestUIHandOffPrintsTheNoOpenNote(t *testing.T) {
	short, err := os.MkdirTemp("", "cbcli")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(short)
	base := filepath.Join(short, "ui")
	srv, err := webui.New(webui.Config{Backend: &guibackend.Server{}, StateDir: base, Profile: "/p/a.toml", Platform: "linux"})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- srv.Serve(ctx) }()
	defer func() { cancel(); <-done }()
	var opened []string
	old := uiOpener
	t.Cleanup(func() { uiOpener = old })
	uiOpener = func(string) webui.Opener {
		return func(_ context.Context, target string) (bool, error) {
			opened = append(opened, target)
			return true, nil
		}
	}
	var out, errOut bytes.Buffer
	if err := handOffUI(ctx, base, "/p/a.toml", "linux", false, &out, &errOut); err != nil {
		t.Fatal(err)
	}
	if len(opened) != 1 || !strings.HasPrefix(opened[0], "file://") {
		t.Fatalf("opened %v", opened)
	}
	if !strings.Contains(errOut.String(), fileRouteNote) {
		t.Errorf("no note: %q", errOut.String())
	}
	for _, s := range []string{out.String(), errOut.String()} {
		if strings.Contains(s, "file://") || strings.Contains(s, "?t=") || strings.Contains(s, ".html") {
			t.Errorf("printed a path or token: %q", s)
		}
	}
	// Another profile does not get this server.
	if err := handOffUI(ctx, base, "/p/b.toml", "linux", false, &out, &errOut); err == nil {
		t.Error("handed off across profiles")
	}
}
