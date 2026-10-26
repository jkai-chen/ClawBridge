package integration

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"clawbridge/internal/server"
)

// renderAPI renders deploy/compose.yaml with env and returns the api
// service's serve flags and environment.
func renderAPI(t *testing.T, env map[string]string) ([]string, map[string]string) {
	t.Helper()
	root := repositoryRoot(t)
	cmd := exec.Command("docker", "compose", "-f", filepath.Join(root, "deploy", "compose.yaml"), "config", "--format", "json")
	cmd.Dir = root
	// Only the listed variables: none of the host's may leak in.
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + os.Getenv("HOME")}
	for k, v := range env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("render compose: %v\n%s", err, output)
	}
	compose, err := decodeRenderedCompose(output)
	if err != nil {
		t.Fatal(err)
	}
	api := compose.Services["api"]
	if len(api.Command) == 0 || api.Command[0] != "serve" {
		t.Fatalf("api command: %q", api.Command)
	}
	return api.Command[1:], api.Environment
}

// The sign-in mode compose starts the server in: the live deployment (an
// audience, no CLAWBRIDGE_AUTH) stays Google only, a fresh install with
// CLAWBRIDGE_AUTH=device needs no Google setting at all, and a Google
// deployment missing an OAuth setting fails at startup rather than falling
// back to device keys.
func TestComposeAuthMode(t *testing.T) {
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker CLI is unavailable")
	}
	base := map[string]string{
		"POSTGRES_PASSWORD":          "review-only-database-password",
		"CLAWBRIDGE_BOOTSTRAP_OWNER": "owner@example.test",
		"CLAWBRIDGE_DOMAIN":          "clawbridge.example.test",
	}
	google := map[string]string{
		"CLAWBRIDGE_AUDIENCE":                   "browser-client.apps.googleusercontent.com,device-client.apps.googleusercontent.com",
		"CLAWBRIDGE_OAUTH_CLIENT_ID":            "browser-client.apps.googleusercontent.com",
		"CLAWBRIDGE_OAUTH_DEVICE_CLIENT_ID":     "device-client.apps.googleusercontent.com",
		"CLAWBRIDGE_OAUTH_CLIENT_SECRET":        "synthetic-compose-browser-app-credential",
		"CLAWBRIDGE_OAUTH_DEVICE_CLIENT_SECRET": "synthetic-compose-device-app-credential",
	}
	with := func(maps ...map[string]string) map[string]string {
		out := map[string]string{}
		for _, m := range maps {
			for k, v := range m {
				out[k] = v
			}
		}
		return out
	}
	parse := func(env map[string]string) (server.ServeSettings, error) {
		args, environment := renderAPI(t, env)
		return server.ParseServe(args, func(k string) string { return environment[k] })
	}

	// Today's deployment: unchanged, Google only.
	live, err := parse(with(base, google))
	if err != nil || fmt.Sprint(live.Auth.Methods) != "[google]" {
		t.Fatalf("the live deployment's mode changed: %v %v", live.Auth.Methods, err)
	}
	// A fresh device-only install renders with no Google setting.
	fresh, err := parse(with(base, map[string]string{"CLAWBRIDGE_AUTH": "device"}))
	if err != nil || fmt.Sprint(fresh.Auth.Methods) != "[device]" || fresh.Auth.PublicOrigin != "https://clawbridge.example.test" {
		t.Fatalf("device-only install: %+v %v", fresh.Auth, err)
	}
	both, err := parse(with(base, google, map[string]string{"CLAWBRIDGE_AUTH": "google,device"}))
	if err != nil || fmt.Sprint(both.Auth.Methods) != "[google device]" {
		t.Fatalf("google,device: %+v %v", both.Auth, err)
	}
	// A Google deployment missing any one setting refuses to start.
	for name := range google {
		broken := with(base, google)
		delete(broken, name)
		if _, err = parse(broken); err == nil {
			t.Errorf("Google sign-in started without %s", name)
		}
	}
}
