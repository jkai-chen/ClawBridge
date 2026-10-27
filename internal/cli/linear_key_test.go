package cli

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"clawbridge/internal/linearconn"
	"clawbridge/internal/linearkey"
	"clawbridge/internal/linearprovider"
)

// A synthetic key: the shape of a Linear personal API key, nobody's.
const cliTestKey = "lin_api_" + "Synthetic0123456789abcdefghijklmnopqrstuv"

func TestLinearKeyEntry(t *testing.T) {
	for args, want := range map[string]bool{
		"linear key set --profile p":           true,
		"--workspace w linear key status":      true,
		"linear view list --team x":            true,
		"linear status --profile p":            false,
		"mcp":                                  false,
		"runner launch":                        false,
		"serve":                                false,
		"notify watch":                         false,
		"doctor":                               false,
		"linear":                               false,
		"--workspace":                          false,
		"linear keys":                          false,
		"--session linear linear key set":      true,
		"-- linear key set":                    false,
		"--linear-profile p linear key remove": true,
	} {
		if got := linearKeyEntry(strings.Fields(args)); got != want {
			t.Fatal(args, got)
		}
	}
}

// personEnv is this process's environment without any agent marker, as a
// person's terminal has it.
func personEnv() []string {
	out := []string{}
	for _, kv := range os.Environ() {
		if linearkey.AgentMarker([]string{kv}) == "" {
			out = append(out, kv)
		}
	}
	return out
}

// child runs this test's body in a fresh process, where nothing has marked
// the process key-forbidden yet.
func child(t *testing.T, name string, env ...string) {
	t.Helper()
	if os.Getenv("CLI_KEY_CHILD") != "" {
		return
	}
	cmd := exec.Command(os.Args[0], "-test.run=^"+name+"$", "-test.count=1", "-test.v")
	cmd.Env = append(append(personEnv(), "CLI_KEY_CHILD=1"), env...)
	out, err := cmd.CombinedOutput()
	if err != nil || !strings.Contains(string(out), "--- PASS: "+name) {
		t.Fatal(string(out), err)
	}
	t.SkipNow()
}

// K1: every entry but linear key and linear view marks the process
// key-forbidden before anything else, the MCP server's first of all.
func TestKeyForbiddenEntries(t *testing.T) {
	if os.Getenv("CLI_KEY_ENTRY") == "" {
		for _, entry := range []string{"mcp", "runner", "serve", "notify", "doctor", "linear status", "linear key status"} {
			cmd := exec.Command(os.Args[0], "-test.run=^TestKeyForbiddenEntries$", "-test.count=1", "-test.v")
			cmd.Env = append(personEnv(), "CLI_KEY_ENTRY="+entry, "HOME="+t.TempDir(), "CLAWBRIDGE_HOME="+t.TempDir())
			if out, err := cmd.CombinedOutput(); err != nil || !strings.Contains(string(out), "--- PASS") {
				t.Fatal(entry, string(out), err)
			}
		}
		return
	}
	entry := os.Getenv("CLI_KEY_ENTRY")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_ = Run(ctx, strings.Fields(entry), io.Discard, io.Discard)
	want := entry != "linear key status"
	if linearkey.Forbidden() != want {
		t.Fatal(entry, "forbidden:", linearkey.Forbidden())
	}
	if want {
		_, err := linearkey.Store{Dir: t.TempDir()}.Open("profile-1")
		if !errors.Is(err, linearkey.ErrForbidden) {
			t.Fatal(entry, err)
		}
	}
}

// fakeKeyLinear answers the key's viewer read with user, and fakes the
// profile's own user as profileUser.
func fakeKeyLinear(t *testing.T, user, profileUser string) *int {
	t.Helper()
	sent := 0
	old := http.DefaultTransport
	http.DefaultTransport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		sent++
		if r.URL.String() != linearkey.Endpoint || r.Header.Get("Authorization") != cliTestKey {
			return nil, errors.New("unexpected request")
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"data":{"viewer":{"id":"` + user + `"}}}`)), Header: http.Header{}}, nil
	})
	oldViewer := keyViewer
	keyViewer = func(context.Context, *linearconn.Manager, string) (linearprovider.Viewer, error) {
		return linearprovider.Viewer{ID: profileUser}, nil
	}
	t.Cleanup(func() { http.DefaultTransport, keyViewer = old, oldViewer })
	return &sent
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// set reads a piped key, checks it acts as the profile's own user, and
// saves it; status checks it again; remove and logout's removal clear it.
func TestLinearKeySetStatusRemove(t *testing.T) {
	child(t, "TestLinearKeySetStatusRemove")
	dir := t.TempDir()
	typing(t, false, "")
	keyStdin = strings.NewReader(cliTestKey + "\n")
	sent := fakeKeyLinear(t, "user-1", "user-1")
	out, err := linearKey(context.Background(), Config{KeyStore: "file"}, nil, dir, "set", "profile-1", &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "confirmed at a terminal") {
		t.Fatal("a file store without a terminal:", err)
	}
	// The OS store is never touched by tests: save to a file at a
	// "terminal".
	stderr := &bytes.Buffer{}
	typing(t, true, "y\n")
	keyStdin = closesStdin
	oldHidden := readHidden
	readHidden = func() ([]byte, error) { return []byte(cliTestKey), nil }
	defer func() { readHidden = oldHidden }()
	out, err = linearKey(context.Background(), Config{KeyStore: "file"}, nil, dir, "set", "profile-1", stderr)
	if m, _ := out.(map[string]any); err != nil || m["saved"] != true || m["store"] != "file" || !strings.Contains(stderr.String(), "plain 0600 file") {
		t.Fatal(out, err, stderr.String())
	}
	if strings.Contains(stderr.String(), cliTestKey) {
		t.Fatal("the key was echoed")
	}
	status, err := linearKey(context.Background(), Config{}, nil, dir, "status", "profile-1", stderr)
	if m, _ := status.(map[string]any); err != nil || m["same_person"] != true || m["read"] != "works" {
		t.Fatal(status, err)
	}
	if *sent < 2 {
		t.Fatal("the key wasn't checked")
	}
	if _, err = linearKey(context.Background(), Config{}, nil, dir, "remove", "profile-1", stderr); err != nil {
		t.Fatal(err)
	}
	if status, _ = linearKey(context.Background(), Config{}, nil, dir, "status", "profile-1", stderr); status.(map[string]any)["saved"] != false {
		t.Fatal(status)
	}
	if entries, _ := os.ReadDir(filepath.Join(dir, "keys")); len(entries) != 0 {
		t.Fatal("files stayed:", entries)
	}
}

// Someone else's key is refused, and nothing is saved.
func TestLinearKeyMustBeTheProfilesOwn(t *testing.T) {
	child(t, "TestLinearKeyMustBeTheProfilesOwn")
	dir := t.TempDir()
	typing(t, true, "y\n")
	keyStdin = closesStdin
	oldHidden := readHidden
	readHidden = func() ([]byte, error) { return []byte(cliTestKey), nil }
	defer func() { readHidden = oldHidden }()
	fakeKeyLinear(t, "someone-else", "user-1")
	if _, err := linearKey(context.Background(), Config{KeyStore: "file"}, nil, dir, "set", "profile-1", &bytes.Buffer{}); err == nil || !strings.Contains(err.Error(), "another Linear user") {
		t.Fatal(err)
	}
	if _, ok := linearKeys(dir).Recorded("profile-1"); ok {
		t.Fatal("someone else's key was saved")
	}
}

// K2: under an agent's environment linear key set refuses before asking
// for the key.
func TestLinearKeyRefusesAgents(t *testing.T) {
	child(t, "TestLinearKeyRefusesAgents", "CLAUDECODE=1")
	typing(t, true, "y\n")
	asked := false
	oldHidden := readHidden
	readHidden = func() ([]byte, error) { asked = true; return []byte(cliTestKey), nil }
	defer func() { readHidden = oldHidden }()
	fakeKeyLinear(t, "user-1", "user-1")
	keyStdin = closesStdin
	_, err := linearKey(context.Background(), Config{KeyStore: "file"}, nil, t.TempDir(), "set", "profile-1", &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "CLAUDECODE") {
		t.Fatal(err)
	}
	if asked {
		t.Fatal("the key was asked for under an agent")
	}
}

// L2: a file store is said and confirmed first; anything but yes saves
// nothing, and the key isn't asked for.
func TestLinearKeyFileStoreNeedsAYes(t *testing.T) {
	child(t, "TestLinearKeyFileStoreNeedsAYes")
	dir := t.TempDir()
	typing(t, true, "n\n")
	keyStdin = closesStdin
	asked := false
	oldHidden := readHidden
	readHidden = func() ([]byte, error) { asked = true; return []byte(cliTestKey), nil }
	defer func() { readHidden = oldHidden }()
	fakeKeyLinear(t, "user-1", "user-1")
	if _, err := linearKey(context.Background(), Config{KeyStore: "file"}, nil, dir, "set", "profile-1", &bytes.Buffer{}); err == nil || !strings.Contains(err.Error(), "nothing was saved") || asked {
		t.Fatal(err, asked)
	}
	if _, ok := linearKeys(dir).Recorded("profile-1"); ok {
		t.Fatal("saved without a yes")
	}
}
