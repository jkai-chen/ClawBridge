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

func TestSignInShowsTheURLWhenTheLaunchIsNotConfirmed(t *testing.T) {
	original := browserWait
	t.Cleanup(func() { browserWait = original })
	browserWait = 100 * time.Millisecond
	var shown []string
	show := func(u string) { shown = append(shown, u) }
	fakeOpener(t, "sleep:5s")
	if err := openBrowser(context.Background(), "https://accounts.example.test/auth", show); err != nil || len(shown) != 1 {
		t.Fatal("unconfirmed launch did not show the URL", err, shown)
	}
	browserWait = 10 * time.Second // let even a slow-starting opener report its failure
	fakeOpener(t, "exit:3")
	if err := openBrowser(context.Background(), "https://accounts.example.test/auth", show); err != nil || len(shown) != 2 {
		t.Fatal("failed launch did not fall back to showing the URL", err, shown)
	}
	if err := openBrowser(context.Background(), "https://accounts.example.test/auth", nil); err == nil {
		t.Fatal("failed launch without a way to show the URL was not an error")
	}
	fakeOpener(t, "exit:0")
	if err := openBrowser(context.Background(), "https://accounts.example.test/auth", show); err != nil || len(shown) != 2 {
		t.Fatal("confirmed launch still showed the URL", err, shown)
	}
	if err := openBrowser(context.Background(), "http://insecure.example.test", show); err == nil {
		t.Fatal("non-https sign-in URL opened")
	}
}
