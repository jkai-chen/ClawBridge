package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// The JSON guard (live/CLI-OUTPUT-PLAN.md §6): what each command prints for
// a script or an agent, recorded from main before the human output existed.
// Every later change must leave it byte for byte, on a pipe and with --json
// alike. Times, generated IDs and paths are normalised by the patterns
// below; nothing else is. Each pattern matches a whole JSON string, quotes
// included, in the exact shape the store writes, so a message body or a note
// that merely contains a time or an ID is compared as it is. Regenerate only
// with CLAWBRIDGE_UPDATE_JSON_GUARD=1, and only for a change meant to alter
// the JSON.
var jsonGuardVolatile = []struct {
	re   *regexp.Regexp
	with string
}{
	{regexp.MustCompile(`"\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(\.\d+)?Z"`), `"<TIME>"`},
	{regexp.MustCompile(`"(evt|msg|prj|ses|ws)_[0-9a-f]{32}"`), `"<ID>"`},
	{regexp.MustCompile(`"(go|version|commit)":"[^"]*"`), `"$1":"<BUILD>"`},
	// A local store's principal is this OS user's, exactly as the store
	// writes it (R170: on Windows a SID and DOMAIN\user, so no pattern).
	{regexp.MustCompile(regexp.QuoteMeta(localPrincipalJSON(nil))), `"local:<USER>"`},
}

// localPrincipalJSON is the OS user's local principal as a JSON string,
// quotes included: local:UID:NAME, as bus.Open makes it. A backslash in a
// Windows name is escaped, as in the output. u is the user, or nil for the
// current one.
func localPrincipalJSON(u *user.User) string {
	if u == nil {
		var err error
		if u, err = user.Current(); err != nil {
			return `"local:<unknown user>"`
		}
	}
	b, _ := json.Marshal("local:" + u.Uid + ":" + u.Username)
	return string(b)
}

// The patterns touch only whole values of their shape (R170-L3).
func TestJSONGuardVolatileAnchored(t *testing.T) {
	normalise := func(s string) string {
		for _, v := range jsonGuardVolatile {
			s = v.re.ReplaceAllString(s, v.with)
		}
		return s
	}
	for in, want := range map[string]string{
		`{"id":"msg_d64f3c174d7fbe9c74072434be60e4d7"}`:               `{"id":"<ID>"}`,
		`{"created_at":"2026-10-27T16:31:03.368169000Z"}`:             `{"created_at":"<TIME>"}`,
		`{"version":"v0.3.6","commit":"abc"}`:                         `{"version":"<BUILD>","commit":"<BUILD>"}`,
		`{"body":"see msg_d64f3c174d7fbe9c74072434be60e4d7 at noon"}`: `{"body":"see msg_d64f3c174d7fbe9c74072434be60e4d7 at noon"}`,
		`{"body":"due 2026-10-27T16:31:03Z, maybe"}`:                  `{"body":"due 2026-10-27T16:31:03Z, maybe"}`,
		`{"id":"p_short"}`: `{"id":"p_short"}`,
		`{"id":"msg_D64F3C174D7FBE9C74072434BE60E4D7"}`:              `{"id":"msg_D64F3C174D7FBE9C74072434BE60E4D7"}`,
		`{"body":"\"msg_d64f3c174d7fbe9c74072434be60e4d7\" quoted"}`: `{"body":"\"msg_d64f3c174d7fbe9c74072434be60e4d7\" quoted"}`,
	} {
		if got := normalise(in); got != want {
			t.Errorf("%s: got %s, want %s", in, got, want)
		}
	}
	// The principal: only the whole value, and only this user's.
	me := localPrincipalJSON(nil)
	for in, want := range map[string]string{
		`{"owner_principal":` + me + `}`:                    `{"owner_principal":"local:<USER>"}`,
		`{"notes":"` + strings.Trim(me, `"`) + ` said hi"}`: `{"notes":"` + strings.Trim(me, `"`) + ` said hi"}`,
		`{"owner_principal":"local:1:someone-else"}`:        `{"owner_principal":"local:1:someone-else"}`,
	} {
		if got := normalise(in); got != want {
			t.Errorf("%s: got %s, want %s", in, got, want)
		}
	}
	// A Windows principal (a SID, DOMAIN\user) is JSON-escaped as written.
	if got := localPrincipalJSON(&user.User{Uid: "S-1-5-21-1-500", Username: `host\admin`}); got != `"local:S-1-5-21-1-500:host\\admin"` {
		t.Fatal(got)
	}
}

func TestJSONOutputGuard(t *testing.T) {
	pkg, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	dir := isolated(t)
	normalise := func(s string) string {
		s = strings.ReplaceAll(s, dir, "<DIR>")
		if real, err := filepath.EvalSymlinks(dir); err == nil {
			s = strings.ReplaceAll(s, real, "<DIR>")
		}
		for _, v := range jsonGuardVolatile {
			s = v.re.ReplaceAllString(s, v.with)
		}
		return s
	}
	run := func(args ...string) string {
		t.Helper()
		var out, errs bytes.Buffer
		if err := Run(context.Background(), args, &out, &errs); err != nil {
			t.Fatalf("%v: %v (%s)", args, err, errs.String())
		}
		return out.String()
	}
	var got strings.Builder
	// msgID is the sent message's ID, the one argument a header carries
	// that changes from run to run.
	var msgID string
	header := func(args []string) string {
		h := strings.Join(args, " ")
		if msgID != "" {
			h = strings.ReplaceAll(h, msgID, "<ID>")
		}
		return "### " + h + "\n"
	}
	record := func(args ...string) string {
		t.Helper()
		plain := run(args...)
		withFlag := run(append(append([]string(nil), args...), "--json")...)
		if normalise(plain) != normalise(withFlag) {
			t.Fatalf("%v: --json differs from the pipe:\n%s\n%s", args, plain, withFlag)
		}
		got.WriteString(header(args) + normalise(plain))
		return plain
	}
	// Commands that change state run once (their --json twin would repeat
	// the change): only their pipe output is recorded.
	once := func(args ...string) string {
		t.Helper()
		out := run(args...)
		got.WriteString(header(args) + normalise(out))
		return out
	}

	once("version") // version takes no arguments, --json included
	once("setup", "--local", "--workspace", "w")
	once("project", "create", "p")
	t.Setenv("CLAWBRIDGE_PROJECT", "p")
	once("register", "a", "--instance", "a-one")
	once("register", "b", "--instance", "b-one")
	record("whoami", "--session", "a", "--instance", "a-one")
	record("sessions")
	sent := once("--session", "a", "send", "b", "hello <guard> & é", "--instance", "a-one", "--operation-id", "one")
	id := regexp.MustCompile(`"id":"([^"]+)"`).FindStringSubmatch(sent)
	if id == nil {
		t.Fatal(sent)
	}
	msgID = id[1]
	record("inbox", "--session=b", "--instance=b-one")
	record("inbox-status", "--session=b", "--instance=b-one")
	record("status", id[1], "--session=a", "--instance=a-one")
	once("--session", "b", "--instance", "b-one", "mark-read", id[1])
	once("reply", id[1], "reply from guard", "--session", "b", "--instance", "b-one", "--operation-id", "reply")
	record("history", "--session=a", "--instance=a-one")
	record("notification-status", "--session=a", "--instance=a-one")
	once("notification-policy", "--policy", "hold", "--session=a", "--instance=a-one")
	once("update", "--session", "a", "--instance", "a-one", "--notes", "guarding")
	record("dispatcher", "show")
	record("project", "context-watch", "show")

	golden := filepath.Join(pkg, "testdata", "json_guard.golden")
	if os.Getenv("CLAWBRIDGE_UPDATE_JSON_GUARD") == "1" {
		if err := os.MkdirAll(filepath.Dir(golden), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(golden, []byte(got.String()), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(golden)
	if err != nil {
		t.Fatal(err)
	}
	if got.String() != string(want) {
		t.Fatalf("the JSON output changed (%s):\n--- got\n%s\n--- want\n%s", firstDifference(got.String(), string(want)), got.String(), want)
	}
}

// firstDifference names the first line where got and want differ, quoted,
// so a difference no one can see (a CR, a trailing space) is readable.
func firstDifference(got, want string) string {
	g, w := strings.SplitAfter(got, "\n"), strings.SplitAfter(want, "\n")
	for i := 0; i < len(g) || i < len(w); i++ {
		var gl, wl string
		if i < len(g) {
			gl = g[i]
		}
		if i < len(w) {
			wl = w[i]
		}
		if gl != wl {
			return fmt.Sprintf("first at line %d: got %q, want %q", i+1, gl, wl)
		}
	}
	return "no line differs"
}

// A mismatch no one can see, such as a golden checked out with CRLF, is named.
func TestFirstDifferenceShowsTheBytes(t *testing.T) {
	if got := firstDifference("a\nb\n", "a\r\nb\r\n"); got != `first at line 1: got "a\n", want "a\r\n"` {
		t.Fatal(got)
	}
	if got := firstDifference("a\n", "a\nb"); got != `first at line 2: got "", want "b"` {
		t.Fatal(got)
	}
	if got := firstDifference("a\n", "a\n"); got != "no line differs" {
		t.Fatal(got)
	}
}
