package cli

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"clawbridge/internal/clirender"
	"clawbridge/internal/install"
	"github.com/muesli/cancelreader"
)

// TUI-UX-AUDIT PR F: onboarding continuity.

// U45: a wrong answer is asked again with the reason, and the flow goes on;
// the reason never repeats the answer.
func TestStartAsksAgainAfterAWrongAnswer(t *testing.T) {
	isolated(t)
	clearRouteEnv(t)
	wizardHooks(t, "4\nlocal\n1\ndemo\nMy Project\nfirst\nn\nn\n")
	var out bytes.Buffer
	if err := Run(context.Background(), []string{"onboard"}, &out, io.Discard); err != nil {
		t.Fatal(err, out.String())
	}
	got := out.String()
	for _, want := range []string{"choose 1, 2 or 3. Try again.", "a project name is lowercase letters, digits and dashes. Try again.", "Workspace demo and project first are ready."} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %q:\n%s", want, got)
		}
	}
	if strings.Count(got, "Choose 1, 2 or 3: ") != 3 || strings.Contains(got, "My Project.") {
		t.Fatal(got)
	}
}

// U48: the local path ends by asking to open ClawBridge; Enter opens it, n
// leaves it closed and says how to open it later.
func TestStartLocalAsksToOpenClawBridge(t *testing.T) {
	for answer, opens := range map[string]bool{"\n": true, "y\n": true, "n\n": false, "": false} {
		isolated(t)
		clearRouteEnv(t)
		r := wizardHooks(t, "1\ndemo\nfirst\nn\n"+answer)
		// Each prompt opened is open until closed.
		open := wizardInput
		wizardInput = func() (cancelreader.CancelReader, error) { r.closed = false; return open() }
		opened := false
		wizardTUI = func(_ context.Context, c Config, _, _ io.Writer) error {
			if c.ClientConfig != clientPath(t) || !r.closed {
				t.Fatal("selector lost, or the prompt still holds the terminal")
			}
			opened = true
			return nil
		}
		var out bytes.Buffer
		if err := Run(context.Background(), []string{"onboard"}, &out, io.Discard); err != nil {
			t.Fatal(err, out.String())
		}
		if opened != opens || !strings.Contains(out.String(), "Open ClawBridge now? [Y/n]") || strings.Contains(out.String(), "Run clawbridge any time") == opens {
			t.Fatalf("%q: opened %v\n%s", answer, opened, out.String())
		}
	}
}

// U46: the join wizard checks each answer when it is given, so an empty URL
// or workspace is asked for again, not reported after every question.
func TestWizardChecksEachAnswer(t *testing.T) {
	isolated(t)
	wizardHooks(t, "\nhttps://service.test\n\nws\nprj\nsomething\n2\nn\n")
	var out bytes.Buffer
	if err := Run(context.Background(), []string{"setup", "wizard"}, &out, io.Discard); err != nil {
		t.Fatal(err, out.String())
	}
	for _, want := range []string{"the service URL is required", "the workspace is required", "choose 1 or 2. Try again.",
		"2. show a code to enter on another device", "Sign-in: a code on another device", "Canceled; no credentials"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("missing %q:\n%s", want, out.String())
		}
	}
	// Three wrong answers stop the flow with the reason.
	isolated(t)
	wizardHooks(t, "\n\n\n")
	if err := Run(context.Background(), []string{"setup", "wizard"}, io.Discard, io.Discard); err == nil || !strings.Contains(err.Error(), "the service URL is required, like https://clawbridge.example.com; run clawbridge onboard to start again") {
		t.Fatal(err)
	}
}

// U47: a member who joined before any project was granted runs bare
// clawbridge: it opens the TUI, which waits for a project (PR D), rather
// than calling the profile incomplete.
func TestStartupOpensTheTUIWithoutAProject(t *testing.T) {
	isolated(t)
	wizardHooks(t, "")
	path := clientPath(t)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := writeConfigValues(path, map[string]any{"mode": "cloud", "server": "https://service.test", "workspace": "pilot"}); err != nil {
		t.Fatal(err)
	}
	opened := false
	wizardTUI = func(_ context.Context, c Config, _, _ io.Writer) error { opened = c.ClientConfig == path; return nil }
	if err := Run(context.Background(), nil, io.Discard, io.Discard); err != nil || !opened {
		t.Fatal(err, opened)
	}
}

// U53: a broken profile says what is wrong and how to start again.
func TestStartupNamesWhatIsWrongWithTheProfile(t *testing.T) {
	for content, want := range map[string]string{
		"mode = \"cloud\"\n":  "are incomplete (missing: workspace, project). Move the file aside and run clawbridge onboard",
		"mode = \n":           "are damaged and can't be read. Repair the file, or move it aside and run clawbridge onboard",
		"workspace = \"w\"\n": "are incomplete (missing: mode, project)",
	} {
		isolated(t)
		wizardHooks(t, "")
		path := clientPath(t)
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		err := Run(context.Background(), nil, io.Discard, io.Discard)
		if err == nil || !strings.Contains(err.Error(), "your ClawBridge settings at "+path) || !strings.Contains(err.Error(), want) || strings.Contains(err.Error(), "trusted config") {
			t.Fatalf("%q: %v", content, err)
		}
	}
}

// U48: on Google Cloud the owner signs in by hand; onboard then resumes at
// sign-in, finds the workspace, and goes on to the first project.
func TestHandoverResumesToTheFirstProject(t *testing.T) {
	isolated(t)
	s := onboardingState{Cloud: "gcp", Step: "signin", Account: "o@example.test", Project: "example-project", Region: "europe-west1",
		Zone: "europe-west1-b", Domain: "bridge.example.test", Owner: "o@example.test", Size: "e2-small", Bucket: "example-backups"}
	if err := saveOnboarding(&s); err != nil {
		t.Fatal(err)
	}
	// Not signed in yet: the written step, and how to come back.
	wizardHooks(t, "3\n\n")
	stubHandover(t)
	made := projectStub(t)
	var out bytes.Buffer
	if err := Run(context.Background(), []string{"onboard"}, &out, io.Discard); err != nil || !strings.Contains(out.String(), "run clawbridge onboard again: it goes on to your first project") || len(*made) != 0 {
		t.Fatal(err, out.String())
	}
	// A profile for another server is not this sign-in.
	if err := os.MkdirAll(filepath.Dir(clientPath(t)), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := writeConfigValues(clientPath(t), map[string]any{"mode": "cloud", "server": "https://other.example.test", "workspace": "acme"}); err != nil {
		t.Fatal(err)
	}
	wizardHooks(t, "3\n\n")
	out.Reset()
	if err := Run(context.Background(), []string{"onboard"}, &out, io.Discard); err != nil || !strings.Contains(out.String(), "references/signin.md") || len(*made) != 0 {
		t.Fatal(err, out.String())
	}
	// Signed in: the first project, then the offer to open.
	if err := os.MkdirAll(filepath.Dir(clientPath(t)), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := writeConfigValues(clientPath(t), map[string]any{"mode": "cloud", "server": "https://bridge.example.test", "workspace": "acme"}); err != nil {
		t.Fatal(err)
	}
	wizardHooks(t, "3\n\nweb\n\n")
	opened := false
	wizardTUI = func(context.Context, Config, io.Writer, io.Writer) error { opened = true; return nil }
	out.Reset()
	if err := Run(context.Background(), []string{"onboard"}, &out, io.Discard); err != nil || !opened || strings.Join(*made, ",") != "web" || profile(t, clientPath(t))["project"] != "web" || strings.Contains(out.String(), "references/signin.md") {
		t.Fatal(err, *made, out.String())
	}
}

// U52: each optional step says where it is in the five.
func TestOnboardingCountsItsSteps(t *testing.T) {
	o := newOnboardingRun(t, "n\nn\nn\nn\n\n")
	if err := o.run(); err != nil {
		t.Fatal(err)
	}
	last := -1
	for i, name := range []string{"Agent skill", "Linear", "MCP", "Doctor", "Dispatcher"} {
		at := strings.Index(o.out.String(), "Optional setup "+string(rune('1'+i))+" of 5 · "+name+"\n")
		if at <= last {
			t.Fatalf("%s:\n%s", name, o.out.String())
		}
		last = at
	}
}

// U51: the MCP entry and the dispatcher preview read as text, not JSON,
// with no colour and every value escaped.
func TestOnboardingPreviewsAreText(t *testing.T) {
	t.Setenv("NO_COLOR", "1")
	var out bytes.Buffer
	entry := install.Result{Path: "/home/a/.claude.json", Changed: true, Entry: map[string]any{"command": "clawbridge", "args": []any{"mcp", "--session", "eng\x1b[2J"}, "type": "stdio"}}
	if err := clirender.Write(&out, clirender.Human, "onboarding_mcp_entry", entry); err != nil {
		t.Fatal(err)
	}
	preview := map[string]any{"preview": true, "mode": "rules", "coordinator_before": nil, "coordinator_after": "dispatcher",
		"sessions": []any{"dispatcher", "dispatcher-model"}, "domains": []any{"default"}, "default_domain_created": true, "auto_confirm_domains": []any{}}
	if err := clirender.Write(&out, clirender.Human, "onboarding_dispatcher_preview", preview); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	for _, want := range []string{"File", "/home/a/.claude.json", "Arguments", `mcp --session eng\x1b[2J`, "Type", "stdio",
		"Coordinator", "none → dispatcher", "Domains", "default (created now", "Confirms claims in", "none yet"} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %q:\n%s", want, got)
		}
	}
	if strings.ContainsAny(got, "{}\x1b") {
		t.Fatalf("JSON or an escape:\n%q", got)
	}
}

// Each own-server input is checked by itself: a zone letter is lowercase.
func TestOwnServerZoneLetter(t *testing.T) {
	base := onboardingState{Cloud: "aws", Profile: "clawbridge-onboarding", Account: "123456789012", Region: "eu-west-1",
		Domain: "bridge.example.test", Owner: "o@example.test", Size: "t4g.small", Bucket: "clawbridge-backups-1"}
	for zone, ok := range map[string]bool{"eu-west-1a": true, "eu-west-1z": true, "eu-west-1A": false, "eu-west-1{": false, "eu-west-1": false} {
		s := base
		s.Zone = zone
		if err := validInputs(&s); (err == nil) != ok {
			t.Fatalf("%s: %v", zone, err)
		}
	}
}

// The invite's email is asked again until it is one.
func TestWizardInviteAsksForTheEmailAgain(t *testing.T) {
	w := newInviteWizard(t, "https://service.test\n1\nada\nada@example.com\n\nn\n", "google", "device")
	out, err := w.run()
	if err != nil || !strings.Contains(out, "that isn't an email address; leave it empty if you have an invite link. Try again.") || !strings.Contains(out, "Email: ada@example.com") {
		t.Fatal(err, out)
	}
}

// Only enable's preview gets the preview's layout.
func TestDispatcherPreviewViewTakesOnlyAPreview(t *testing.T) {
	if _, ok := dispatcherPreviewView(map[string]any{"enabled": true, "mode": "rules"}); ok {
		t.Fatal("took an enable result for a preview")
	}
	if _, ok := mcpEntryView(install.Result{Path: "/x"}); ok {
		t.Fatal("took a result without an entry")
	}
}

// Each numbered question's words name one choice, in lower case, since pick
// lowers the answer and walks a map.
func TestPickWordsAreUnique(t *testing.T) {
	for name, choices := range map[string]map[string][]string{"cloud": cloudChoices, "join": joinChoices, "sign-in": signInChoices} {
		seen := map[string]string{}
		for value, words := range choices {
			for _, w := range words {
				if w != strings.ToLower(w) {
					t.Fatalf("%s: %q isn't lower case", name, w)
				}
				if seen[w] != "" {
					t.Fatalf("%s: %q means %s and %s", name, w, seen[w], value)
				}
				seen[w] = value
				if got, ok := pick(strings.ToUpper(w), "", choices); !ok || got != value {
					t.Fatalf("%s: %q picked %q", name, w, got)
				}
			}
		}
	}
}
