package launchcmd

import (
	"context"
	"errors"
	"os/exec"
	"runtime"
	"strings"
	"testing"

	"clawbridge/internal/bus"
	"clawbridge/internal/secretenv"
	"github.com/pelletier/go-toml/v2"
)

func own(agent string) Input {
	return Input{Agent: agent, Session: "api-worker", Instance: "inst_abc", MCPConfigPath: "/state/mcp/api-worker.json", CodexCommand: "/usr/local/bin/clawbridge", CodexArgs: []string{"mcp", "--session", "api-worker"}, OwnerPrincipal: "mara@example.test", ViewerPrincipal: "mara@example.test"}
}

func TestClaudeCodeLine(t *testing.T) {
	got, err := Build(own(ClaudeCode))
	if err != nil {
		t.Fatal(err)
	}
	want := "CLAWBRIDGE_SESSION=api-worker CLAWBRIDGE_INSTANCE=inst_abc claude --mcp-config /state/mcp/api-worker.json"
	if got != want {
		t.Fatalf("got %q want %q", got, want)
	}
}

func TestCodexLinePassesTheServerWithOverridesUnderOneFixedName(t *testing.T) {
	got, err := Build(own(Codex))
	if err != nil {
		t.Fatal(err)
	}
	want := `CLAWBRIDGE_SESSION=api-worker CLAWBRIDGE_INSTANCE=inst_abc codex -c 'mcp_servers.clawbridge.command="/usr/local/bin/clawbridge"' -c 'mcp_servers.clawbridge.args=["mcp", "--session", "api-worker"]'`
	if got != want {
		t.Fatalf("got  %s\nwant %s", got, want)
	}
	in := own(Codex)
	in.CodexCommand = ""
	if _, err = Build(in); err == nil {
		t.Fatal("codex without a server accepted")
	}
}

// Both layers: the TOML value is escaped, then the whole override is
// shell-quoted. A real shell gives the override back, and a TOML parser
// gives the values back.
func TestCodexOverridesSurviveTheShellAndTOMLWithHostileValues(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("needs a POSIX shell")
	}
	hostileValues := []string{
		`/opt/o'brien/my "apps"/clawbridge`, `/p a/$HOME/` + "`id`" + `/cb`, "line\nbreak\ttab", `back\slash\"quote`, "naïve-日本語-😀",
		"ctl\x01\x7f", "]\n[mcp_servers.evil]\ncommand = \"x\"", `" -c 'x=1`, "'; rm -rf / #",
	}
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("no sh")
	}
	for _, v := range hostileValues {
		in := own(Codex)
		in.CodexCommand = v
		in.CodexArgs = []string{"mcp", "--session", v, "--db", v}
		line, err := Build(in)
		if err != nil {
			t.Fatal(err)
		}
		// Run the line's words through a shell that prints each argument on
		// its own line, NUL separated; the leading assignments go with it.
		script := "codex() { for a in \"$@\"; do printf '%s\\0' \"$a\"; done; }; " + line
		out, err := exec.Command(sh, "-c", script).Output()
		if err != nil {
			t.Fatalf("%q: %v", v, err)
		}
		args := strings.Split(strings.TrimSuffix(string(out), "\x00"), "\x00")
		if len(args) != 4 || args[0] != "-c" || args[2] != "-c" {
			t.Fatalf("%q: shell gave %q", v, args)
		}
		var doc struct {
			Mcp_servers map[string]struct {
				Command string
				Args    []string
			}
		}
		if err = toml.Unmarshal([]byte(args[1]+"\n"+args[3]), &doc); err != nil {
			t.Fatalf("%q: not TOML: %v\n%s\n%s", v, err, args[1], args[3])
		}
		srv := doc.Mcp_servers["clawbridge"]
		if len(doc.Mcp_servers) != 1 || srv.Command != v || len(srv.Args) != 5 || srv.Args[2] != v || srv.Args[3] != "--db" || srv.Args[4] != v {
			t.Fatalf("%q: servers %+v", v, doc.Mcp_servers)
		}
	}
}

func TestRefusesAnotherPrincipalsSession(t *testing.T) {
	for _, agent := range []string{ClaudeCode, Codex} {
		in := own(agent)
		in.OwnerPrincipal = "kofi@example.test"
		line, err := Build(in)
		if err == nil || err.Error() != "only kofi@example.test can start this agent" || line != "" {
			t.Fatalf("%s: line %q err %v", agent, line, err)
		}
	}
}

func TestRefusesWhenWhoIsUnknown(t *testing.T) {
	for _, mod := range []func(*Input){func(i *Input) { i.OwnerPrincipal = "" }, func(i *Input) { i.ViewerPrincipal = "" }} {
		in := own(ClaudeCode)
		mod(&in)
		if _, err := Build(in); err == nil {
			t.Fatal("an unknown owner or viewer must be refused, even when both are empty and so equal")
		}
	}
}

func TestRefusesUnknownAgentAndMissingParts(t *testing.T) {
	in := own("gemini")
	if _, err := Build(in); err == nil {
		t.Fatal("unknown agent accepted")
	}
	in = own(ClaudeCode)
	in.Instance = ""
	if _, err := Build(in); err == nil {
		t.Fatal("empty instance accepted")
	}
	in = own(ClaudeCode)
	in.MCPConfigPath = ""
	if _, err := Build(in); err == nil {
		t.Fatal("claude-code without a config path accepted")
	}
}

func TestOwnerNameInTheErrorIsPrintable(t *testing.T) {
	in := own(Codex)
	in.OwnerPrincipal = "evil\x1b[31m‮@example.test"
	_, err := Build(in)
	if err == nil || strings.ContainsAny(err.Error(), "\x1b‮") {
		t.Fatalf("error %q carries control characters", err)
	}
}

var hostile = []string{
	"plain", "with space", "it's", "'", "''", "$HOME", "$(rm -rf /)", "`id`", "a\nb", "a\r\nb", "tab\there",
	"naïve-日本語-😀", "semi;colon", "amp&&x", "pipe|x", "glob*?[a]", "~/x", "#c", "!bang", `back\slash`, `"dq"`, "-leading", "",
}

func TestQuoteRoundTripsThroughARealShell(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("needs a POSIX shell")
	}
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("no sh")
	}
	for _, s := range hostile {
		out, err := exec.Command(sh, "-c", "printf %s "+Quote(s)).Output()
		if err != nil {
			t.Fatalf("%q: %v", s, err)
		}
		if string(out) != s {
			t.Fatalf("%q came back as %q", s, out)
		}
	}
}

func TestQuoteShapes(t *testing.T) {
	cases := map[string]string{
		"abc-1_2.x/y": "abc-1_2.x/y",
		"":            "''",
		"a b":         "'a b'",
		"it's":        `'it'\''s'`,
		"$x":          "'$x'",
		"`x`":         "'`x`'",
		"a\nb":        "'a\nb'",
		"é":           "'é'",
	}
	for in, want := range cases {
		if got := Quote(in); got != want {
			t.Errorf("Quote(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestEveryVariablePartIsQuoted(t *testing.T) {
	in := own(ClaudeCode)
	in.Session, in.Instance, in.MCPConfigPath = "a b; rm -rf /", "i'`id`", "/p a/$HOME/x.json"
	got, err := Build(in)
	if err != nil {
		t.Fatal(err)
	}
	want := `CLAWBRIDGE_SESSION='a b; rm -rf /' CLAWBRIDGE_INSTANCE='i'\''` + "`id`" + `' claude --mcp-config '/p a/$HOME/x.json'`
	if got != want {
		t.Fatalf("got  %s\nwant %s", got, want)
	}
}

// The line carries two environment names and the agent's flag, never a
// secret's name, whatever the session is called.
func TestNoSecretNameCanAppear(t *testing.T) {
	names := append([]string{"CLAWBRIDGE_DEVICE_KEY", "CLAWBRIDGE_DATABASE_URL", "CLAWBRIDGE_TOKEN", "CLAWBRIDGE_X_SECRET"}, hostile...)
	for _, agent := range []string{ClaudeCode, Codex} {
		for _, n := range names {
			in := own(agent)
			in.Session, in.Instance = n+"x", n+"y"
			if n == "" {
				in.Session, in.Instance = "s", "i"
			}
			line, err := Build(in)
			if err != nil {
				t.Fatal(err)
			}
			// Assignment names are the words before "=" in the leading
			// NAME=value words; a hostile value is one quoted word.
			rest := line
			var assigns []string
			for strings.HasPrefix(rest, "CLAWBRIDGE_") {
				name, after, _ := strings.Cut(rest, "=")
				assigns = append(assigns, name)
				rest = skipWord(after)
			}
			if len(assigns) != 2 || assigns[0] != "CLAWBRIDGE_SESSION" || assigns[1] != "CLAWBRIDGE_INSTANCE" {
				t.Fatalf("%s %q: assignments %v in %q", agent, n, assigns, line)
			}
			for _, a := range assigns {
				if secretenv.Secret(a) {
					t.Fatalf("%s is a secret name", a)
				}
			}
		}
	}
}

// skipWord drops one shell word (single-quote aware) and the space after it.
func skipWord(s string) string {
	inQuote, skip := false, false
	for i, r := range s {
		if skip {
			skip = false
			continue
		}
		switch {
		case r == '\\' && !inQuote:
			skip = true
		case r == '\'':
			inQuote = !inQuote
		case r == ' ' && !inQuote:
			return s[i+1:]
		}
	}
	return ""
}

func TestSlug(t *testing.T) {
	if Slug("api-worker_1") != "api-worker_1" {
		t.Fatal("a safe name must be kept")
	}
	a, b := Slug("atlas/billing-retry"), Slug("atlas-billing-retry")
	if a == b || !strings.HasPrefix(a, "atlas-billing-retry-") {
		t.Fatalf("slugs %q %q must differ", a, b)
	}
	if strings.ContainsAny(Slug("../../etc/x y"), "/. ") {
		t.Fatal("slug must not carry path characters")
	}
	if got := ConfigPath("/st", "api-worker"); got != "/st/mcp/api-worker.json" {
		t.Fatal(got)
	}
}

type fakeExec struct {
	who      string
	sessions []map[string]any
	paged    bool
	calls    []string
}

func (f *fakeExec) Execute(_ context.Context, r bus.Request) (bus.Result, error) {
	f.calls = append(f.calls, r.Operation)
	switch r.Operation {
	case "whoami":
		return bus.Result{Data: map[string]any{"principal": f.who}}, nil
	case "list_sessions":
		after, _ := r.Args["after"].(int64)
		if f.paged && after == 0 {
			return bus.Result{Data: map[string]any{"sessions": []map[string]any{{"name": "filler"}}, "next_cursor": int64(1), "has_more": true}}, nil
		}
		return bus.Result{Data: map[string]any{"sessions": f.sessions, "next_cursor": int64(9), "has_more": false}}, nil
	}
	return bus.Result{}, errors.New("unexpected " + r.Operation)
}

func fixtureExec() *fakeExec {
	return &fakeExec{who: "mara@example.test", sessions: []map[string]any{
		{"name": "mine", "instance": "inst_m", "owner_principal": "mara@example.test", "host": "mara-mbp"},
		{"name": "theirs", "instance": "", "owner_principal": "kofi@example.test", "host": "kofi-pc"},
	}}
}

func TestSourceBuildsOwnAndRefusesOthersWithoutWriting(t *testing.T) {
	var ensured []string
	src := Source{Exec: fixtureExec(), StateDir: "/st", Prepare: func(_ context.Context, agent, session, instance, path string) (Prep, error) {
		ensured = append(ensured, agent+"|"+session+"|"+instance+"|"+path)
		return Prep{}, nil
	}}
	got, err := src.Command(context.Background(), "mine", ClaudeCode)
	if err != nil {
		t.Fatal(err)
	}
	if got.Line != "CLAWBRIDGE_SESSION=mine CLAWBRIDGE_INSTANCE=inst_m claude --mcp-config /st/mcp/mine.json" || got.Host != "mara-mbp" {
		t.Fatalf("%+v", got)
	}
	if len(ensured) != 1 || ensured[0] != "claude-code|mine|inst_m|/st/mcp/mine.json" {
		t.Fatalf("ensure calls %v", ensured)
	}
	_, err = src.Command(context.Background(), "theirs", ClaudeCode)
	if err == nil || err.Error() != "only kofi@example.test can start this agent" {
		t.Fatalf("err %v", err)
	}
	if len(ensured) != 1 {
		t.Fatal("the other principal's session wrote a config")
	}
	if _, err = src.Command(context.Background(), "nope", Codex); err == nil {
		t.Fatal("unknown session accepted")
	}
	if _, err = src.Command(context.Background(), "mine", "gemini"); err == nil {
		t.Fatal("unknown agent accepted")
	}
}

func TestSourceCodexLineCarriesTheServerAndTheWarnings(t *testing.T) {
	src := Source{Exec: fixtureExec(), StateDir: "/st", Prepare: func(_ context.Context, agent, _, _, path string) (Prep, error) {
		if path != "" {
			t.Fatalf("codex got a path %q", path)
		}
		return Prep{CodexCommand: "/bin/clawbridge", CodexArgs: []string{"mcp", "--session", "mine"}, Warnings: []string{OtherEntryWarning("old", "mine")}}, nil
	}}
	got, err := src.Command(context.Background(), "mine", Codex)
	if err != nil || !strings.HasPrefix(got.Line, "CLAWBRIDGE_SESSION=mine CLAWBRIDGE_INSTANCE=inst_m codex -c 'mcp_servers.clawbridge.command=") {
		t.Fatalf("%+v %v", got, err)
	}
	if len(got.Warnings) != 1 || got.Warnings[0] != "Your Codex config also starts ClawBridge as old; remove it so this agent acts only as mine." {
		t.Fatalf("%v", got.Warnings)
	}
}

func TestCodexOtherEntries(t *testing.T) {
	cfg := `
[mcp_servers.clawbridge]
command = "/usr/bin/clawbridge"
[mcp_servers.worker-a]
command = "/home/u/bin/clawbridge"
args = ["mcp"]
[mcp_servers.win]
command = 'C:\tools\ClawBridge.exe'
[mcp_servers.other]
command = "/bin/true"
[mcp_servers.notit]
command = "/bin/clawbridge-agentd"
`
	got := CodexOtherEntries([]byte(cfg))
	if strings.Join(got, ",") != "win,worker-a" {
		t.Fatalf("%v", got)
	}
	if CodexOtherEntries([]byte("not = [toml")) != nil || CodexOtherEntries(nil) != nil {
		t.Fatal("junk must say nothing")
	}
}

func TestSourceFindsASessionOnALaterPageAndRefusesWhenWhoamiIsEmpty(t *testing.T) {
	f := fixtureExec()
	f.paged = true
	src := Source{Exec: f, StateDir: "/st", Prepare: func(context.Context, string, string, string, string) (Prep, error) {
		return Prep{CodexCommand: "/bin/clawbridge", CodexArgs: []string{"mcp"}}, nil
	}}
	if _, err := src.Command(context.Background(), "mine", Codex); err != nil {
		t.Fatal(err)
	}
	f = fixtureExec()
	f.who = ""
	if _, err := (Source{Exec: f}).Command(context.Background(), "mine", Codex); err == nil {
		t.Fatal("an unknown viewer must be refused")
	}
}
