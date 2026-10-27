package cli

import (
	"bytes"
	"context"
	"errors"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"clawbridge/internal/bus"
	"clawbridge/internal/dispatchermodel"
)

const modelTestKey = "sk-ant-api03-SECRETSECRETSECRET1234"

// fakeModelServer answers the model setup operations and records them.
type fakeModelServer struct {
	requests []bus.Request
	outcomes []string
	fail     error
}

func (f *fakeModelServer) Execute(_ context.Context, r bus.Request) (bus.Result, error) {
	f.requests = append(f.requests, r)
	if f.fail != nil {
		return bus.Result{}, f.fail
	}
	switch r.Operation {
	case "dispatcher_model_setup":
		return bus.Result{Data: map[string]any{"config_revision": int64(3), "test": "pending"}}, nil
	case "dispatcher_model_status":
		outcome := f.outcomes[0]
		if len(f.outcomes) > 1 {
			f.outcomes = f.outcomes[1:]
		}
		return bus.Result{Data: map[string]any{"config_revision": int64(3), "test": outcome}}, nil
	}
	return bus.Result{}, errors.New("unexpected " + r.Operation)
}

// modelTerminal stands in for the owner's terminal: answers on stdin, the
// key at the hidden prompt, and whether stdin is a terminal.
func modelTerminal(t *testing.T, answers, hidden string, terminal bool) {
	t.Helper()
	oldIn, oldTerm, oldHidden, oldPoll := cloudStdin, stdinTerminal, readHidden, modelTestPoll
	t.Cleanup(func() { cloudStdin, stdinTerminal, readHidden, modelTestPoll = oldIn, oldTerm, oldHidden, oldPoll })
	cloudStdin = strings.NewReader(answers)
	stdinTerminal = func() bool { return terminal }
	readHidden = func() ([]byte, error) { return []byte(hidden), nil }
	modelTestPoll = time.Millisecond
}

func setupArgsFor(r bus.Request) map[string]any {
	a := map[string]any{}
	for k, v := range r.Args {
		a[k] = v
	}
	return a
}

// The interactive flow asks in §11.1's order, reads the key only at the
// hidden prompt, sends the one setup request, waits for the test and says
// what to do next. The key is in no output.
func TestModelSetupInteractive(t *testing.T) {
	modelTerminal(t, "1\nclaude-sonnet-4-5\n\n40\n\n\n500000\nyes\n", modelTestKey, true)
	server := &fakeModelServer{outcomes: []string{"pending", "active"}}
	var out bytes.Buffer
	config := Config{Workspace: "w", Project: "p"}
	if err := runModelSetup(context.Background(), server, config, map[string]any{}, &out); err != nil {
		t.Fatal(err, out.String())
	}
	got := setupArgsFor(server.requests[0])
	caps := dispatchermodel.DefaultCaps()
	caps.RunsPerDay, caps.DailyTokens = 40, 500000
	want := map[string]any{"provider_id": "anthropic", "api": "anthropic-messages", "base_url": "https://api.anthropic.com", "model": "claude-sonnet-4-5",
		"key": modelTestKey, "caps": caps.Args(), "egress_text_version": dispatchermodel.DisclosureVersion, "egress_confirmed": true}
	if !reflect.DeepEqual(got, want) || server.requests[0].Project != "p" {
		t.Fatalf("%v", got)
	}
	ops := []string{}
	for _, r := range server.requests {
		ops = append(ops, r.Operation)
	}
	if !reflect.DeepEqual(ops, []string{"dispatcher_model_setup", "dispatcher_model_status", "dispatcher_model_status"}) {
		t.Fatal(ops)
	}
	text := out.String()
	for _, want := range []string{"1. Anthropic (anthropic)", "Anthropic API key", "(hidden)", "daily token ceiling", "[required]", "never sends emails", "the model is set up", "dispatcher enable --engine model"} {
		if !strings.Contains(text, want) {
			t.Fatalf("no %q in %s", want, text)
		}
	}
	if strings.Contains(text, "SECRET") {
		t.Fatal("the key was printed")
	}
}

// R152-L1: the catalogue's text is escaped before it reaches the terminal.
func TestModelSetupHintsEscaped(t *testing.T) {
	modelTerminal(t, "anthropic\n", modelTestKey, true)
	restore := dispatchermodel.SetModelHintsForTest(map[string][]dispatchermodel.ModelHint{"anthropic": {{ID: "m1", Name: "Evil\x1b[2J", ContextWindow: 1000, InputPerMTok: 1, OutputPerMTok: 1}}})
	defer restore()
	var out bytes.Buffer
	_ = runModelSetup(context.Background(), &fakeModelServer{}, Config{}, map[string]any{}, &out)
	if strings.Contains(out.String(), "\x1b[2J") || !strings.Contains(out.String(), "1. Evil") {
		t.Fatalf("%q", out.String())
	}
}

// Scripted: --key-stdin takes every other answer from flags; a missing one
// stops before the server is asked.
func TestModelSetupScripted(t *testing.T) {
	flags := map[string]any{"provider": "openai-compatible", "base_url": "https://llm.example.test/v1", "model": "local-model", "key_stdin": true,
		"daily_tokens": int64(20000), "caps": "runs_per_hour=4", "confirm_egress": true}
	modelTerminal(t, modelTestKey+"\n", "", false)
	server := &fakeModelServer{outcomes: []string{"no_tool_use"}}
	var out bytes.Buffer
	err := runModelSetup(context.Background(), server, Config{}, flags, &out)
	if err == nil || !strings.Contains(err.Error(), "no_tool_use") || !strings.Contains(out.String(), "didn't call the tool") {
		t.Fatal(err, out.String())
	}
	got := setupArgsFor(server.requests[0])
	if got["key"] != modelTestKey || got["base_url"] != "https://llm.example.test/v1" || got["api"] != "openai-completions" || got["caps"].(map[string]any)["runs_per_hour"] != int64(4) {
		t.Fatalf("%v", got)
	}
	for _, missing := range []string{"provider", "model", "daily_tokens", "confirm_egress", "base_url"} {
		t.Run(missing, func(t *testing.T) {
			a := map[string]any{}
			for k, v := range flags {
				a[k] = v
			}
			delete(a, missing)
			modelTerminal(t, modelTestKey+"\n", "", false)
			server := &fakeModelServer{}
			err := runModelSetup(context.Background(), server, Config{}, a, &bytes.Buffer{})
			if err == nil || len(server.requests) != 0 || strings.Contains(err.Error(), "SECRET") {
				t.Fatal(err, server.requests)
			}
		})
	}
	// A listed provider's base URL is fixed: --base-url with it is refused,
	// not ignored.
	listed := map[string]any{}
	for k, v := range flags {
		listed[k] = v
	}
	listed["provider"] = "anthropic"
	modelTerminal(t, modelTestKey+"\n", "", false)
	server = &fakeModelServer{}
	if err := runModelSetup(context.Background(), server, Config{}, listed, &bytes.Buffer{}); err == nil || !strings.Contains(err.Error(), "only for openai-compatible") || len(server.requests) != 0 {
		t.Fatal(err, server.requests)
	}
	// Without --key-stdin there is no hidden prompt to read from.
	modelTerminal(t, "", "", false)
	if err := runModelSetup(context.Background(), &fakeModelServer{}, Config{}, map[string]any{"provider": "anthropic"}, &bytes.Buffer{}); err == nil || !strings.Contains(err.Error(), "needs a terminal") {
		t.Fatal(err)
	}
}

// Stopping at any step sends nothing; a server error that echoes the key
// is replaced.
func TestModelSetupStopsAndRedacts(t *testing.T) {
	for name, answers := range map[string]string{
		"no provider":   "",
		"bad provider":  "nope\n",
		"no model":      "anthropic\n\n",
		"bad model":     "anthropic\na b\n",
		"bad cap":       "anthropic\nm1\nx\n",
		"no ceiling":    "anthropic\nm1\n\n\n\n\n\n",
		"not confirmed": "anthropic\nm1\n\n\n\n\n9000\nno\n",
	} {
		t.Run(name, func(t *testing.T) {
			modelTerminal(t, answers, modelTestKey, true)
			server := &fakeModelServer{}
			var out bytes.Buffer
			err := runModelSetup(context.Background(), server, Config{}, map[string]any{}, &out)
			if err == nil || len(server.requests) != 0 || strings.Contains(err.Error()+out.String(), "SECRET") {
				t.Fatal(err, server.requests, out.String())
			}
		})
	}
	modelTerminal(t, "anthropic\nm1\n\n\n\n\n9000\nyes\n", modelTestKey, true)
	server := &fakeModelServer{fail: errors.New("invalid: " + modelTestKey)}
	if err := runModelSetup(context.Background(), server, Config{}, map[string]any{}, &bytes.Buffer{}); err == nil || strings.Contains(err.Error(), "SECRET") {
		t.Fatal(err)
	}
}

// Through the command line: a key-looking argument stops everything, key
// delete needs --confirm, status reads doctor, and a server without a seal
// key says setup is unavailable.
func TestCLIDispatcherModel(t *testing.T) {
	dir := isolated(t)
	t.Setenv("CLAWBRIDGE_DB", filepath.Join(dir, "bus.db"))
	invoke(t, "setup", "--local", "--workspace", "w")
	invoke(t, "project", "create", "p")
	t.Setenv("CLAWBRIDGE_PROJECT", "p")
	for _, args := range [][]string{
		{"dispatcher", "model", "setup", "--provider", "anthropic", modelTestKey},
		{"dispatcher", "model", "setup", "--key=" + modelTestKey},
		{"dispatcher", "model", "setup", "--model", "AbCdEfGhIjKlMnOpQrStUvWx1234"},
	} {
		var out, errs bytes.Buffer
		err := Run(context.Background(), args, &out, &errs)
		if !errors.Is(err, errKeyInArgs) || strings.Contains(out.String()+errs.String(), "SECRET") {
			t.Fatal(args, err, out.String(), errs.String())
		}
	}
	rejectCLI(t, "dispatcher", "model", "key", "delete")
	if deleted := invoke(t, "dispatcher", "model", "key", "delete", "--confirm"); deleted["deleted"] != false {
		t.Fatal(deleted)
	}
	if status := invoke(t, "dispatcher", "model", "status"); status["seal_configured"] != false {
		t.Fatal(status)
	}
	modelTerminal(t, modelTestKey, "", false)
	var out, errs bytes.Buffer
	err := Run(context.Background(), []string{"dispatcher", "model", "setup", "--provider", "anthropic", "--model", "claude-sonnet-4-5", "--key-stdin", "--daily-tokens", "500000", "--confirm-egress"}, &out, &errs)
	if err == nil || !strings.Contains(err.Error(), "unavailable") || strings.Contains(err.Error()+out.String()+errs.String(), "SECRET") {
		t.Fatal(err, out.String(), errs.String())
	}
}

// R142-M2: --key-stdin never reads a terminal, where the key would echo.
// R142-M1: a key typed at the (echoing) model prompt is refused before
// anything is sent. R142-L3: a test still pending exits 3, not 0.
func TestModelSetupReviewFixes(t *testing.T) {
	modelTerminal(t, modelTestKey+"\n", "", true)
	server := &fakeModelServer{}
	flags := map[string]any{"provider": "anthropic", "model": "claude-sonnet-4-5", "key_stdin": true, "daily_tokens": int64(20000), "confirm_egress": true}
	if err := runModelSetup(context.Background(), server, Config{}, flags, &bytes.Buffer{}); err == nil || !strings.Contains(err.Error(), "not a terminal") || len(server.requests) != 0 {
		t.Fatal(err, server.requests)
	}
	modelTerminal(t, "anthropic\n"+modelTestKey+"\n", modelTestKey, true)
	var out bytes.Buffer
	if err := runModelSetup(context.Background(), server, Config{}, map[string]any{}, &out); err == nil || !strings.Contains(err.Error(), "looks like a key") || len(server.requests) != 0 || strings.Contains(err.Error(), "SECRET") {
		t.Fatal(err, server.requests)
	}
	modelTerminal(t, modelTestKey+"\n", "", false)
	server = &fakeModelServer{outcomes: []string{"pending"}}
	old := modelTestWait
	modelTestWait = 10 * time.Millisecond
	t.Cleanup(func() { modelTestWait = old })
	err := runModelSetup(context.Background(), server, Config{}, flags, &bytes.Buffer{})
	var exit *ExitCodeError
	if !errors.As(err, &exit) || exit.Code != 3 {
		t.Fatal(err)
	}
}

// rotateServer answers dispatcher_model_config with the owner's stored
// setup, and the setup and status calls like fakeModelServer.
type rotateServer struct {
	fakeModelServer
	active, pending map[string]any
}

func (r *rotateServer) Execute(ctx context.Context, req bus.Request) (bus.Result, error) {
	if req.Operation == "dispatcher_model_config" {
		r.requests = append(r.requests, req)
		var active, pending any
		if r.active != nil {
			active = r.active
		}
		if r.pending != nil {
			pending = r.pending
		}
		return bus.Result{Data: map[string]any{"active": active, "pending": pending}}, nil
	}
	return r.fakeModelServer.Execute(ctx, req)
}

func storedSetup(version string) map[string]any {
	caps := dispatchermodel.DefaultCaps()
	caps.RunsPerHour, caps.DailyTokens = 4, 500000
	return map[string]any{"provider_id": "anthropic", "api": "anthropic-messages", "base_url": "https://api.anthropic.com", "model": "claude-sonnet-4-5",
		"caps": caps.Args(), "egress_text_version": version, "config_revision": int64(5), "created_at": "2026-10-27T10:00:00Z"}
}

// A rotation sends the stored setup unchanged, with only the key new, and
// asks nothing else while the disclosure is the current one.
func TestModelRotate(t *testing.T) {
	const newKey = "sk-ant-api03-NEWNEWNEWNEWNEWNEW5678"
	modelTerminal(t, "", newKey, true)
	server := &rotateServer{fakeModelServer: fakeModelServer{outcomes: []string{"active"}}, active: storedSetup(dispatchermodel.DisclosureVersion)}
	var out bytes.Buffer
	if err := runModelRotate(context.Background(), server, Config{Workspace: "w", Project: "p"}, map[string]any{}, &out); err != nil {
		t.Fatal(err, out.String())
	}
	got := setupRequest(t, server.requests)
	caps := dispatchermodel.DefaultCaps()
	caps.RunsPerHour, caps.DailyTokens = 4, 500000
	want := map[string]any{"provider_id": "anthropic", "api": "anthropic-messages", "base_url": "https://api.anthropic.com", "model": "claude-sonnet-4-5",
		"key": newKey, "caps": caps.Args(), "egress_text_version": dispatchermodel.DisclosureVersion, "egress_confirmed": true}
	if !reflect.DeepEqual(got.Args, want) || got.Project != "p" {
		t.Fatalf("%v", got.Args)
	}
	text := out.String()
	for _, want := range []string{"Rotating the key of your active setup: Anthropic, model claude-sonnet-4-5 (revision 5)", "(hidden)", "the model is set up", "The new key is active"} {
		if !strings.Contains(text, want) {
			t.Fatalf("no %q in %s", want, text)
		}
	}
	if strings.Contains(text, "NEWNEW") || strings.Contains(text, "never sends emails") {
		t.Fatal(text)
	}
	// Only a pending setup: that one is rotated.
	modelTerminal(t, "", newKey, true)
	server = &rotateServer{fakeModelServer: fakeModelServer{outcomes: []string{"active"}}, pending: storedSetup(dispatchermodel.DisclosureVersion)}
	out.Reset()
	if err := runModelRotate(context.Background(), server, Config{}, map[string]any{}, &out); err != nil || !strings.Contains(out.String(), "your setup whose test is pending") {
		t.Fatal(err, out.String())
	}
}

// An older disclosure is confirmed again, by "yes" or --confirm-egress;
// without it, or with no setup, or a provider no longer offered, nothing is
// sent.
func TestModelRotateRefusals(t *testing.T) {
	const newKey = "sk-ant-api03-NEWNEWNEWNEWNEWNEW5678"
	old := storedSetup("v0")
	run := func(t *testing.T, server *rotateServer, a map[string]any, answers string, terminal bool) (string, error) {
		t.Helper()
		hidden := newKey
		if !terminal {
			answers, hidden = newKey+"\n", ""
		}
		modelTerminal(t, answers, hidden, terminal)
		var out bytes.Buffer
		err := runModelRotate(context.Background(), server, Config{}, a, &out)
		if err != nil && strings.Contains(err.Error(), "NEWNEW") || strings.Contains(out.String(), "NEWNEW") {
			t.Fatal("the key was printed")
		}
		return out.String(), err
	}
	sent := func(s *rotateServer) bool {
		for _, r := range s.requests {
			if r.Operation == "dispatcher_model_setup" {
				return true
			}
		}
		return false
	}
	s := &rotateServer{fakeModelServer: fakeModelServer{outcomes: []string{"active"}}, active: old}
	if out, err := run(t, s, map[string]any{}, "yes\n", true); err != nil || !sent(s) || !strings.Contains(out, "never sends emails") {
		t.Fatal(err, out)
	}
	s = &rotateServer{fakeModelServer: fakeModelServer{outcomes: []string{"active"}}, active: old}
	if _, err := run(t, s, map[string]any{}, "no\n", true); err == nil || !strings.Contains(err.Error(), "not confirmed") || sent(s) {
		t.Fatal(err)
	}
	s = &rotateServer{fakeModelServer: fakeModelServer{outcomes: []string{"active"}}, active: old}
	if _, err := run(t, s, map[string]any{"key_stdin": true}, "", false); err == nil || !strings.Contains(err.Error(), "--confirm-egress is required") || sent(s) {
		t.Fatal(err)
	}
	s = &rotateServer{fakeModelServer: fakeModelServer{outcomes: []string{"active"}}, active: old}
	if _, err := run(t, s, map[string]any{"key_stdin": true, "confirm_egress": true}, "", false); err != nil || !sent(s) {
		t.Fatal(err)
	}
	s = &rotateServer{}
	if _, err := run(t, s, map[string]any{}, "", true); err == nil || !strings.Contains(err.Error(), "dispatcher model setup") || sent(s) {
		t.Fatal(err)
	}
	gone := storedSetup(dispatchermodel.DisclosureVersion)
	gone["provider_id"] = "retired"
	s = &rotateServer{active: gone}
	if _, err := run(t, s, map[string]any{}, "", true); err == nil || !strings.Contains(err.Error(), "no longer offered") || sent(s) {
		t.Fatal(err)
	}
	// Through the command line: a key-looking argument stops it first.
	var outb, errs bytes.Buffer
	if err := Run(context.Background(), []string{"dispatcher", "model", "key", "rotate", newKey}, &outb, &errs); !errors.Is(err, errKeyInArgs) {
		t.Fatal(err)
	}
}
