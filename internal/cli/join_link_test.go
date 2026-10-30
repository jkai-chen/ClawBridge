package cli

import (
	"bytes"
	"context"
	"os"
	"strings"
	"testing"

	"clawbridge/internal/devicekey"
)

func testLink(t *testing.T, server string) string {
	t.Helper()
	link, err := devicekey.FormatLink(server, "acme", "ada@example.com", "CBJ-4K7P-WQ9D-2MXH-T6RB")
	if err != nil {
		t.Fatal(err)
	}
	return link
}

// §17.1, A1-D1: `clawbridge join` takes the whole link at a prompt or on
// stdin, never on the command line, and asks before joining.
func TestJoinWithALink(t *testing.T) {
	f := newJoinFixture(t)
	link := testLink(t, f.url)
	join := func(args []string, stdin string) (string, string, error) {
		var out, errOut bytes.Buffer
		err := runDeviceJoin(context.Background(), Config{}, args, strings.NewReader(stdin), &out, &errOut)
		return out.String(), errOut.String(), err
	}
	// On the command line: refused, nothing made.
	for _, args := range [][]string{{link}, {"CBJ-4K7P-WQ9D-2MXH-T6RB"}} {
		if _, _, err := join(args, ""); err == nil || !strings.Contains(err.Error(), "paste the link at the prompt") || len(f.keys) != 0 {
			t.Fatal(args, err)
		}
	}
	// Piped, it needs --yes, since nothing can ask.
	if _, _, err := join(nil, link+"\n"); err == nil || !strings.Contains(err.Error(), "Join workspace acme on 127.0.0.1") || !strings.Contains(err.Error(), "--yes") || len(f.keys) != 0 {
		t.Fatal(err)
	}
	// A link for another server than --server is refused.
	var out, errOut bytes.Buffer
	if err := runDeviceJoin(context.Background(), Config{Server: "https://other.example.com"}, []string{"--yes"}, strings.NewReader(link+"\n"), &out, &errOut); err == nil || !strings.Contains(err.Error(), "leave --server out") {
		t.Fatal(err)
	}
	if _, _, err := join([]string{"--yes"}, "https://cb.example.com/join#nope\n"); err == nil {
		t.Fatal("joined with a broken link")
	}
	got, prompts, err := join([]string{"--yes", "--label", "laptop"}, link+"\n")
	if err != nil || len(f.keys) != 1 || !strings.Contains(got, f.keys[0]) || !strings.Contains(prompts, "Paste the invite link") || strings.Contains(prompts, "CBJ-4K7P") {
		t.Fatal(err, got, prompts)
	}
}

// At a terminal the link is read unechoed and the join waits for a yes.
func TestJoinLinkAtATerminalAsksFirst(t *testing.T) {
	f := newJoinFixture(t)
	oldTerm, oldHidden := stdinTerminal, readHidden
	stdinTerminal = func() bool { return true }
	readHidden = func() ([]byte, error) { return []byte(testLink(t, f.url)), nil }
	t.Cleanup(func() { stdinTerminal, readHidden = oldTerm, oldHidden })
	// go test's stdin answers nothing: no yes, no join.
	var out, errOut bytes.Buffer
	if err := runDeviceJoin(context.Background(), Config{}, nil, os.Stdin, &out, &errOut); err != nil || len(f.keys) != 0 || !strings.Contains(errOut.String(), "Join workspace acme on 127.0.0.1") || !strings.Contains(errOut.String(), "Canceled") {
		t.Fatal(err, errOut.String())
	}
}

// The --email path's code prompt takes the link too, if it's for that email.
func TestJoinCodePromptTakesTheLink(t *testing.T) {
	f := newJoinFixture(t)
	var out, errOut bytes.Buffer
	if err := runDeviceJoin(context.Background(), Config{Server: f.url}, []string{"--email", "bob@example.com"}, strings.NewReader(testLink(t, f.url)+"\n"), &out, &errOut); err == nil || !strings.Contains(err.Error(), "for ada@example.com") {
		t.Fatal(err)
	}
	if err := runDeviceJoin(context.Background(), Config{Server: f.url}, []string{"--email", "ada@example.com"}, strings.NewReader(testLink(t, f.url)+"\n"), &out, &errOut); err != nil || len(f.keys) != 1 {
		t.Fatal(err)
	}
}

// invite and device add print the link with the code; --out writes the link.
func TestInvitePrintsTheLink(t *testing.T) {
	cloudProfile(t)
	onTerminal(t)
	r := &accountRecorder{data: map[string]any{"code": "CBJ-4K7P-WQ9D-2MXH-T6RB", "email": "ada@example.com", "invite_id": "inv_1"}}
	withAccountRecorder(t, r)
	out, err := run("device", "add")
	if err != nil || !strings.Contains(out, `"link":"https://cb.example.test/join#cbj1.acme.YWRhQGV4YW1wbGUuY29t.CBJ-4K7P-WQ9D-2MXH-T6RB"`) {
		t.Fatal(err, out)
	}
	file := t.TempDir() + "/invite"
	if out, err = run("invite", "--email", "ada@example.com", "--grant", "api=member", "--out", file); err != nil || strings.Contains(out, "CBJ-") {
		t.Fatal(err, out)
	}
	b, _ := os.ReadFile(file)
	if link, e := devicekey.ParseLink(string(b)); e != nil || link.Email != "ada@example.com" {
		t.Fatalf("%q %v", b, e)
	}
}

// R159: the code prompt checks a pasted link's server too (L1); a link
// whose workspace name differs from the server's answer is noted (L2); and
// an invite without a link says why (L3).
func TestJoinLinkChecksR159(t *testing.T) {
	f := newJoinFixture(t)
	other := testLink(t, "https://other.example.com")
	var out, errOut bytes.Buffer
	if err := runDeviceJoin(context.Background(), Config{Server: f.url}, []string{"--email", "ada@example.com"}, strings.NewReader(other+"\n"), &out, &errOut); err == nil || !strings.Contains(err.Error(), "not --server") || len(f.keys) != 0 {
		t.Fatal(err)
	}
	renamed, err := devicekey.FormatLink(f.url, "old-name", "ada@example.com", "CBJ-4K7P-WQ9D-2MXH-T6RB")
	if err != nil {
		t.Fatal(err)
	}
	errOut.Reset()
	if err = runDeviceJoin(context.Background(), Config{}, []string{"--yes"}, strings.NewReader(renamed+"\n"), &out, &errOut); err != nil || !strings.Contains(errOut.String(), "the link said workspace old-name, but the server joined you to acme") {
		t.Fatal(err, errOut.String())
	}
}

func TestInviteSaysWhyThereIsNoLink(t *testing.T) {
	path := cloudProfile(t)
	b, _ := os.ReadFile(path)
	if err := os.WriteFile(path, []byte(strings.Replace(string(b), "https://cb.example.test", "http://cb.example.test", 1)), 0o600); err != nil {
		t.Fatal(err)
	}
	onTerminal(t)
	withAccountRecorder(t, &accountRecorder{data: map[string]any{"code": "CBJ-4K7P-WQ9D-2MXH-T6RB", "email": "ada@example.com", "invite_id": "inv_1"}})
	var out, errOut bytes.Buffer
	err := Run(context.Background(), []string{"device", "add"}, &out, &errOut)
	if err != nil || strings.Contains(out.String(), `"link"`) || !strings.Contains(errOut.String(), "No invite link") || !strings.Contains(errOut.String(), "http://cb.example.test") {
		t.Fatal(err, out.String(), errOut.String())
	}
}
