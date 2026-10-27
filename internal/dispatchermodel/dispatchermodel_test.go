package dispatchermodel

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"time"
)

// The server's own shape rules (internal/bus/dispatcher_model.go): every
// listed provider passes them.
var (
	serverProviderID = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,63}$`)
	serverBaseURL    = regexp.MustCompile(`^(https://[^/?#@\s]+|http://(localhost|127\.0\.0\.1|\[::1\])(:[0-9]{1,5})?)(/[^?#\s]*)?$`)
	agentAPI         = regexp.MustCompile(`(?m)^\t"([a-z0-9-]+)": async \(\) =>`)
)

// agentAPIs are the APIs the agent's build runs, read from its source
// (dispatcher-agent/src/provider.ts, APIS), so the two lists can't drift
// between CI jobs; the agent's own test checks the list against
// SUPPORTED_APIS too.
func agentAPIs(t *testing.T) map[string]bool {
	t.Helper()
	src, err := os.ReadFile(filepath.Join("..", "..", "dispatcher-agent", "src", "provider.ts"))
	if err != nil {
		t.Fatal(err)
	}
	block, _, _ := strings.Cut(string(src)[strings.Index(string(src), "const APIS"):], "\n};")
	apis := map[string]bool{}
	for _, m := range agentAPI.FindAllStringSubmatch(block, -1) {
		apis[m[1]] = true
	}
	if len(apis) < 3 {
		t.Fatalf("read %d APIs from provider.ts: has its APIS table changed shape?", len(apis))
	}
	return apis
}

func TestProviders(t *testing.T) {
	agentAPIs := agentAPIs(t)
	seen := map[string]bool{}
	for _, p := range Providers() {
		if seen[p.ID] || !serverProviderID.MatchString(p.ID) || !serverProviderID.MatchString(p.API) || !agentAPIs[p.API] || p.Name == "" || p.KeyHint == "" {
			t.Fatalf("%+v", p)
		}
		seen[p.ID] = true
		if p.Custom != (p.BaseURL == "") || !p.Custom && (!serverBaseURL.MatchString(p.BaseURL) || strings.HasSuffix(p.BaseURL, "/") || !strings.HasPrefix(p.BaseURL, "https://")) {
			t.Fatalf("base URL of %+v", p)
		}
	}
	// The bus and server tests set up exactly this one.
	if p, ok := LookupProvider("anthropic"); !ok || p.API != "anthropic-messages" || p.BaseURL != "https://api.anthropic.com" {
		t.Fatalf("%+v", p)
	}
	if err := CheckProvider("anthropic", "anthropic-messages", "https://api.anthropic.com"); err != nil {
		t.Fatal(err)
	}
	for _, c := range [][3]string{
		{"nope", "anthropic-messages", "https://api.anthropic.com"},
		{"anthropic", "openai-completions", "https://api.anthropic.com"},
		{"anthropic", "anthropic-messages", "https://evil.test"},
		{"anthropic", "anthropic-messages", "https://api.anthropic.com/"},
		{"openai-compatible", "anthropic-messages", "https://llm.example.test/v1"},
	} {
		if CheckProvider(c[0], c[1], c[2]) == nil {
			t.Fatalf("%v accepted", c)
		}
	}
	if err := CheckProvider("openai-compatible", "openai-completions", "https://llm.example.test/v1"); err != nil {
		t.Fatal(err)
	}
	// The list is a copy.
	Providers()[0].ID = "changed"
	if Providers()[0].ID == "changed" {
		t.Fatal("Providers returned the list itself")
	}
}

func TestCaps(t *testing.T) {
	c := DefaultCaps()
	if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "required") {
		t.Fatalf("no daily ceiling: %v", err)
	}
	if err := c.ParseCaps("daily_tokens=500_000, runs_per_hour=5"); err != nil || c.DailyTokens != 500000 || c.RunsPerHour != 5 || c.Validate() != nil {
		t.Fatalf("%+v %v", c, err)
	}
	for _, bad := range []string{"daily_tokens", "nope=3", "runs_per_hour=0", "runs_per_hour=101", "daily_tokens=999", "max_output_tokens_per_run=x"} {
		c := DefaultCaps()
		if c.ParseCaps(bad) == nil {
			t.Fatalf("%q accepted", bad)
		}
	}
	c = DefaultCaps()
	_ = c.ParseCaps("daily_tokens=1000,runs_per_hour=60,runs_per_day=50")
	if c.Validate() == nil {
		t.Fatal("more runs an hour than a day accepted")
	}
	// The fields the server reads.
	raw, _ := json.Marshal(DefaultCaps())
	var fields map[string]any
	_ = json.Unmarshal(raw, &fields)
	for _, f := range CapFields {
		if _, ok := fields[f.Key]; !ok {
			t.Fatalf("%s not sent", f.Key)
		}
	}
}

func TestValidate(t *testing.T) {
	for _, m := range []string{"claude-sonnet-4-5", "gpt-4.1", "meta-llama/Llama-3.3-70B-Instruct-Turbo", "models/gemini-2.5-pro"} {
		if ValidateModel(m) != nil {
			t.Fatal(m)
		}
	}
	for _, m := range []string{"", "a b", "-x", strings.Repeat("a", 129), "x\n"} {
		if ValidateModel(m) == nil {
			t.Fatalf("%q", m)
		}
	}
	for _, u := range []string{"https://llm.example.test/v1", "http://localhost:8080/v1", "http://127.0.0.1:1234"} {
		if ValidateBaseURL(u) != nil {
			t.Fatal(u)
		}
	}
	for _, u := range []string{"http://llm.example.test/v1", "https://u:p@llm.example.test", "https://llm.example.test/v1?k=1", "ftp://x", "https://", "llm.example.test"} {
		if ValidateBaseURL(u) == nil {
			t.Fatal(u)
		}
	}
}

// fakeServer records each request and answers from a script.
type fakeServer struct {
	requests []map[string]any
	ops      []string
	answer   func(op string, args map[string]any) (map[string]any, error)
}

func (f *fakeServer) exec(_ context.Context, op string, args map[string]any) (map[string]any, error) {
	f.ops = append(f.ops, op)
	f.requests = append(f.requests, args)
	return f.answer(op, args)
}

const testKey = "sk-ant-api03-SECRETSECRETSECRET1234"

func setupRequest() Request {
	p, _ := LookupProvider("anthropic")
	caps := DefaultCaps()
	caps.DailyTokens = 500000
	return Request{Provider: p, Model: "claude-sonnet-4-5", Key: []byte(testKey + "\n"), Caps: caps, EgressConfirmed: true}
}

// Setup sends exactly the server's fields, with the disclosure version,
// zeroes the key afterwards, and never returns an error that carries it.
func TestSetup(t *testing.T) {
	f := &fakeServer{answer: func(string, map[string]any) (map[string]any, error) {
		return map[string]any{"config_revision": float64(7), "test": "pending"}, nil
	}}
	r := setupRequest()
	revision, err := Setup(context.Background(), f.exec, r)
	if err != nil || revision != 7 {
		t.Fatal(revision, err)
	}
	want := map[string]any{"provider_id": "anthropic", "api": "anthropic-messages", "base_url": "https://api.anthropic.com", "model": "claude-sonnet-4-5",
		"key": testKey, "caps": r.Caps.Args(), "egress_text_version": "v1", "egress_confirmed": true}
	if !reflect.DeepEqual(f.requests[0], want) || f.ops[0] != "dispatcher_model_setup" {
		t.Fatalf("%v", f.requests[0])
	}
	if strings.Trim(string(r.Key), "\x00") != "" {
		t.Fatal("the key was not zeroed")
	}
	// A server error that echoes the key, whole, truncated or split, is
	// replaced.
	for _, echo := range []string{testKey, testKey[:12], testKey[10:20] + "…"} {
		f.answer = func(string, map[string]any) (map[string]any, error) {
			return nil, errors.New("bad request: key " + echo + " refused")
		}
		if _, err := Setup(context.Background(), f.exec, setupRequest()); !errors.Is(err, errSetupFailed) {
			t.Fatalf("%q: %v", echo, err)
		}
	}
	// Nothing is sent without a confirmed disclosure, a daily ceiling, a key,
	// or a listed provider.
	f.requests = nil
	for name, edit := range map[string]func(*Request){
		"unconfirmed": func(r *Request) { r.EgressConfirmed = false },
		"no ceiling":  func(r *Request) { r.Caps.DailyTokens = 0 },
		"no key":      func(r *Request) { r.Key = []byte("\n") },
		"space":       func(r *Request) { r.Key = []byte("sk-ant one two\n") },
		"control":     func(r *Request) { r.Key = []byte("sk-ant-\x1bone\n") },
		"bad model":   func(r *Request) { r.Model = "a b" },
		"unlisted":    func(r *Request) { r.Provider.ID = "nope" },
		"wrong url":   func(r *Request) { r.Provider.BaseURL = "https://evil.test" },
		"bad custom": func(r *Request) {
			r.Provider, _ = LookupProvider("openai-compatible")
			r.BaseURL = "http://llm.example.test"
		},
	} {
		r := setupRequest()
		edit(&r)
		if _, err := Setup(context.Background(), f.exec, r); err == nil || strings.Contains(err.Error(), "SECRET") {
			t.Fatalf("%s: %v", name, err)
		}
		if strings.Trim(string(r.Key), "\x00\n") != "" {
			t.Fatalf("%s: the key was not zeroed", name)
		}
	}
	if len(f.requests) != 0 {
		t.Fatalf("sent %v", f.requests)
	}
	// A custom provider sends its own base URL.
	f.answer = func(string, map[string]any) (map[string]any, error) { return map[string]any{"config_revision": 8}, nil }
	r = setupRequest()
	r.Provider, _ = LookupProvider("openai-compatible")
	r.BaseURL = "https://llm.example.test/v1"
	if _, err := Setup(context.Background(), f.exec, r); err != nil || f.requests[0]["base_url"] != "https://llm.example.test/v1" || f.requests[0]["api"] != "openai-completions" {
		t.Fatal(err, f.requests)
	}
}

func TestWaitForTest(t *testing.T) {
	answers := []string{"pending", "pending", "no_tool_use"}
	f := &fakeServer{answer: func(_ string, args map[string]any) (map[string]any, error) {
		next := answers[0]
		if len(answers) > 1 {
			answers = answers[1:]
		}
		return map[string]any{"config_revision": args["config_revision"], "test": next}, nil
	}}
	outcome, err := WaitForTest(context.Background(), f.exec, 7, time.Millisecond, time.Second)
	if err != nil || outcome != "no_tool_use" || len(f.ops) != 3 || f.ops[0] != "dispatcher_model_status" || f.requests[0]["config_revision"] != int64(7) {
		t.Fatal(outcome, err, f.ops)
	}
	// Still pending at the limit: say so, don't wait on.
	answers = []string{"pending"}
	outcome, err = WaitForTest(context.Background(), f.exec, 7, 20*time.Millisecond, 50*time.Millisecond)
	if err != nil || outcome != "pending" {
		t.Fatal(outcome, err)
	}
}

func TestOutcomeText(t *testing.T) {
	for _, code := range []string{"active", "invalid_key", "no_model_access", "quota", "rate_limited", "no_tool_use", "egress_blocked", "provider_error", "timeout", "agent_unavailable", "pending"} {
		if text := OutcomeText(code, "https://api.anthropic.com"); text == "" || strings.Contains(text, "PROVIDER_HOST") || strings.HasPrefix(text, "The test failed: ") {
			t.Fatalf("%s: %q", code, text)
		}
	}
	if !strings.Contains(OutcomeText("egress_blocked", "https://api.anthropic.com"), "api.anthropic.com") {
		t.Fatal("egress_blocked doesn't name the host")
	}
	if OutcomeText("new_code", "") != "The test failed: new_code." {
		t.Fatal(OutcomeText("new_code", ""))
	}
}

func TestDisclosure(t *testing.T) {
	p, _ := LookupProvider("openai")
	text := Disclosure(p)
	for _, want := range []string{"OpenAI", "4 KiB", "chat questions", "never sends emails", "encrypted", "charged to your key"} {
		if !strings.Contains(text, want) {
			t.Fatalf("no %q in %s", want, text)
		}
	}
	if DisclosureVersion != "v1" {
		t.Fatal("a changed disclosure text needs a new version, and the server records it")
	}
}

func TestLooksLikeCredential(t *testing.T) {
	for _, arg := range []string{testKey, "--key=" + testKey, "AIzaSyD-abcdefghijklmnop", "gsk_abcdefghijklmnop", "3f8a9c2b7d6e1f0a4b5c6d7e8f9a0b1c", "AbCdEfGhIjKlMnOpQrStUvWx12",
		// R142-M1: base64 padding, a UUID, a long run of letters only.
		"sk-AbCdEfGhIjKlMnOp==", "--model=sk-AbCdEfGhIjKlMnOp==", "123e4567-e89b-12d3-a456-426614174000", "abcdefghijklmnopqrstuvwxyzabcdefgh"} {
		if !LooksLikeCredential(arg) {
			t.Fatalf("%q", arg)
		}
		if ValidateModel(strings.TrimPrefix(arg, "--model=")) == nil {
			t.Fatalf("a key as the model: %q", arg)
		}
	}
	for _, arg := range []string{"anthropic", "--model=claude-sonnet-4-5-20250929-thinking-extended", "meta-llama/Llama-3.3-70B-Instruct-Turbo", "https://api.groq.com/openai/v1", "daily_tokens=500000,runs_per_hour=10", "--confirm-egress", "openai-compatible", "sk-"} {
		if LooksLikeCredential(arg) {
			t.Fatalf("%q", arg)
		}
	}
}
