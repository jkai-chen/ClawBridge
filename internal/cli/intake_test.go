package cli

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"clawbridge/internal/linearboard"
	"clawbridge/internal/linearprovider"
)

func TestCLIIntakeRoundTrip(t *testing.T) {
	isolated(t)
	invoke(t, "setup", "--local", "--workspace", "w")
	invoke(t, "project", "create", "p")
	t.Setenv("CLAWBRIDGE_PROJECT", "p")
	invoke(t, "register", "coord", "--instance", "coord-i")
	as := []string{"--session", "coord", "--instance", "coord-i"}
	run := func(args ...string) string {
		t.Helper()
		var out, errOut bytes.Buffer
		if e := Run(context.Background(), append(append([]string{}, as...), args...), &out, &errOut); e != nil {
			t.Fatalf("%v: %v (%s)", args, e, errOut.String())
		}
		return out.String()
	}
	if p := invoke(t, append(as, "intake", "policy")...); p["mode"] != "notify" {
		t.Fatal("default policy:", p)
	}
	p := invoke(t, append(as, "intake", "policy", "set", "--intake-mode", "off", "--signals", "S1", "--signals", "S5", "--roles", "engineer", "--s5-calls-per-hour", "10", "--expected-revision", "0")...)
	if p["mode"] != "off" || p["s5_calls_per_hour"] != float64(10) || len(p["signals"].([]any)) != 2 {
		t.Fatal("policy set:", p)
	}
	if out := run("intake", "trust", "add", "00000000-0000-4000-8000-0000000000f1"); !strings.Contains(out, "00000000-0000-4000-8000-0000000000f1") {
		t.Fatal(out)
	}
	onTerminal(t)
	token := invoke(t, append(as, "intake", "token", "create", "--expires-days", "7")...)
	if !strings.HasPrefix(token["token"].(string), "cbi_") || token["id"] == nil {
		t.Fatal(token)
	}
	if out := run("intake", "token", "list"); strings.Contains(out, token["token"].(string)) {
		t.Fatal("token listed")
	}
	if out := run("intake", "token", "revoke", token["id"].(string)); !strings.Contains(out, "revoked_at") {
		t.Fatal(out)
	}
	if out := run("intake", "list", "--state", "queued"); !strings.Contains(out, `"data":[]`) {
		t.Fatal(out)
	}
	var out, errOut bytes.Buffer
	if e := Run(context.Background(), append(append([]string{}, as...), "intake", "policy", "set", "--intake-mode", "auto"), &out, &errOut); e == nil || !strings.Contains(e.Error(), "not available yet") {
		t.Fatal("auto accepted:", e)
	}
}

// intake trust add|remove takes a Linear name or email too: it's found
// among the board's active members, shown escaped, and changed only with
// --confirm, which says it again. The owner needs no session for it.
func TestCLIIntakeTrustByNameOrEmail(t *testing.T) {
	isolated(t)
	invoke(t, "setup", "--local", "--workspace", "w")
	invoke(t, "project", "create", "p")
	t.Setenv("CLAWBRIDGE_PROJECT", "p")
	const ada = "00000000-0000-4000-8000-0000000000a1"
	const ann = "00000000-0000-4000-8000-0000000000a2"
	const other = "00000000-0000-4000-8000-0000000000a3"
	const mallory = "00000000-0000-4000-8000-0000000000a4"
	const gone = "00000000-0000-4000-8000-0000000000a5"
	oldMembers, oldEnv := trusteeMembers, closesEnv
	t.Cleanup(func() { trusteeMembers, closesEnv = oldMembers, oldEnv })
	closesEnv = func() []string { return []string{"HOME=/x"} }
	lookups := 0
	trusteeMembers = func(context.Context, linearboard.Executor, Config) ([]linearprovider.Member, error) {
		lookups++
		return []linearprovider.Member{
			{ID: ada, Name: "Ada Lovelace", Email: "ada@example.test", Active: true},
			{ID: ann, Name: "Ann", Email: "ann.one@example.test", Active: true},
			{ID: other, Name: "Ann", Email: "ann.two\x1b]8;;x\x07@example.test", Active: true},
			{ID: mallory, Name: "Mal\x1b[2Jlory\u202e", Email: "mal@example.test", Active: true},
			{ID: gone, Name: "Grace", Email: "grace@example.test"},
		}, nil
	}
	run := func(args ...string) (string, string, error) {
		var out, errOut bytes.Buffer
		e := Run(context.Background(), args, &out, &errOut)
		return out.String(), errOut.String(), e
	}
	_, diag, err := run("intake", "trust", "add", "ada lovelace")
	if err == nil || !strings.Contains(err.Error(), "rerun with --confirm to trust them") || !strings.Contains(diag, "That is Ada Lovelace <ada@example.test> ("+ada+")") {
		t.Fatal("no preview:", err, diag)
	}
	if out, _, _ := run("intake", "trust", "list"); !strings.Contains(out, `"data":[]`) {
		t.Fatal("trusted without --confirm:", out)
	}
	if out, diag, err := run("intake", "trust", "add", "ADA@example.test", "--confirm"); err != nil || !strings.Contains(out, ada) || !strings.Contains(diag, "Trusting Ada Lovelace <ada@example.test> ("+ada+").") {
		t.Fatal(out, diag, err)
	}
	// Other people's text is escaped, in the preview and in the
	// ambiguity error alike.
	if _, _, err = run("intake", "trust", "add", "Ann", "--confirm"); err == nil || !strings.Contains(err.Error(), "matches 2 Linear members") || strings.ContainsAny(err.Error(), "\x1b\x07") || !strings.Contains(err.Error(), `ann.two\x1b]8;;x\x07@example.test`) {
		t.Fatal("an ambiguous name:", err)
	}
	if _, diag, err = run("intake", "trust", "add", "mal@example.test"); err == nil || strings.ContainsAny(diag, "\x1b\u202e") || !strings.Contains(diag, `Mal\x1b[2Jlory\u202e`) {
		t.Fatalf("a hostile name: %q %v", diag, err)
	}
	if _, _, err = run("intake", "trust", "add", "Grace", "--confirm"); err == nil || !strings.Contains(err.Error(), "not an active member") {
		t.Fatal("an inactive member:", err)
	}
	if _, _, err = run("intake", "trust", "add", "Nobody", "--confirm"); err == nil || !strings.Contains(err.Error(), "no member") {
		t.Fatal("nobody:", err)
	}
	if _, diag, err = run("intake", "trust", "remove", "Ada Lovelace"); err == nil || !strings.Contains(err.Error(), "stop trusting") || !strings.Contains(diag, ada) {
		t.Fatal(err, diag)
	}
	if out, diag, err := run("intake", "trust", "remove", "Ada Lovelace", "--confirm"); err != nil || strings.Contains(out, ada) || !strings.Contains(diag, "No longer trusting Ada Lovelace") {
		t.Fatal(out, diag, err)
	}
	// Under an agent, no email is printed.
	closesEnv = func() []string { return []string{"CLAUDECODE=1"} }
	if _, diag, err = run("intake", "trust", "add", "Ada Lovelace"); err == nil || strings.Contains(diag, "@example.test") || !strings.Contains(diag, "That is Ada Lovelace ("+ada+")") {
		t.Fatal("an email under an agent:", diag)
	}
	if _, _, err = run("intake", "trust", "add", "Ann"); err == nil || strings.Contains(err.Error(), "@example.test") {
		t.Fatal("an email under an agent:", err)
	}
	// A user ID needs no lookup, and no --confirm.
	before := lookups
	if out, _, err := run("intake", "trust", "add", other); err != nil || !strings.Contains(out, other) || lookups != before {
		t.Fatal(out, err, lookups)
	}
}
