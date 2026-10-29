package server

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	"clawbridge/internal/devicekey"
)

// The code is a secret: without a terminal, and without --force-stdout,
// the command refuses before it creates anything.
func TestBootstrapInviteRefusesANonTerminal(t *testing.T) {
	t.Setenv("CLAWBRIDGE_DATABASE_URL", "postgres://unused.invalid/none")
	t.Setenv("CLAWBRIDGE_BOOTSTRAP_OWNER", "owner@example.test")
	var out bytes.Buffer
	err := RunOperator(context.Background(), []string{"bootstrap-invite", "--workspace", "acme"}, &out)
	if err == nil || !strings.Contains(err.Error(), "secret") || out.Len() != 0 {
		t.Fatal(err, out.String())
	}
	if err = RunOperator(context.Background(), []string{"bootstrap-invite"}, &out); err == nil || !strings.Contains(err.Error(), "--workspace") {
		t.Fatal(err)
	}
}

// --code-stdin refuses stdin without a code, and --force-stdout with it,
// before it opens the database; it never echoes what it read.
// --server names the URL in the printed join command; anything but a bare
// origin is refused before Postgres is opened.
func TestBootstrapInviteServerFlag(t *testing.T) {
	t.Setenv("CLAWBRIDGE_DATABASE_URL", "postgres://unused.invalid/db")
	t.Setenv("CLAWBRIDGE_BOOTSTRAP_OWNER", "owner@example.test")
	for _, bad := range []string{"clawbridge.lan", "ftp://clawbridge.lan", "https://clawbridge.lan/path", "https://u@clawbridge.lan", "https://clawbridge.lan?x=1", "https://"} {
		err := RunOperator(context.Background(), []string{"bootstrap-invite", "--workspace", "acme", "--server", bad, "--force-stdout"}, io.Discard)
		if err == nil || !strings.Contains(err.Error(), "--server") {
			t.Errorf("%q: %v", bad, err)
		}
	}
	for _, good := range []string{"https://clawbridge.lan", "http://127.0.0.1:8443", "https://192.0.2.10/"} {
		if !joinServerURL(good) {
			t.Errorf("%q refused", good)
		}
	}
}

func TestBootstrapInviteCodeStdinRefuses(t *testing.T) {
	t.Setenv("CLAWBRIDGE_DATABASE_URL", "postgres://unused.invalid/none")
	t.Setenv("CLAWBRIDGE_BOOTSTRAP_OWNER", "owner@example.test")
	t.Cleanup(func() { operatorStdin = os.Stdin })
	for stdin, args := range map[string][]string{
		"not-a-code-zzzz\n":         {"--code-stdin"},
		"":                          {"--code-stdin"},
		"CBJ-ABCD-EFGH-JKMN-PQRS\n": {"--code-stdin", "--force-stdout"},
	} {
		operatorStdin = strings.NewReader(stdin)
		var out bytes.Buffer
		err := RunOperator(context.Background(), append([]string{"bootstrap-invite", "--workspace", "acme"}, args...), &out)
		if err == nil || out.Len() != 0 || strings.Contains(err.Error(), "zzzz") || strings.Contains(err.Error(), "unused.invalid") || strings.Contains(err.Error(), "Postgres") {
			t.Fatal(args, err, out.String())
		}
	}
	// R128-L1: --expect-owner refuses another owner before the database;
	// the same email in another case passes on to it.
	operatorStdin = strings.NewReader("CBJ-ABCD-EFGH-JKMN-PQRS\n")
	if err := RunOperator(context.Background(), []string{"bootstrap-invite", "--workspace", "acme", "--code-stdin", "--expect-owner", "someone@example.test"}, io.Discard); err == nil || !strings.Contains(err.Error(), "another email than --expect-owner") {
		t.Fatal(err)
	}
	operatorStdin = strings.NewReader("CBJ-ABCD-EFGH-JKMN-PQRS\n")
	if err := RunOperator(context.Background(), []string{"bootstrap-invite", "--workspace", "acme", "--code-stdin", "--expect-owner", "Owner@Example.test"}, io.Discard); err == nil || !strings.Contains(err.Error(), "Postgres") {
		t.Fatal("a matching owner did not reach the database:", err)
	}
}

// End to end on Postgres, device sign-in only: the operator's code, piped
// with --force-stdout, joins the owner, whose device then signs requests.
func TestPostgresBootstrapInviteJoinsTheOwner(t *testing.T) {
	dsn := os.Getenv("CLAWBRIDGE_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("reviewer supplies disposable Postgres DSN")
	}
	joinFloor = time.Millisecond
	t.Cleanup(func() { joinFloor = 250 * time.Millisecond })
	owner := awUnique("boot") + "@device-test.example"
	ws := awUnique("bootws")
	t.Setenv("CLAWBRIDGE_DATABASE_URL", dsn)
	t.Setenv("CLAWBRIDGE_BOOTSTRAP_OWNER", owner)
	var piped bytes.Buffer
	if err := RunOperator(context.Background(), []string{"bootstrap-invite", "--workspace", ws, "--force-stdout"}, &piped); err != nil {
		t.Fatal(err)
	}
	code := piped.String()
	if !regexp.MustCompile(`^CBJ-[0-9A-Z]{4}-[0-9A-Z]{4}-[0-9A-Z]{4}-[0-9A-Z]{4}\n$`).MatchString(code) {
		t.Fatalf("--force-stdout must print the code alone on one line: %q", code)
	}
	// On a terminal it explains, and tells the owner to keep it secret.
	stdoutIsTerminal = func(io.Writer) bool { return true }
	t.Cleanup(func() { stdoutIsTerminal = func(w io.Writer) bool { return false } })
	var tty bytes.Buffer
	if err := RunOperator(context.Background(), []string{"bootstrap-invite", "--workspace", ws, "--server", "http://127.0.0.1:8443/"}, &tty); err != nil || !strings.Contains(tty.String(), "clawbridge join --server http://127.0.0.1:8443 --email "+owner+"\n") || !strings.Contains(tty.String(), "secret") || strings.Contains(tty.String(), "--code") || !strings.Contains(tty.String(), "asks for the code") {
		t.Fatal(err, tty.String())
	}

	// cloud bootstrap's way: the code it made, on stdin; one JSON line
	// back, without it, to any output.
	mine := devicekey.NewCode()
	operatorStdin = strings.NewReader(strings.ToLower(mine) + "\n")
	t.Cleanup(func() { operatorStdin = os.Stdin })
	stdoutIsTerminal = func(io.Writer) bool { return false }
	var given bytes.Buffer
	if err := RunOperator(context.Background(), []string{"bootstrap-invite", "--workspace", ws, "--code-stdin"}, &given); err != nil {
		t.Fatal(err)
	}
	var invite map[string]any
	if err := json.Unmarshal(given.Bytes(), &invite); err != nil || invite["code"] != nil || invite["email"] != owner || invite["workspace"] != ws || strings.Contains(strings.ToUpper(given.String()), mine[4:8]+mine[9:13]) || strings.Count(given.String(), "\n") != 1 {
		t.Fatal(err, given.String())
	}

	store, err := openServingStore(context.Background(), dsn, owner)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	f := newPGDeviceClient(t, store)
	if w := f.postJoin(f.joinBody(owner, strings.TrimSpace(code), f.now.Unix(), f.priv), "198.51.100.30"); w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	// The given code joins too, as the owner's second device.
	second := newPGDeviceClient(t, store)
	if w := second.postJoin(second.joinBody(owner, mine, second.now.Unix(), second.priv), "198.51.100.31"); w.Code != 200 {
		t.Fatal("the given code:", w.Code, w.Body.String())
	}
	if w := f.do("/v1/execute", `{"operation":"project_create","workspace":"`+ws+`","args":{"slug":"p"}}`); w.Code != 200 {
		t.Fatal("the owner's first device can't act as the owner:", w.Code, w.Body.String())
	}
}
