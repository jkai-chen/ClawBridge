package integration

import (
	"bytes"
	"context"
	"errors"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"clawbridge/internal/devicesign"
)

// The released binary's serve, since v1.0 removed Google sign-in: a start
// that names Google exits 2 with the swap runbook's text and nothing else
// on stderr, before the database; Google sign-in's five old settings left
// in the environment are ignored with one warning that prints no value.
func TestServeRefusesGoogleSignInBeforePostgres(t *testing.T) {
	const (
		browserSecret = "synthetic-browser-public-app-credential-must-not-log"
		deviceSecret  = "synthetic-device-public-app-credential-must-not-log"
	)
	p := newProcessFixture(t)
	baseEnv := append([]string{}, p.env...)
	baseEnv = append(baseEnv,
		"CLAWBRIDGE_DATABASE_URL=postgres://review:not-a-real-password@127.0.0.1:1/review?sslmode=disable",
		"CLAWBRIDGE_AUDIENCE=desktop.example.test",
		"CLAWBRIDGE_OAUTH_CLIENT_ID=desktop.example.test",
		"CLAWBRIDGE_OAUTH_CLIENT_SECRET="+browserSecret,
		"CLAWBRIDGE_OAUTH_DEVICE_CLIENT_ID=device.example.test",
		"CLAWBRIDGE_OAUTH_DEVICE_CLIENT_SECRET="+deviceSecret,
	)
	run := func(args ...string) (string, string, int) {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, p.binary, append([]string{"serve", "--bootstrap-owner", "owner@example.test", "--listen", "127.0.0.1:0"}, args...)...)
		cmd.Dir = p.dir
		cmd.Env = baseEnv
		var stdout, stderr bytes.Buffer
		cmd.Stdout, cmd.Stderr = &stdout, &stderr
		err := cmd.Run()
		var exit *exec.ExitError
		if !errors.As(err, &exit) {
			t.Fatalf("serve %v did not exit with an error: %v", args, err)
		}
		return stdout.String(), stderr.String(), exit.ExitCode()
	}
	origin := []string{"--public-origin", "https://cb.example.test"}
	for _, c := range []struct {
		args []string
		want string
	}{
		{append([]string{"--auth", "google"}, origin...), "clawbridge serve: Google sign-in was removed in v1.0; set CLAWBRIDGE_AUTH=device after every member has an active device (docs/operations/google-sign-in-removal.md#before-you-switch)\n"},
		{append([]string{"--auth", "google,device"}, origin...), "clawbridge serve: Google sign-in was removed in v1.0; set CLAWBRIDGE_AUTH=device after every member has an active device (docs/operations/google-sign-in-removal.md#before-you-switch)\n"},
		{append([]string{"--auth", "Google device"}, origin...), "clawbridge serve: Google sign-in was removed in v1.0; set CLAWBRIDGE_AUTH=device after every member has an active device (docs/operations/google-sign-in-removal.md#before-you-switch)\n"},
		{append([]string{"--auth", "device", "--audience", "desktop.example.test"}, origin...), "clawbridge serve: --audience was for Google sign-in, removed in v1.0 (docs/operations/google-sign-in-removal.md#before-you-switch)\n"},
		{append([]string{"--oauth-client-id", "desktop.example.test"}, origin...), "clawbridge serve: --oauth-client-id was for Google sign-in, removed in v1.0 (docs/operations/google-sign-in-removal.md#before-you-switch)\n"},
	} {
		stdout, stderr, code := run(c.args...)
		if code != 2 || stderr != c.want || stdout != "" {
			t.Fatalf("serve %v: exit %d, stderr %q, stdout %q", c.args, code, stderr, stdout)
		}
	}
	// CLAWBRIDGE_AUTH alone counts too.
	baseEnv = append(baseEnv, "CLAWBRIDGE_AUTH=google device")
	if stdout, stderr, code := run(origin...); code != 2 || stderr != "clawbridge serve: Google sign-in was removed in v1.0; set CLAWBRIDGE_AUTH=device after every member has an active device (docs/operations/google-sign-in-removal.md#before-you-switch)\n" || stdout != "" {
		t.Fatalf("CLAWBRIDGE_AUTH=google device: exit %d, stderr %q", code, stderr)
	}
	baseEnv = baseEnv[:len(baseEnv)-1]
	// Device sign-in starts, ignores the leftover settings with one
	// warning, and reaches the database; local configuration is never read.
	configPath := filepath.Join(p.dir, "must-not-be-read.toml")
	if err := os.WriteFile(configPath, []byte("not = [valid"), 0600); err != nil {
		t.Fatal(err)
	}
	baseEnv = append(baseEnv, "CLAWBRIDGE_CONFIG="+configPath)
	stdout, stderr, code := run(append([]string{"--auth", "device"}, origin...)...)
	output := stdout + stderr
	if code != 1 || !strings.Contains(output, "cannot initialize Postgres") {
		t.Fatalf("device sign-in did not reach the Postgres boundary: exit %d %s", code, output)
	}
	if strings.Count(output, "Google sign-in settings are set but unused since v1.0 (CLAWBRIDGE_AUDIENCE, CLAWBRIDGE_OAUTH_*); remove them from the env file") != 1 {
		t.Fatalf("not one leftover-settings warning: %s", output)
	}
	for _, secret := range []string{browserSecret, deviceSecret, "desktop.example.test", "device.example.test", "postgres://", "not-a-real-password"} {
		if strings.Contains(output, secret) {
			t.Fatalf("serve printed %q: %s", secret, output)
		}
	}
	// v0.3.13's compose file, still on a VM that pulls the v1.0 image,
	// passes every Google flag empty: the server starts.
	stdout, stderr, code = run("--auth", "device", "--public-origin", "https://cb.example.test", "--audience", "", "--oauth-client-id", "", "--oauth-device-client-id", "")
	if code != 1 || !strings.Contains(stdout+stderr, "cannot initialize Postgres") {
		t.Fatalf("the old compose command line did not start: exit %d %s%s", code, stdout, stderr)
	}
	argvSecret := "must-not-enter-process-argv"
	if stdout, stderr, _ = run(append([]string{"--oauth-client-secret", argvSecret}, origin...)...); strings.Contains(stdout+stderr, argvSecret) {
		t.Fatalf("serve echoed a rejected argv credential: %s%s", stdout, stderr)
	}
}

// fileDeviceRoute writes a device key for server into home's file store
// and returns the trusted profile lines that sign with it: a subprocess
// can't share a test's mock keyring, and a device key is the only sign-in
// since v1.0.
func fileDeviceRoute(t *testing.T, home, server string) string {
	t.Helper()
	key, id, err := devicesign.NewKey()
	if err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(server)
	if err != nil {
		t.Fatal(err)
	}
	if err = devicesign.Save(devicesign.Location{Store: devicesign.StoreFile, Host: u.Host, Home: home, DeviceID: id}, key); err != nil {
		t.Fatal(err)
	}
	return "credential_source = 'device'\ndevice_id = " + quoteTOML(id) + "\nkey_store = 'file'\n"
}
