package cli

import (
	"bytes"
	"context"
	"strings"
	"testing"
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
