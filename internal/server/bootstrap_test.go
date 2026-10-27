package server

import (
	"bytes"
	"context"
	"io"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"
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
	if err := RunOperator(context.Background(), []string{"bootstrap-invite", "--workspace", ws}, &tty); err != nil || !strings.Contains(tty.String(), "clawbridge join") || !strings.Contains(tty.String(), "secret") || strings.Contains(tty.String(), "--code") || !strings.Contains(tty.String(), "asks for the code") {
		t.Fatal(err, tty.String())
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
	if w := f.do("/v1/execute", `{"operation":"project_create","workspace":"`+ws+`","args":{"slug":"p"}}`); w.Code != 200 {
		t.Fatal("the owner's first device can't act as the owner:", w.Code, w.Body.String())
	}
}
