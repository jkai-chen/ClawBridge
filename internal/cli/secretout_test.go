package cli

import (
	"crypto/ed25519"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"clawbridge/internal/devicekey"
	"clawbridge/internal/devicesign"
)

// shownData is a command's JSON answer's data.
func shownData(t *testing.T, out string) map[string]any {
	t.Helper()
	var shown struct {
		Data map[string]any `json:"data"`
	}
	if err := json.Unmarshal([]byte(out), &shown); err != nil {
		t.Fatal("the answer isn't JSON:", err)
	}
	return shown.Data
}

// onTerminal stands in a terminal for stdout.
func onTerminal(t *testing.T) {
	t.Helper()
	old := stdoutIsTerminal
	stdoutIsTerminal = func(io.Writer) bool { return true }
	t.Cleanup(func() { stdoutIsTerminal = old })
}

// G9 F2: an invite code or a CI device's key, asked for with stdout not a
// terminal (an agent's or a pipe's), is refused before anything is
// created; --out writes it to a new 0600 file, never over an existing one,
// and stdout carries no secret.
func TestAccountSecretsGoToATerminalOrANewFile(t *testing.T) {
	googleProfile(t)
	for _, args := range [][]string{
		{"invite", "--email", "bob@example.com"},
		{"device", "add"},
		{"device", "add", "--ci", "deploy", "--grant", "api=member", "--expires", "30d"},
	} {
		r := &accountRecorder{}
		withAccountRecorder(t, r)
		if _, err := run(args...); err == nil || !strings.Contains(err.Error(), "--out FILE") || len(r.requests) != 0 {
			t.Fatal("a secret went to a non-terminal:", args, err, r.requests)
		}
	}
	dir := t.TempDir()

	// An invite code.
	code := filepath.Join(dir, "code")
	r := &accountRecorder{data: map[string]any{"code": "CBJ-AAAA-BBBB-CCCC-DDDD", "invite_id": "inv_1"}}
	withAccountRecorder(t, r)
	out, err := run("invite", "--email", "bob@example.com", "--out", code)
	// Paths are compared decoded (JSON escapes Windows' backslashes), and
	// no failure message prints a secret.
	if err != nil || strings.Contains(out, "CBJ-") || shownData(t, out)["code_file"] != code {
		t.Fatal("the code reached stdout, or the file isn't named:", err)
	}
	if got, e := os.ReadFile(code); e != nil || string(got) != "CBJ-AAAA-BBBB-CCCC-DDDD\n" {
		t.Fatal("the file doesn't hold the code:", e)
	}
	// Windows has no POSIX modes.
	if st, _ := os.Stat(code); runtime.GOOS != "windows" && st.Mode().Perm() != 0o600 {
		t.Fatal("mode", st.Mode())
	}
	// Never over an existing file, and nothing is created then.
	r = &accountRecorder{data: map[string]any{"code": "CBJ-EEEE-FFFF-GGGG-HHHH", "invite_id": "inv_2"}}
	withAccountRecorder(t, r)
	if _, err = run("invite", "--email", "bob@example.com", "--out", code); err == nil || !strings.Contains(err.Error(), "must not exist") || len(r.requests) != 0 {
		t.Fatal("an existing file was overwritten:", err, r.requests)
	}

	// A CI device's key.
	keyFile := filepath.Join(dir, "ci.env")
	r = &accountRecorder{data: map[string]any{"principal": "ci:deploy@acme"}}
	withAccountRecorder(t, r)
	out, err = run("device", "add", "--ci", "deploy", "--grant", "api=member", "--expires", "30d", "--out", keyFile)
	if err != nil || strings.Contains(out, devicesign.EnvKey+`":`) || shownData(t, out)["key_file"] != keyFile {
		t.Fatal("the key reached stdout, or the file isn't named:", err)
	}
	raw, _ := os.ReadFile(keyFile)
	line := strings.TrimSpace(string(raw))
	key, err := devicesign.ParseKey(strings.TrimPrefix(line, devicesign.EnvKey+"="))
	if err != nil || !strings.HasPrefix(line, devicesign.EnvKey+"=") || devicekey.EncodePublicKey(key.Public().(ed25519.PublicKey)) != r.requests[0].Args["public_key"] {
		t.Fatal("the file doesn't hold the registered key:", err)
	}
}

// An intake token likewise: refused to a non-terminal before it exists,
// or written to a new file.
func TestIntakeTokenGoesToATerminalOrANewFile(t *testing.T) {
	isolated(t)
	invoke(t, "setup", "--local", "--workspace", "w")
	invoke(t, "project", "create", "p")
	t.Setenv("CLAWBRIDGE_PROJECT", "p")
	invoke(t, "register", "coord", "--instance", "coord-i")
	as := []string{"--session", "coord", "--instance", "coord-i"}
	if _, err := run(append(as, "intake", "token", "create")...); err == nil || !strings.Contains(err.Error(), "--out FILE") {
		t.Fatal("a token went to a non-terminal:", err)
	}
	if list, _ := run(append(as, "intake", "token", "list")...); !strings.Contains(list, `"data":[]`) {
		t.Fatal("a refused run made a token:", list)
	}
	file := filepath.Join(t.TempDir(), "token")
	out, err := run(append(as, "intake", "token", "create", "--out", file)...)
	got, _ := os.ReadFile(file)
	token := strings.TrimSpace(string(got))
	if err != nil || !strings.HasPrefix(token, "cbi_") || strings.Contains(out, token) || shownData(t, out)["token_file"] != file {
		t.Fatal("the token reached stdout, or the file isn't named or holds no token:", err)
	}
}
