package cli

import (
	"bytes"
	"strings"
	"testing"

	"clawbridge/internal/bus"
	"clawbridge/internal/clirender"
	"clawbridge/internal/devicesign"
)

// M3: each command that prints a secret has its own view, registered as a
// secret's, so the generic layout never gets it; the secret is printed once,
// alone and whole; written to a file, it isn't printed at all.
func TestSecretViews(t *testing.T) {
	for _, op := range []string{"invite_create", "device_ci_create", "intake_token_create"} {
		if !clirender.IsSecret(op) {
			t.Fatalf("%s has no secret view", op)
		}
	}
	link := "https://bridge.example.test/join#" + strings.Repeat("k", 150)
	for _, c := range []struct {
		op      string
		data    map[string]any
		secrets []string
		absent  []string
	}{
		{"invite_create", map[string]any{"invite_id": "inv_1", "email": "ada@example.test", "purpose": "user", "code": "CBJ-4K7P-WQ9D", "link": link,
			"grants": map[string]any{"p": "member"}, "note": "Give the link privately."}, []string{link}, []string{"Invite code"}},
		// Without a link (a code-only invite), the code is the secret shown.
		{"invite_create", map[string]any{"invite_id": "inv_3", "code": "CBJ-4K7P-WQ9D"}, []string{"CBJ-4K7P-WQ9D"}, nil},
		{"invite_create", map[string]any{"invite_id": "inv_2", "code_file": "/tmp/code", "note": "Wrote the code to /tmp/code (0600)."}, nil, []string{"Invite code"}},
		{"device_ci_create", map[string]any{"device_id": "dev_ci", devicesign.EnvKey: "c2VjcmV0LWtleS1ieXRlcw", "note": "Store it now."}, []string{"c2VjcmV0LWtleS1ieXRlcw"}, nil},
		{"intake_token_create", map[string]any{"id": "it_1", "token": "cbi_tokentokentoken", "expires_at": "2026-11-01"}, []string{"cbi_tokentokentoken"}, nil},
		{"intake_token_create", map[string]any{"id": "it_2", "token_file": "/tmp/t", "note": "Wrote the token."}, nil, []string{"Intake token (shown once)"}},
	} {
		var out bytes.Buffer
		if err := clirender.Write(&out, clirender.Human, c.op, bus.Result{Source: "server", Data: c.data}); err != nil {
			t.Fatal(err)
		}
		text := out.String()
		if strings.HasPrefix(text, "{") {
			t.Fatalf("%s fell back to JSON:\n%s", c.op, text)
		}
		for _, s := range c.secrets {
			if strings.Count(text, s) != 1 || !strings.Contains(text, "\n  "+s+"\n") {
				t.Fatalf("%s: the secret is not printed once, whole, on its own line:\n%s", c.op, text)
			}
		}
		for _, s := range c.absent {
			if strings.Contains(text, s) {
				t.Fatalf("%s: %q printed:\n%s", c.op, s, text)
			}
		}
		if note, _ := c.data["note"].(string); note != "" && !strings.Contains(text, note) {
			t.Fatalf("%s: no note:\n%s", c.op, text)
		}
	}
	// A secret from outside is escaped like any data; it still prints once.
	var out bytes.Buffer
	_ = clirender.Write(&out, clirender.Human, "invite_create", bus.Result{Data: map[string]any{"code": "CBJ\x1b[2J"}})
	if strings.Contains(out.String(), "\x1b") || strings.Count(out.String(), `CBJ\x1b[2J`) != 1 {
		t.Fatal(out.String())
	}
}
