package cli

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func clearRouteEnv(t *testing.T) {
	t.Helper()
	for _, k := range []string{"CLAWBRIDGE_PROJECT", "CLAWBRIDGE_WORKSPACE", "CLAWBRIDGE_DB", "CLAWBRIDGE_SESSION", "CLAWBRIDGE_INSTANCE", "CLAWBRIDGE_MODE"} {
		t.Setenv(k, "")
	}
}

// setup --local --project saves the project as the default, so a new
// terminal (no CLAWBRIDGE_* variables) works and doctor finds a project.
func TestSetupLocalSavesTheProject(t *testing.T) {
	home := isolated(t)
	clearRouteEnv(t)
	invoke(t, "setup", "--local", "--name", "demo", "--project", "demo")
	// Saved by setup itself, before the project exists.
	raw, err := os.ReadFile(filepath.Join(home, "state", "config.toml"))
	if err != nil || !strings.Contains(string(raw), "project = 'demo'") && !strings.Contains(string(raw), `project = "demo"`) {
		t.Fatal(string(raw), err)
	}
	invoke(t, "project", "create", "demo")
	invoke(t, "register", "eng", "--instance", "eng-i")
	if out := invoke(t, "sessions"); len(out["sessions"].([]any)) != 1 {
		t.Fatal(out)
	}
}

// Without --project, the first project created becomes the default; a
// later one does not replace it.
func TestFirstLocalProjectBecomesTheDefault(t *testing.T) {
	home := isolated(t)
	clearRouteEnv(t)
	invoke(t, "setup", "--local", "--name", "demo")
	invoke(t, "project", "create", "first")
	invoke(t, "project", "create", "second")
	raw, _ := os.ReadFile(filepath.Join(home, "state", "config.toml"))
	if !strings.Contains(string(raw), "first") || strings.Contains(string(raw), "second") {
		t.Fatal(string(raw))
	}
	invoke(t, "register", "eng", "--instance", "eng-i")
}

// A hosted (cloud) profile's default is saved the same way: only when it has
// none, never replacing one, and keeping every other key, its device
// credential included, owner-only.
func TestCloudProfileKeepsItsFirstDefaultProject(t *testing.T) {
	path := filepath.Join(t.TempDir(), "client.toml")
	profile := "mode = 'cloud'\nserver = 'https://clawbridge.example.test'\nprincipal = 'owner@example.test'\ncredential_source = 'device'\ndevice_id = 'dev_example'\nkey_store = 'keyring'\nworkspace = 'w'\n"
	if err := os.WriteFile(path, []byte(profile), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := writeDefaultProject(path, "cloud", "first", false); err != nil {
		t.Fatal(err)
	}
	if err := writeDefaultProject(path, "cloud", "second", false); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(path)
	for _, want := range []string{"project = 'first'", "device_id = 'dev_example'", "key_store = 'keyring'", "credential_source = 'device'", "mode = 'cloud'", "server = 'https://clawbridge.example.test'"} {
		if !strings.Contains(strings.ReplaceAll(string(raw), `"`, "'"), want) {
			t.Fatalf("missing %s in:\n%s", want, raw)
		}
	}
	if strings.Contains(string(raw), "second") {
		t.Fatal("a later project replaced the default:\n" + string(raw))
	}
	if info, err := os.Stat(path); err != nil || (runtime.GOOS != "windows" && info.Mode().Perm() != 0o600) {
		t.Fatal(info.Mode(), err)
	}
	// A default the person chose is kept, too.
	chosen := filepath.Join(t.TempDir(), "chosen.toml")
	if err := os.WriteFile(chosen, []byte(profile+"project = 'mine'\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := writeDefaultProject(chosen, "cloud", "other", false); err != nil {
		t.Fatal(err)
	}
	if raw, _ := os.ReadFile(chosen); !strings.Contains(strings.ReplaceAll(string(raw), `"`, "'"), "project = 'mine'") || strings.Contains(string(raw), "other") {
		t.Fatal(string(raw))
	}
}

// A profile set only by the environment (a CI device's) has no file: nothing
// is written and nothing fails. A file for another mode than the one in effect
// is left alone: a hosted project never lands in a local profile.
func TestDefaultProjectNeedsAFileOfTheSameMode(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "absent.toml")
	if err := writeDefaultProject(missing, "cloud", "p", false); err != nil {
		t.Fatal("a profile without a file:", err)
	}
	if _, err := os.Stat(missing); !os.IsNotExist(err) {
		t.Fatal("a file was created for an environment-only profile:", err)
	}
	local := filepath.Join(t.TempDir(), "local.toml")
	if err := os.WriteFile(local, []byte("workspace = 'demo'\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := writeDefaultProject(local, "cloud", "hosted", false); err != nil {
		t.Fatal(err)
	}
	if raw, _ := os.ReadFile(local); strings.Contains(string(raw), "hosted") {
		t.Fatal("a hosted project landed in a local profile:\n" + string(raw))
	}
	// The same mode-less file is local: a local project is its default.
	if err := writeDefaultProject(local, "local", "mine", false); err != nil {
		t.Fatal(err)
	}
	if raw, _ := os.ReadFile(local); !strings.Contains(strings.ReplaceAll(string(raw), `"`, "'"), "project = 'mine'") {
		t.Fatal(string(raw))
	}
}

// A local profile with no project yet gets local guidance, never the
// hosted join.
func TestBareClawbridgeLocalWithoutProject(t *testing.T) {
	isolated(t)
	wizardHooks(t, "")
	path := filepath.Join(t.TempDir(), "client.toml")
	os.WriteFile(path, []byte("mode='local'\nworkspace='demo'\n"), 0600)
	err := Run(context.Background(), []string{"--client-config", path}, io.Discard, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "project create") || strings.Contains(err.Error(), "join") {
		t.Fatal(err)
	}
}

// On a clean HOME, setup --cloud no longer conflicts with the default local
// mode; it fails, here, only for want of a reachable server.
func TestSetupCloudOnACleanHome(t *testing.T) {
	isolated(t)
	clearRouteEnv(t)
	var out, errs bytes.Buffer
	err := Run(context.Background(), []string{"--server", "https://clawbridge.invalid", "setup", "--cloud", "--name", "w"}, &out, &errs)
	if err == nil || strings.Contains(err.Error(), "conflicts with configured storage mode") || strings.Contains(err.Error(), "mode=cloud") {
		t.Fatal("setup --cloud did not get past the mode check:", err)
	}
	// Its help shows the cloud flags.
	out.Reset()
	errs.Reset()
	_ = Run(context.Background(), []string{"setup", "--help"}, &out, &errs)
	if help := out.String() + errs.String(); !strings.Contains(help, "setup --cloud --name WORKSPACE") || !strings.Contains(help, "--project") {
		t.Fatal(help)
	}
}

// A .clawbridge.toml outside a git checkout's root is warned about.
func TestIgnoredRepoConfigWarns(t *testing.T) {
	isolated(t)
	clearRouteEnv(t)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ".clawbridge.toml"), []byte("project='p'\n"), 0600); err != nil {
		t.Fatal(err)
	}
	wd, _ := os.Getwd()
	t.Cleanup(func() { os.Chdir(wd) })
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	r, w, _ := os.Pipe()
	stderr := os.Stderr
	os.Stderr = w
	_ = Run(context.Background(), []string{"sessions"}, io.Discard, io.Discard)
	os.Stderr = stderr
	w.Close()
	got, _ := io.ReadAll(r)
	if !strings.Contains(string(got), ".clawbridge.toml is ignored") || !strings.Contains(string(got), "not in one") {
		t.Fatal(string(got))
	}
}
