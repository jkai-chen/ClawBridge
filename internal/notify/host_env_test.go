package notify

import (
	"context"
	"os"
	"strings"
	"testing"
)

// G9 F3: the Codex queue command runs without ClawBridge's secrets in its
// environment, and with the rest of it.
func TestHostCommandGetsNoClawBridgeSecrets(t *testing.T) {
	env := "/usr/bin/env"
	if _, err := os.Stat(env); err != nil {
		t.Skip("no env command here")
	}
	t.Setenv("CLAWBRIDGE_DEVICE_KEY", "device-seed-F3")
	t.Setenv("CLAWBRIDGE_OAUTH_CLIENT_SECRET", "oauth-secret-F3")
	t.Setenv("NOTIFY_USER_SETTING", "kept-F3")
	out, err := processRunner{}.Run(context.Background(), env, nil)
	if err != nil {
		t.Fatal(err)
	}
	if text := string(out); strings.Contains(text, "seed-F3") || strings.Contains(text, "secret-F3") || !strings.Contains(text, "NOTIFY_USER_SETTING=kept-F3") {
		t.Fatal("the host command's environment:", text)
	}
}
