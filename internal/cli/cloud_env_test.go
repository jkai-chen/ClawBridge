package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"runtime"
	"strings"
	"testing"
)

// runEnvInit runs the script with sh on an env file path in a temp dir;
// chown is stubbed, since the test isn't root.
func runEnvInit(t *testing.T, path string, keys ...string) (string, int) {
	t.Helper()
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "chown"), []byte("#!/bin/sh\nexit 0\n"), 0700); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("sh", "-c", envInitScript(append([]string{"POSTGRES_PASSWORD"}, keys...), path))
	cmd.Env = append(os.Environ(), "PATH="+bin+":"+os.Getenv("PATH"))
	out, err := cmd.Output()
	if err == nil {
		return string(out), 0
	}
	return string(out), exitCode(err)
}

// The script makes each missing key once, keeps every existing line, never
// prints a value, and installs the file 0600; an empty line or a path that
// isn't a file stops it with nothing written.
func TestEnvInitScript(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the script runs on the Linux server")
	}
	if strings.Contains(envInitScript([]string{"POSTGRES_PASSWORD", "CLAWBRIDGE_LINEAR_APP_TOKEN_KEY", "CLAWBRIDGE_MODEL_KEY_SEAL_KEY"}, serverEnvFile), "'") {
		t.Fatal("the script has a single quote")
	}
	dir := t.TempDir()
	env := filepath.Join(dir, "clawbridge.env")

	out, code := runEnvInit(t, env)
	raw, _ := os.ReadFile(env)
	if code != 0 || out != "added POSTGRES_PASSWORD\n" || !regexp.MustCompile(`^POSTGRES_PASSWORD=[0-9a-f]{64}\n$`).Match(raw) {
		t.Fatalf("new file: %d %q %q", code, out, raw)
	}
	if fi, _ := os.Stat(env); fi.Mode().Perm() != 0600 {
		t.Fatalf("mode %v", fi.Mode())
	}
	first := string(raw)

	// Again, with a token key named: the password is kept, the key added.
	if err := os.WriteFile(env, []byte(first+"CLAWBRIDGE_DOMAIN=bridge.example.test\n"), 0644); err != nil {
		t.Fatal(err)
	}
	out, code = runEnvInit(t, env, "CLAWBRIDGE_LINEAR_APP_TOKEN_KEY")
	raw, _ = os.ReadFile(env)
	if code != 0 || out != "kept POSTGRES_PASSWORD\nadded CLAWBRIDGE_LINEAR_APP_TOKEN_KEY\n" ||
		!regexp.MustCompile(`^`+regexp.QuoteMeta(first)+`CLAWBRIDGE_DOMAIN=bridge\.example\.test\nCLAWBRIDGE_LINEAR_APP_TOKEN_KEY=[A-Za-z0-9+/]{43}=\n$`).Match(raw) {
		t.Fatalf("second run: %d %q %q", code, out, raw)
	}
	if fi, _ := os.Stat(env); fi.Mode().Perm() != 0600 {
		t.Fatalf("mode after a run: %v", fi.Mode())
	}
	for _, line := range strings.Split(string(raw), "\n") {
		if _, v, ok := strings.Cut(line, "="); ok && strings.Contains(out, v) {
			t.Fatalf("a value was printed: %q", out)
		}
	}

	// A link is refused (exit 4): root's write never follows it, and its
	// target is as it was.
	link := filepath.Join(dir, "link.env")
	if err := os.Symlink(env, link); err != nil {
		t.Fatal(err)
	}
	before := string(raw)
	if out, code = runEnvInit(t, link, "CLAWBRIDGE_MODEL_KEY_SEAL_KEY"); code != 4 || out != "" {
		t.Fatalf("link: %d %q", code, out)
	}
	if raw, _ = os.ReadFile(env); string(raw) != before {
		t.Fatalf("link target changed: %q", raw)
	}
	if fi, _ := os.Lstat(link); fi.Mode()&os.ModeSymlink == 0 {
		t.Fatal("the link was replaced")
	}

	// A last line without a newline stays its own line.
	unended := filepath.Join(dir, "unended.env")
	if err := os.WriteFile(unended, []byte("CLAWBRIDGE_DOMAIN=bridge.example.test"), 0600); err != nil {
		t.Fatal(err)
	}
	if out, code = runEnvInit(t, unended); code != 0 || out != "added POSTGRES_PASSWORD\n" {
		t.Fatalf("unended: %d %q", code, out)
	}
	if raw, _ = os.ReadFile(unended); !regexp.MustCompile(`^CLAWBRIDGE_DOMAIN=bridge\.example\.test\nPOSTGRES_PASSWORD=[0-9a-f]{64}\n$`).Match(raw) {
		t.Fatalf("unended: %q", raw)
	}

	// An empty line stops it; the file is as it was.
	empty := filepath.Join(dir, "empty.env")
	if err := os.WriteFile(empty, []byte("CLAWBRIDGE_DOMAIN=bridge.example.test\nPOSTGRES_PASSWORD=\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if out, code = runEnvInit(t, empty); code != 5 || out != "empty POSTGRES_PASSWORD\n" {
		t.Fatalf("empty: %d %q", code, out)
	}
	if raw, _ = os.ReadFile(empty); string(raw) != "CLAWBRIDGE_DOMAIN=bridge.example.test\nPOSTGRES_PASSWORD=\n" {
		t.Fatalf("empty changed: %q", raw)
	}
	// A generator that comes back short stops it before the file is written.
	short := t.TempDir()
	if err := os.WriteFile(filepath.Join(short, "od"), []byte("#!/bin/sh\necho 0123\n"), 0700); err != nil {
		t.Fatal(err)
	}
	fresh := filepath.Join(dir, "fresh.env")
	cmd := exec.Command("sh", "-c", envInitScript([]string{"POSTGRES_PASSWORD"}, fresh))
	cmd.Env = append(os.Environ(), "PATH="+short+":"+os.Getenv("PATH"))
	if b, err := cmd.Output(); err == nil || len(b) != 0 {
		t.Fatalf("short value: %q %v", b, err)
	}
	if _, err := os.Stat(fresh); !os.IsNotExist(err) {
		t.Fatalf("written with a short value: %v", err)
	}
	if out, code = runEnvInit(t, dir); code != 4 || out != "" {
		t.Fatalf("a directory: %d %q", code, out)
	}
	if left, _ := filepath.Glob(filepath.Join(dir, "*.env.*")); len(left) != 0 {
		t.Fatalf("temp files left: %v", left)
	}
}

func cloudEnvInit(args ...string) (string, error) {
	var out bytes.Buffer
	err := Run(context.Background(), append([]string{"cloud", "env", "init"}, args...), &out, &bytes.Buffer{})
	return out.String(), err
}

// The command runs the fixed script over the user's ssh with the asked
// keys, reads the answer strictly, and prints key names only.
func TestCloudEnvInit(t *testing.T) {
	host := "admin@bridge.example.test"
	runs := stubRemote(t, "kept POSTGRES_PASSWORD\nadded CLAWBRIDGE_LINEAR_APP_TOKEN_KEY\n", nil)
	out, err := cloudEnvInit("CLAWBRIDGE_LINEAR_APP_TOKEN_KEY", "--ssh", host, "POSTGRES_PASSWORD")
	var got struct {
		Source string              `json:"source"`
		Data   map[string][]string `json:"data"`
	}
	if err != nil || json.Unmarshal([]byte(out), &got) != nil || got.Source != "cloud_env" ||
		!reflect.DeepEqual(got.Data, map[string][]string{"added": {"CLAWBRIDGE_LINEAR_APP_TOKEN_KEY"}, "kept": {"POSTGRES_PASSWORD"}}) {
		t.Fatalf("%q %v", out, err)
	}
	script := envInitScript([]string{"POSTGRES_PASSWORD", "CLAWBRIDGE_LINEAR_APP_TOKEN_KEY"}, serverEnvFile)
	if len(*runs) != 1 || !reflect.DeepEqual((*runs)[0].argv, wantSSH(host, `env LC_ALL=C sudo -n sh -c '`+script+`'`)) {
		t.Fatalf("runs %+v", *runs)
	}

	// A key that isn't generated is refused before anything runs.
	runs = stubRemote(t, "", nil)
	if _, err = cloudEnvInit("CLAWBRIDGE_OAUTH_CLIENT_SECRET", "--ssh", host); err == nil || !strings.Contains(err.Error(), "env init makes only these keys") || len(*runs) != 0 {
		t.Fatalf("other key: %v %+v", err, *runs)
	}

	// An answer missing a key, naming another, or with any other line fails.
	for _, answer := range []string{"", "added CLAWBRIDGE_LINEAR_APP_TOKEN_KEY\n", "added POSTGRES_PASSWORD\nadded POSTGRES_PASSWORD\n",
		"added POSTGRES_PASSWORD\nadded CLAWBRIDGE_MODEL_KEY_SEAL_KEY\n", "made POSTGRES_PASSWORD\n", "added POSTGRES_PASSWORD\nPOSTGRES_PASSWORD=0123\n"} {
		stubRemote(t, answer, nil)
		if out, err = cloudEnvInit("--ssh", host); err == nil || out != "" {
			t.Fatalf("answer %q: %q %v", answer, out, err)
		}
	}

	// Two keys asked, one answered.
	stubRemote(t, "added POSTGRES_PASSWORD\n", nil)
	if out, err = cloudEnvInit("CLAWBRIDGE_LINEAR_APP_TOKEN_KEY", "--ssh", host); err == nil || out != "" {
		t.Fatalf("one of two: %q %v", out, err)
	}

	// Refusals name what to do.
	for code, want := range map[int]string{4: "is a link or not a regular file", 5: "has an empty line for POSTGRES_PASSWORD"} {
		stubRemote(t, "empty POSTGRES_PASSWORD\n", exitStatus(code))
		if _, err = cloudEnvInit("--ssh", host); err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("exit %d: %v", code, err)
		}
	}
	for _, c := range []struct {
		err  error
		want string
	}{{errHostKey, "verify the host's key"}, {errSudoPassword, "passwordless sudo"}} {
		stubRemote(t, "", c.err)
		if _, err = cloudEnvInit("--ssh", host); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Fatalf("%v: %v", c.err, err)
		}
	}
	if _, err = cloudEnvInit(); err == nil {
		t.Fatal("no route accepted")
	}
	if _, err = cloudEnvInit("--ssh", "-oProxyCommand=x"); err == nil {
		t.Fatal("a host starting with - accepted")
	}
}
