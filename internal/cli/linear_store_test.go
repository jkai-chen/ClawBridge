package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"clawbridge/internal/linearconn"
	"clawbridge/internal/linearprovider"
)

const storeTestToken = "lin_api_syntheticstoretoken00000000000000000"

// storeManager stubs the Linear connection's manager: secrets is the
// keyring, keyring says whether one is there, and Linear itself is never
// reached.
func storeManager(t *testing.T, secrets *statusSecrets, keyring *error) {
	t.Helper()
	previousManager, previousOpen := newLinearManager, openLinearProvider
	t.Cleanup(func() { newLinearManager, openLinearProvider = previousManager, previousOpen })
	newLinearManager = func(directory string) *linearconn.Manager {
		return linearconn.New(linearconn.Config{Directory: directory, Store: secrets,
			HTTPClient:   &http.Client{Transport: failingTransport{}},
			KeyringCheck: func(context.Context) error { return *keyring },
			Verify: func(context.Context, *http.Client, linearconn.Access) (linearconn.Identity, error) {
				return linearconn.Identity{}, nil
			}})
	}
	openLinearProvider = func(context.Context, *http.Client, bool) (*linearprovider.Provider, error) {
		return nil, &linearprovider.Error{Kind: "unavailable"}
	}
}

type failingTransport struct{}

func (failingTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, errors.New("no network in tests")
}

func TestLinearConnectKeyStoreFlag(t *testing.T) {
	isolated(t)
	secrets := &statusSecrets{values: map[string]string{}}
	var keyring error = errors.New("Linux Secret Service is unavailable")
	storeManager(t, secrets, &keyring)
	directory, err := linearDirectory()
	if err != nil {
		t.Fatal(err)
	}
	connections := filepath.Join(directory, "connections")
	run := func(args ...string) (string, error) {
		var out, diag bytes.Buffer
		err := Run(context.Background(), args, &out, &diag)
		if strings.Contains(out.String()+diag.String(), storeTestToken) {
			t.Fatal("a secret was printed")
		}
		return diag.String(), err
	}
	if _, err := run("linear", "connect", "--name", "w", "--access", "read", "--no-browser", "--key-store", "vault"); err == nil || !strings.Contains(err.Error(), "--key-store is keyring or file") {
		t.Fatal(err)
	}
	// No keyring and no --key-store file: the error names the way out,
	// before the browser, and nothing falls back to a file.
	for _, args := range [][]string{{"linear", "connect", "--name", "w", "--access", "read", "--no-browser"}, {"linear", "connect", "--name", "w", "--access", "read", "--no-browser", "--key-store", "keyring"}} {
		diag, err := run(args...)
		if err == nil || !strings.Contains(err.Error(), "--key-store file") || strings.Contains(diag, "https://") {
			t.Fatal(args, err, diag)
		}
		if _, e := os.Stat(connections); !errors.Is(e, os.ErrNotExist) {
			t.Fatal("a file store was made without --key-store file", e)
		}
	}
	// The explicit choice, before or after the command (Run takes it as a
	// global flag), or from the guided setup's own call: warned of, and
	// accepted without a terminal, as the device key's --key-store file is.
	for _, args := range [][]string{{"linear", "connect", "--name", "w", "--access", "read", "--no-browser", "--key-store", "file"}, {"--key-store", "file", "linear", "connect", "--name", "w", "--access", "read", "--no-browser"}} {
		diag, err := run(args...)
		if !strings.Contains(diag, "warning: "+linearconn.FileWarning+".") {
			t.Fatal(args, "no warning:", diag)
		}
		// Linear itself is unreachable here; the store was not the problem.
		if err == nil || strings.Contains(err.Error(), "key store") {
			t.Fatal(args, err)
		}
	}
	var diag bytes.Buffer
	err = runLinear(context.Background(), Config{}, []string{"connect", "--name", "w", "--access", "read", "--no-browser", "--key-store", "file"}, &bytes.Buffer{}, &diag)
	if err == nil || strings.Contains(err.Error(), "key store") || !strings.Contains(diag.String(), linearconn.FileWarning) {
		t.Fatal("the command's own --key-store was not taken", err, diag.String())
	}
	if entries, _ := os.ReadDir(connections); len(entries) != 0 || len(secrets.values) != 0 {
		t.Fatal("a failed connection left a credential", entries, secrets.values)
	}
}

func TestLinearStatusAndDoctorShowTheStore(t *testing.T) {
	isolated(t)
	secrets := &statusSecrets{values: map[string]string{}}
	var keyring error
	storeManager(t, secrets, &keyring)
	directory, err := linearDirectory()
	if err != nil {
		t.Fatal(err)
	}
	manager := newLinearManager(directory)
	file, err := manager.ImportTo(context.Background(), "file", linearconn.Read, linearconn.StoreFile, strings.NewReader(storeTestToken))
	if err != nil {
		t.Fatal(err)
	}
	kept, err := manager.Import(context.Background(), "keyring", linearconn.Read, strings.NewReader(storeTestToken))
	if err != nil {
		t.Fatal(err)
	}
	if len(secrets.values) != 1 {
		t.Fatal("the file connection went to the keyring", len(secrets.values))
	}
	run := func(args ...string) map[string]any {
		t.Helper()
		var out, diag bytes.Buffer
		_ = Run(context.Background(), args, &out, &diag)
		if strings.Contains(out.String()+diag.String(), storeTestToken) {
			t.Fatal("a secret was printed", out.String())
		}
		var value map[string]any
		if e := json.Unmarshal(out.Bytes(), &value); e != nil {
			t.Fatal(args, e, out.String(), diag.String())
		}
		return value
	}
	for id, store := range map[string]string{file.ID: "file", kept.ID: "keyring"} {
		data, _ := run("linear", "status", "--profile", id)["data"].(map[string]any)
		if data["key_store"] != store {
			t.Fatalf("status of a %s connection: %v", store, data)
		}
		check := doctorCheck(t, run("--linear-profile", id, "doctor"), "linear.connection")
		if check["status"] != "pass" || !strings.Contains(fmt.Sprint(check["message"]), "its "+store+" store") {
			t.Fatalf("doctor for a %s connection: %v", store, check)
		}
	}
	// A file opened to others is refused, and doctor says why.
	path := filepath.Join(directory, "connections", file.ID+".json")
	if runtime.GOOS != "windows" {
		if err := os.Chmod(path, 0o644); err != nil {
			t.Fatal(err)
		}
		check := doctorCheck(t, run("--linear-profile", file.ID, "doctor"), "linear.connection")
		if check["status"] != "warn" || !strings.Contains(fmt.Sprint(check["message"]), "(file) can't be used") || !strings.Contains(fmt.Sprint(check["message"]), "mode 0644") {
			t.Fatalf("doctor accepted a wide file: %v", check)
		}
		_ = os.Chmod(path, 0o600)
	}
	// A record that can't be read: doctor says the store is unknown, not
	// the keyring.
	record := strings.TrimSuffix(path, ".json") + ".store"
	if err := os.WriteFile(record, []byte("vault\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	check := doctorCheck(t, run("--linear-profile", file.ID, "doctor"), "linear.connection")
	if check["status"] != "warn" || !strings.Contains(fmt.Sprint(check["message"]), "(unknown) can't be used") {
		t.Fatalf("doctor guessed an unreadable record's store: %v", check)
	}
	if err := os.WriteFile(record, []byte("file\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// Logout removes the file and its record.
	run("linear", "logout", "--profile", file.ID)
	for _, kept := range []string{path, strings.TrimSuffix(path, ".json") + ".store"} {
		if _, err := os.Stat(kept); !errors.Is(err, os.ErrNotExist) {
			t.Fatal("logout kept", kept, err)
		}
	}
}

func doctorCheck(t *testing.T, report map[string]any, id string) map[string]any {
	t.Helper()
	data, _ := report["data"].(map[string]any)
	if data == nil {
		data = report
	}
	checks, _ := data["checks"].([]any)
	for _, c := range checks {
		if m, _ := c.(map[string]any); m["id"] == id {
			return m
		}
	}
	t.Fatalf("doctor has no %s check: %v", id, report)
	return nil
}
