package auth

import (
	"context"
	"os/exec"
	"testing"
	"time"

	"clawbridge/internal/faketool"
)

func fakeOpener(t *testing.T, behaviour string) {
	t.Helper()
	path := faketool.Install(t, t.TempDir(), "opener", behaviour)
	original := browserCommand
	t.Cleanup(func() { browserCommand = original })
	browserCommand = func(target string) *exec.Cmd { return exec.Command(path, target) }
}

func TestLaunchBrowserReportsOnlyWhatItKnows(t *testing.T) {
	ctx := context.Background()
	fakeOpener(t, "exit:0")
	if opened, err := LaunchBrowser(ctx, "https://example.test", 10*time.Second); !opened || err != nil {
		t.Fatal("clean opener exit not reported as opened", opened, err)
	}
	fakeOpener(t, "exit:3")
	if opened, err := LaunchBrowser(ctx, "https://example.test", 10*time.Second); opened || err == nil {
		t.Fatal("failed opener reported as opened", opened, err)
	}
	// An opener that stays in the foreground (xdg-open with a new browser)
	// is neither a failure nor confirmed, and is left running.
	fakeOpener(t, "sleep:5s")
	start := time.Now()
	opened, err := LaunchBrowser(ctx, "https://example.test", 100*time.Millisecond)
	if opened || err != nil || time.Since(start) > time.Second {
		t.Fatal("foreground opener not reported as unknown promptly", opened, err)
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if opened, err = LaunchBrowser(canceled, "https://example.test", time.Minute); opened || err != nil {
		t.Fatal("canceled wait not reported as unknown", opened, err)
	}
}
