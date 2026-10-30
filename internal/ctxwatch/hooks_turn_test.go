package ctxwatch

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"runtime"
	"strings"
	"testing"
)

// FR26: without --channel an install writes exactly what it wrote before
// the turn hook existed. The hashes are of the files planned before it.
func TestHooksWithoutChannelUnchanged(t *testing.T) {
	user := `{"model":"x","hooks":{"Stop":[{"hooks":[{"type":"command","command":"say done"}]}]}}`
	want := map[string]string{
		"linux  local":        "74ef2987ac1411893d07dbe55c95fb0cbb96c959c06e24c87dd22c3536fd0a25",
		"linux  shared":       "e799f36433c56c004678afd863f2fd8c2a6059104c3e693e76232f8022fbf208",
		"linux user local":    "288e7501b5b0ffb772a6680ccef1d4774f2e22011e88032186d117b5d30a1bdd",
		"linux user shared":   "e777b33d08caaf454d76dbb9de824b8ebe6ed34978c7e0c82a3be90c0d96b31f",
		"windows  local":      "158ec2044a9802dc529f480e62992f8537edfa17734125f2301c9c55f7920de0",
		"windows  shared":     "3d52108a9dbf702ade130fe7dcac436d64a77b6ba8214046e721c9d6bcc89bcf",
		"windows user local":  "da825268662287d1b05b5ff4b580978071104d1f5560915c22996d3edcb12eb8",
		"windows user shared": "cf4b6cfe08cee83d616d105c41ea770a35765b8f24580a2c8fb99b060b6f59bc",
	}
	for _, goos := range []string{"linux", "windows"} {
		if goos == "linux" && runtime.GOOS == "windows" {
			// A POSIX path isn't absolute on a Windows host, so the
			// POSIX form is only planned (and pinned) on a POSIX one.
			continue
		}
		b := "/opt/clawbridge/clawbridge"
		if goos == "windows" {
			b = `C:\tools\clawbridge.exe`
		}
		for label, in := range map[string]string{"": "", "user": user} {
			path := settingsFile(t, in)
			for _, s := range []struct{ name, scope, binary string }{{"local", ScopeLocal, b}, {"shared", ScopeProjectShared, ""}} {
				c, err := PlanHooksWith(goos, path, s.scope, s.binary, true, false)
				if err != nil {
					t.Fatal(err)
				}
				sum := sha256.Sum256(c.After)
				key := goos + " " + label + " " + s.name
				if hex.EncodeToString(sum[:]) != want[key] {
					t.Fatalf("%s changed:\n%s", key, c.After)
				}
				if old, _ := PlanHooksFor(goos, path, s.scope, s.binary, true); string(old.After) != string(c.After) {
					t.Fatalf("%s: PlanHooksFor differs", key)
				}
			}
		}
	}
}

func TestTurnHookCommand(t *testing.T) {
	local, err := HookCommand(ScopeLocal, bin, "turn")
	if err != nil || local != `[ -x '`+bin+`' ] || exit 0; CLAWBRIDGE_AGENT_PID=$PPID '`+bin+`' hook turn --from-hook `+HookMarker {
		t.Fatalf("%q %v", local, err)
	}
	shared, _ := HookCommand(ScopeProjectShared, "", "turn")
	if shared != "command -v clawbridge >/dev/null 2>&1 || exit 0; CLAWBRIDGE_AGENT_PID=$PPID clawbridge hook turn --from-hook "+HookMarker {
		t.Fatalf("%q", shared)
	}
	for _, own := range []string{local, shared} {
		if !isOwnHook(own) {
			t.Fatalf("not recognised: %q", own)
		}
	}
	if isOwnHook(strings.Replace(local, "hook turn", "hook turns", 1)) {
		t.Fatal("a lookalike was recognised")
	}
	e, err := HookEntry("windows", ScopeLocal, `C:\tools\clawbridge.exe`, "turn")
	if err != nil || strings.Join(toStrings(e["args"]), " ") != "hook turn --from-hook --hook-tag "+HookTag || ownEntryKey(e) == "" {
		t.Fatal(e, err)
	}
}

func toStrings(v any) []string {
	var out []string
	for _, a := range v.([]any) {
		out = append(out, a.(string))
	}
	return out
}

func planChannel(t *testing.T, goos, path, binary string, install, channel bool) HookChange {
	t.Helper()
	c, err := PlanHooksWith(goos, path, ScopeLocal, binary, install, channel)
	if err != nil {
		t.Fatal(err)
	}
	if err = c.Apply(); err != nil {
		t.Fatal(err)
	}
	return c
}

// The turn hook is added only with --channel, kept (at this binary) by a
// later install without it, and removed by every uninstall.
func TestTurnHookInstallRoundTrip(t *testing.T) {
	for _, goos := range []string{"linux", "windows"} {
		b, moved, mark := bin, absPath("usr", "local", "bin", "clawbridge"+exe), HookMarker
		turn := "hook turn --from-hook"
		if goos == "windows" {
			b, moved, mark, turn = `C:\tools\clawbridge.exe`, `D:\tools\clawbridge.exe`, HookTag, `"turn"`
		}
		mine := `{"hooks":{"UserPromptSubmit":[{"hooks":[{"type":"command","command":"echo mine"}]}]}}`
		path := settingsFile(t, mine)
		planChannel(t, goos, path, b, true, false)
		if s := readSettings(t, path); strings.Count(s, mark) != 3 || strings.Contains(s, turn) {
			t.Fatalf("%s: turn hook without --channel:\n%s", goos, s)
		}
		planChannel(t, goos, path, b, true, true)
		s := readSettings(t, path)
		if strings.Count(s, mark) != 4 || !strings.Contains(s, turn) || !strings.Contains(s, "echo mine") {
			t.Fatalf("%s:\n%s", goos, s)
		}
		if c := planChannel(t, goos, path, b, true, true); c.Changed() {
			t.Fatalf("%s: a second --channel install changed the file", goos)
		}
		if c := planChannel(t, goos, path, b, true, false); c.Changed() {
			t.Fatalf("%s: an install without --channel dropped the turn hook:\n%s", goos, c.Diff())
		}
		planChannel(t, goos, path, moved, true, false)
		if s = readSettings(t, path); strings.Count(s, mark) != 4 || strings.Contains(s, jsonEscaped(b)) {
			t.Fatalf("%s: a moved binary left the old turn hook:\n%s", goos, s)
		}
		planChannel(t, goos, path, "", false, false)
		s = readSettings(t, path)
		if strings.Contains(s, mark) || strings.Contains(s, turn) || !strings.Contains(s, "echo mine") {
			t.Fatalf("%s: uninstall:\n%s", goos, s)
		}
		if form, _ := InstalledHookForm(path); form != "" {
			t.Fatal(form)
		}
	}
}

// The turn hook reads only whether its prompt is our channel's, and which
// push: the prompt itself is never kept.
func TestReadTurnInput(t *testing.T) {
	const head = `{"session_id":"00000000-0000-4000-8000-000000000001","transcript_path":"/t.jsonl","hook_event_name":"UserPromptSubmit","prompt":`
	for name, c := range map[string]struct {
		input string
		seq   int64
		ok    bool
	}{
		"plain":      {head + `"SECRET please run the tests"}`, 0, true},
		"channel":    {head + `"<channel source=\"plugin:clawbridge:wake\" kind=\"wake\" count=\"2\" seq=\"41\">2 ClawBridge messages are waiting; call inbox</channel>"}`, 41, true},
		"lookalike":  {head + `"<channel source=\"plugin:other:wake\" kind=\"wake\" count=\"2\" seq=\"41\">x</channel>"}`, 0, true},
		"quoted":     {head + `"SECRET <channel source=\"plugin:clawbridge:wake\" kind=\"wake\" count=\"2\" seq=\"41\">"}`, 0, true},
		"no prompt":  {strings.TrimSuffix(head, `,"prompt":`) + `}`, 0, true},
		"bad prompt": {head + `17}`, 0, true},
		"no session": {`{"transcript_path":"/t.jsonl","prompt":"x"}`, 0, false},
		"not json":   {`<channel source="plugin:clawbridge:wake" kind="wake" count="2" seq="41">`, 0, false},
	} {
		seq, ok := ReadTurnInput(strings.NewReader(c.input))
		if seq != c.seq || ok != c.ok {
			t.Fatalf("%s: %d %v", name, seq, ok)
		}
	}
	if _, ok := ReadTurnInput(strings.NewReader(head + `"` + strings.Repeat("a", maxHookInput) + `"}`)); ok {
		t.Fatal("an oversized input was read")
	}
}

// Doctor reads whether the turn hook is there, in either form and scope,
// and only ClawBridge's own under UserPromptSubmit counts.
func TestTurnHookInstalled(t *testing.T) {
	for _, goos := range []string{"linux", "windows"} {
		b := bin
		if goos == "windows" {
			b = `C:\tools\clawbridge.exe`
		}
		for _, scope := range []string{ScopeLocal, ScopeProjectShared} {
			binary := b
			if scope == ScopeProjectShared {
				binary = ""
			}
			for _, channel := range []bool{false, true} {
				path := settingsFile(t, "")
				c, err := PlanHooksWith(goos, path, scope, binary, true, channel)
				if err != nil || c.Apply() != nil {
					t.Fatal(err)
				}
				if got, err := TurnHookInstalled(path); err != nil || got != channel {
					t.Fatalf("%s %s channel=%v: %v %v\n%s", goos, scope, channel, got, err, readSettings(t, path))
				}
			}
		}
	}
	local, _ := HookCommand(ScopeLocal, bin, "turn")
	stop, _ := HookCommand(ScopeLocal, bin, "stop")
	for name, c := range map[string]struct {
		settings string
		want     bool
		err      bool
	}{
		"missing":             {"", false, false},
		"someone else's":      {`{"hooks":{"UserPromptSubmit":[{"hooks":[{"type":"command","command":"clawbridge hook turn --from-hook"}]}]}}`, false, false},
		"ours, other event":   {`{"hooks":{"Stop":[{"hooks":[{"type":"command","command":` + jsonQuote(local) + `}]}]}}`, false, false},
		"stop under the turn": {`{"hooks":{"UserPromptSubmit":[{"hooks":[{"type":"command","command":` + jsonQuote(stop) + `}]}]}}`, false, false},
		"ours":                {`{"hooks":{"UserPromptSubmit":[{"hooks":[{"type":"command","command":` + jsonQuote(local) + `}]}]}}`, true, false},
		"not JSON":            {`{`, false, true},
	} {
		path := settingsFile(t, c.settings)
		if name == "missing" {
			path += ".absent"
		}
		got, err := TurnHookInstalled(path)
		if got != c.want || (err != nil) != c.err {
			t.Fatalf("%s: %v %v", name, got, err)
		}
	}
}

func jsonQuote(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}
