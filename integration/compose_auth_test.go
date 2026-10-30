package integration

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
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

// The sign-in mode compose starts the server in (v1.0): device keys by
// default; google in CLAWBRIDGE_AUTH refuses the start with the swap
// runbook's text; Google sign-in's five old settings, still in an env file,
// never reach the server's argv or environment.
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
		"CLAWBRIDGE_AUDIENCE":                   "browser-client.example.test",
		"CLAWBRIDGE_OAUTH_CLIENT_ID":            "browser-client.example.test",
		"CLAWBRIDGE_OAUTH_DEVICE_CLIENT_ID":     "device-client.example.test",
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
		for _, a := range args {
			if strings.Contains(a, "example.test") && !strings.Contains(a, "clawbridge.example.test") && a != "owner@example.test" || strings.HasPrefix(a, "--audience") || strings.HasPrefix(a, "--oauth") {
				t.Fatalf("a Google sign-in setting reached serve's argv: %q", args)
			}
		}
		for k := range google {
			if _, ok := environment[k]; ok {
				t.Fatalf("%s reached the server's environment", k)
			}
		}
		return server.ParseServe(args, func(k string) string { return environment[k] })
	}
	for name, env := range map[string]map[string]string{
		"default":         base,
		"explicit":        with(base, map[string]string{"CLAWBRIDGE_AUTH": "device"}),
		"leftover google": with(base, google),
	} {
		settings, err := parse(env)
		if err != nil || fmt.Sprint(settings.Auth.Methods) != "[device]" || settings.Auth.PublicOrigin != "https://clawbridge.example.test" {
			t.Fatalf("%s: %+v %v", name, settings.Auth, err)
		}
	}
	for _, auth := range []string{"google", "google,device"} {
		if _, err := parse(with(base, google, map[string]string{"CLAWBRIDGE_AUTH": auth})); err == nil || err.Error() != server.GoogleAuthRefused {
			t.Errorf("CLAWBRIDGE_AUTH=%s: %v", auth, err)
		}
	}
}
