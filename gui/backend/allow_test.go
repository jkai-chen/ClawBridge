package backend

import (
	"encoding/json"
	"errors"
	"testing"
)

func TestCheckCallRefusesANonceField(t *testing.T) {
	for name, params := range map[string]string{
		"top level":      `{"nonce":"abc"}`,
		"other case":     `{"Nonce":"abc"}`,
		"upper case":     `{"limit":1,"NONCE":"abc"}`,
		"nested":         `{"a":{"b":{"nonce":"x"}}}`,
		"inside a list":  `{"a":[1,{"nonce":"x"}]}`,
		"empty value":    `{"nonce":null}`,
		"duplicate keys": `{"nonce":"a","nonce":"b"}`,
	} {
		t.Run(name, func(t *testing.T) {
			_, err := CheckCall("sessions.list", json.RawMessage(params))
			if !errors.Is(err, ErrNonce) {
				t.Fatalf("got %v, want ErrNonce", err)
			}
		})
	}
	// A value that merely mentions the word is fine.
	if _, err := CheckCall("sessions.list", json.RawMessage(`{"query":"nonce"}`)); err != nil {
		t.Fatalf("a value is not a field: %v", err)
	}
}

func TestCheckCallAllowList(t *testing.T) {
	for _, m := range ReadMethods {
		if _, err := CheckCall(m, nil); err != nil {
			t.Errorf("%s must be allowed: %v", m, err)
		}
	}
	for _, m := range []string{
		"", "Hello", "sessions.launchCommand", "linear.do", "messages.send", "messages.reply",
		"messages.markRead", "profile.setProject", "updates.apply", "model.setup", "account.invite",
		"pty.open", "confirm", "bus.request", "sessions.list ", "doctor.checks\n", "../hello",
	} {
		if _, err := CheckCall(m, json.RawMessage(`{}`)); !errors.Is(err, ErrNotAllowed) {
			t.Errorf("%q: got %v, want ErrNotAllowed", m, err)
		}
	}
}

func TestCheckCallParams(t *testing.T) {
	for _, in := range []json.RawMessage{nil, json.RawMessage(""), json.RawMessage("null"), json.RawMessage("  ")} {
		out, err := CheckCall("hello", in)
		if err != nil || string(out) != "{}" {
			t.Errorf("%q -> %q %v", in, out, err)
		}
	}
	for _, in := range []string{`[]`, `"x"`, `3`, `{`, `{"a":}`} {
		if _, err := CheckCall("hello", json.RawMessage(in)); !errors.Is(err, ErrBadParams) {
			t.Errorf("%s: got %v, want ErrBadParams", in, err)
		}
	}
	if out, err := CheckCall("hello", json.RawMessage(`{"limit": 5}`)); err != nil || string(out) != `{"limit": 5}` {
		t.Errorf("an object passes through: %q %v", out, err)
	}
}
