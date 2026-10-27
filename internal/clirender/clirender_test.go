package clirender

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"
)

// §2's rule, signal by signal, the first that applies winning: the flag,
// CLAWBRIDGE_OUTPUT (a bad value an error, L5), an agent's shell (M1), then
// the terminal (a buffer is not one).
func TestChoose(t *testing.T) {
	for _, c := range []struct {
		name string
		flag Flag
		env  []string
		want Mode
		err  error
	}{
		{"pipe", FlagUnset, nil, JSON, nil},
		{"--json", FlagTrue, []string{"CLAWBRIDGE_OUTPUT=human"}, JSON, nil},
		{"--json=false on a pipe", FlagFalse, []string{"CLAUDECODE=1"}, Human, nil},
		{"env human", FlagUnset, []string{"CLAWBRIDGE_OUTPUT=human"}, Human, nil},
		{"env human beats an agent shell", FlagUnset, []string{"CLAUDECODE=1", "CLAWBRIDGE_OUTPUT=human"}, Human, nil},
		{"env json", FlagUnset, []string{"CLAWBRIDGE_OUTPUT=json"}, JSON, nil},
		{"env typo", FlagUnset, []string{"CLAWBRIDGE_OUTPUT=jsno"}, JSON, ErrOutputEnv},
		{"env empty is unset", FlagUnset, []string{"CLAWBRIDGE_OUTPUT="}, JSON, nil},
		{"claude code", FlagUnset, []string{"CLAUDECODE=1"}, JSON, nil},
		{"codex", FlagUnset, []string{"CODEX_SANDBOX=seatbelt"}, JSON, nil},
	} {
		got, err := Choose(c.flag, c.env, &bytes.Buffer{})
		if got != c.want || !errors.Is(err, c.err) {
			t.Fatalf("%s: %v %v", c.name, got, err)
		}
	}
	// At a terminal: a person gets the layout, an agent's shell JSON.
	old := isTerminal
	isTerminal = func(io.Writer) bool { return true }
	t.Cleanup(func() { isTerminal = old })
	for env, want := range map[string]Mode{"": Human, "CLAUDECODE=1": JSON, "CODEX_SANDBOX=seatbelt": JSON, "CLAUDECODE=1|CLAWBRIDGE_OUTPUT=human": Human, "TERM=xterm": Human} {
		var environ []string
		if env != "" {
			environ = strings.Split(env, "|")
		}
		if got, err := Choose(FlagUnset, environ, &bytes.Buffer{}); got != want || err != nil {
			t.Fatalf("at a terminal with %q: %v %v", env, got, err)
		}
	}
	if !agentShell(map[string]string{"CODEX_HOME": "/x"}) || agentShell(map[string]string{"CODEX_HOME": ""}) || agentShell(map[string]string{"HOME": "/x"}) {
		t.Fatal("agent markers")
	}
}

// The R1 floor: control characters, ESC and bidi controls are escaped;
// newline and tab show as ↵ and ⇥.
func TestSafe(t *testing.T) {
	for in, want := range map[string]string{
		"plain":          "plain",
		"a\x1b[2Jb":      `a\x1b[2Jb`,
		"bell\a":         `bell\x07`,
		"cr\rlf\n":       `cr\x0dlf↵`,
		"tab\there":      "tab⇥here",
		"rlo\u202eevil":  `rlo\u202eevil`,
		"isolate\u2066x": `isolate\u2066x`,
		"c1\u009bx":      `c1\x9bx`,
		"émoji 🙂 ok":     "émoji 🙂 ok",
	} {
		if got := Safe(in); got != want {
			t.Fatalf("%q: %q, want %q", in, got, want)
		}
	}
}

// JSON mode is exactly json.NewEncoder(w).Encode(v), byte for byte, for
// every kind of value the CLI encodes.
func TestWriteJSONIsEncode(t *testing.T) {
	type inner struct {
		A string `json:"a,omitempty"`
		B int    `json:"b"`
	}
	type result struct {
		Source string `json:"source"`
		Data   any    `json:"data"`
		Note   string `json:"note,omitempty"`
	}
	for _, v := range []any{
		nil, true, 3, "x <tag> & \u2028 é \x1b", json.Number("12.50"),
		map[string]any{"z": 1, "a": []any{1, "two", nil}, "html": "<a href=x>"},
		result{Source: "server", Data: []inner{{A: "a"}, {B: 2}}},
		&result{Source: "client"},
		[]map[string]string{{"k": "v"}},
	} {
		var want, got bytes.Buffer
		if err := json.NewEncoder(&want).Encode(v); err != nil {
			t.Fatal(err)
		}
		if err := Write(&got, JSON, "anything", v); err != nil || !bytes.Equal(got.Bytes(), want.Bytes()) {
			t.Fatalf("%#v:\n%q\n%q %v", v, got.String(), want.String(), err)
		}
	}
}

// The generic layout (L1): every field but the top-level source, in the
// encoder's order; a list of objects as a table; deeper nesting pointed at
// --json; data from outside escaped.
func TestGeneric(t *testing.T) {
	type session struct {
		Name      string `json:"name"`
		SessionID string `json:"session_id"`
		Role      string `json:"role,omitempty"`
	}
	v := map[string]any{"source": "server", "data": map[string]any{
		"workspace": map[string]any{"name": "pilot", "owner": map[string]any{"deep": 1}},
		"principal": "ada@example.test\x1b[31m",
		"online":    true,
		"sessions":  []session{{Name: "lead", SessionID: "s_1", Role: "lead"}, {Name: "dev", SessionID: "s_2"}},
		"tags":      []string{"a", "b"},
		"empty":     "",
	}}
	doc, err := Generic(v)
	if err != nil {
		t.Fatal(err)
	}
	out := Render(doc, 100, style{})
	flat := strings.Join(strings.Fields(out), " ")
	for _, want := range []string{"Empty -", "Online yes", `Principal ada@example.test\x1b[31m`, "Tags a, b", "Workspace Name pilot Owner (… see --json)", "Sessions Name Session id Role lead s_1 lead dev s_2"} {
		if !strings.Contains(flat, want) {
			t.Fatalf("no %q in\n%s", want, out)
		}
	}
	if strings.Contains(out, "server") || strings.Contains(out, "\x1b") {
		t.Fatalf("source or a raw ESC in\n%s", out)
	}
	// Map keys sort, so the layout is stable.
	if strings.Index(out, "Empty") > strings.Index(out, "Online") {
		t.Fatal(out)
	}
	// Extra top-level fields come after data.
	doc, _ = Generic(map[string]any{"source": "x", "data": []any{}, "freshness": map[string]any{"stale": false}})
	if out := Render(doc, 100, style{}); !strings.Contains(out, "None.") || !strings.Contains(out, "Freshness") {
		t.Fatal(out)
	}
}

// A table too wide cuts the human columns, never a kept one (L4); a secret
// is printed whole on its own line, whatever the width.
func TestRenderWidth(t *testing.T) {
	var d Doc
	id := "inv_" + strings.Repeat("x", 40)
	d.Table([]Col{{Name: "ID", Keep: true}, {Name: "Detail"}}, [][]string{{id, strings.Repeat("long detail ", 20)}}, "")
	link := "https://bridge.example.test/join#" + strings.Repeat("k", 120)
	d.Secret("Invite link (shown once):", link)
	out := Render(d, 60, style{})
	if !strings.Contains(out, id) || !strings.Contains(out, "…") || !strings.Contains(out, "\n  "+link+"\n") {
		t.Fatal(out)
	}
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, "long detail") && len([]rune(line)) > 60 {
			t.Fatalf("not cut: %q", line)
		}
	}
	var e Doc
	e.Table([]Col{{Name: "Name"}}, nil, "No sessions.")
	if out := Render(e, 60, style{}); out != "No sessions.\n" {
		t.Fatalf("%q", out)
	}
}

// A secret's view never falls back to the generic layout (M3).
func TestSecretNeverGeneric(t *testing.T) {
	RegisterSecret("test_secret", func(v any) (Doc, bool) { return Doc{}, false })
	t.Cleanup(func() { viewsMu.Lock(); delete(views, "test_secret"); delete(secret, "test_secret"); viewsMu.Unlock() })
	var out bytes.Buffer
	v := map[string]any{"code": "CBJ-1234"}
	if err := Write(&out, Human, "test_secret", v); err != nil || strings.Count(out.String(), "CBJ-1234") != 1 || !strings.HasPrefix(out.String(), "{") {
		t.Fatal(out.String(), err)
	}
	if !IsSecret("test_secret") || IsSecret("whoami") {
		t.Fatal("IsSecret")
	}
}

// A generic table too wide for the terminal becomes one block per row,
// every field whole; one that fits stays a table.
func TestRecords(t *testing.T) {
	cols := []Col{{Name: "Name"}, {Name: "Detail"}, {Name: "Id", Keep: true}}
	var wide Doc
	wide.Records(cols, [][]string{{"a", strings.Repeat("x", 80), "id_1"}, {"b", "-", "id_2"}}, "None.")
	out := Render(wide, 60, style{})
	if !strings.Contains(out, "#1\n") || !strings.Contains(out, "#2\n") || !strings.Contains(out, strings.Repeat("x", 40)) || strings.Contains(out, "…") || strings.Count(out, "Detail") != 1 {
		t.Fatal(out)
	}
	var narrow Doc
	narrow.Records(cols, [][]string{{"a", "short", "id_1"}}, "None.")
	if out := Render(narrow, 60, style{}); !strings.Contains(out, "Name  Detail  Id") {
		t.Fatal(out)
	}
}
