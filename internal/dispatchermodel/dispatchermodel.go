// Package dispatchermodel is the owner's model setup for the dispatcher
// (DISPATCHER-AGENT-DESIGN §11): the providers the agent can run, the caps,
// the egress disclosure, the setup request and its test result. The CLI and
// the terminal UI both call only this package, so they can't drift (§11.9),
// and the server checks a setup against the same provider list. It has no
// terminal I/O and doesn't import the bus: callers pass an Exec.
package dispatchermodel

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Provider is one entry of the provider list: what the agent's pinned
// build can run with a key (the key-based APIs only, for now).
type Provider struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	API     string `json:"api"`
	BaseURL string `json:"base_url,omitempty"`
	// Custom: the owner gives the base URL (an OpenAI-compatible endpoint).
	Custom  bool   `json:"custom,omitempty"`
	KeyHint string `json:"key_hint"`
}

//go:embed providers.json
var providersJSON []byte

var providers = func() []Provider {
	var file struct {
		Version   int        `json:"version"`
		Providers []Provider `json:"providers"`
	}
	dec := json.NewDecoder(bytes.NewReader(providersJSON))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&file); err != nil || file.Version != 1 || len(file.Providers) == 0 {
		panic("dispatchermodel: providers.json is not valid")
	}
	return file.Providers
}()

// Providers returns the provider list, in its order.
func Providers() []Provider { return append([]Provider(nil), providers...) }

// LookupProvider returns the provider with id.
func LookupProvider(id string) (Provider, bool) {
	for _, p := range providers {
		if p.ID == id {
			return p, true
		}
	}
	return Provider{}, false
}

// CheckProvider reports whether a setup's provider, API and base URL come
// from the list: the API must be the provider's, and a listed base URL must
// be the one given. A custom provider's base URL is checked by the caller's
// own URL rules. The messages are fixed.
func CheckProvider(id, api, baseURL string) error {
	p, ok := LookupProvider(id)
	switch {
	case !ok:
		return errors.New("provider_id must be a provider ID from the providers list")
	case api != p.API:
		return errors.New("api must be the provider's API kind from the providers list")
	case !p.Custom && baseURL != p.BaseURL:
		return fmt.Errorf("base_url must be the provider's own from the providers list: %s", p.BaseURL)
	}
	return nil
}

var modelPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:/@+-]{0,127}$`)

// ValidateModel checks a model ID's shape, and refuses one that looks like a
// key: the model is stored and audited in the clear, so a key pasted at the
// wrong prompt must not get there. Whether the provider has the model, and
// whether it calls tools, is what the setup test proves.
func ValidateModel(model string) error {
	if !modelPattern.MatchString(model) {
		return errors.New("the model must be a model ID, such as the provider lists it")
	}
	if LooksLikeCredential(model) {
		return errors.New("that looks like a key, not a model ID: the key goes only at its hidden prompt, or on stdin")
	}
	return nil
}

// ValidateBaseURL checks a custom provider's base URL: HTTPS, or HTTP to
// this machine only, with no credentials, query or fragment.
func ValidateBaseURL(raw string) error {
	u, err := url.Parse(raw)
	ok := err == nil && u.Host != "" && u.User == nil && u.RawQuery == "" && u.Fragment == "" && !u.ForceQuery
	if ok && u.Scheme != "https" {
		h := u.Hostname()
		ok = u.Scheme == "http" && (h == "localhost" || h == "127.0.0.1" || h == "::1")
	}
	if !ok {
		return errors.New("the base URL must be https://HOST[/PATH] (http only for this machine), with no credentials or query")
	}
	return nil
}

// Caps are the owner's limits (§11.1 step 4), in the server's fields.
type Caps struct {
	RunsPerHour     int   `json:"runs_per_hour"`
	RunsPerDay      int   `json:"runs_per_day"`
	MaxInputTokens  int   `json:"max_input_tokens_per_run"`
	MaxOutputTokens int   `json:"max_output_tokens_per_run"`
	DailyTokens     int64 `json:"daily_tokens"`
}

// DefaultCaps are the defaults. DailyTokens has none: it is the owner's
// bill, so they set it.
func DefaultCaps() Caps {
	return Caps{RunsPerHour: 10, RunsPerDay: 50, MaxInputTokens: 20000, MaxOutputTokens: 2000}
}

// CapField is one cap as the front ends ask for it.
type CapField struct {
	Key, Label string
	Min, Max   int64
}

// CapFields are the caps in the order they are asked.
var CapFields = []CapField{
	{"runs_per_hour", "model runs per hour, per project", 1, 100},
	{"runs_per_day", "model runs per day, per project", 1, 1000},
	{"max_input_tokens_per_run", "input tokens per run", 1000, 200000},
	{"max_output_tokens_per_run", "output tokens per run", 100, 32000},
	{"daily_tokens", "your daily token ceiling, across your projects (required)", 1000, 100_000_000},
}

// Get returns the cap named key.
func (c Caps) Get(key string) int64 {
	switch key {
	case "runs_per_hour":
		return int64(c.RunsPerHour)
	case "runs_per_day":
		return int64(c.RunsPerDay)
	case "max_input_tokens_per_run":
		return int64(c.MaxInputTokens)
	case "max_output_tokens_per_run":
		return int64(c.MaxOutputTokens)
	}
	return c.DailyTokens
}

// Set sets the cap named key to v, within its range.
func (c *Caps) Set(key string, v int64) error {
	for _, f := range CapFields {
		if f.Key != key {
			continue
		}
		if v < f.Min || v > f.Max {
			return fmt.Errorf("%s must be %d–%d", key, f.Min, f.Max)
		}
		switch key {
		case "runs_per_hour":
			c.RunsPerHour = int(v)
		case "runs_per_day":
			c.RunsPerDay = int(v)
		case "max_input_tokens_per_run":
			c.MaxInputTokens = int(v)
		case "max_output_tokens_per_run":
			c.MaxOutputTokens = int(v)
		default:
			c.DailyTokens = v
		}
		return nil
	}
	return fmt.Errorf("%q is not a cap: use %s", key, capKeys())
}

func capKeys() string {
	keys := make([]string, len(CapFields))
	for i, f := range CapFields {
		keys[i] = f.Key
	}
	return strings.Join(keys, ", ")
}

// ParseCaps applies "key=value,key=value" to c.
func (c *Caps) ParseCaps(s string) error {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	for _, part := range strings.Split(s, ",") {
		key, value, ok := strings.Cut(strings.TrimSpace(part), "=")
		n, err := strconv.ParseInt(strings.ReplaceAll(value, "_", ""), 10, 64)
		if !ok || err != nil {
			return fmt.Errorf("caps are key=number pairs, separated by commas: %s", capKeys())
		}
		if err := c.Set(strings.TrimSpace(key), n); err != nil {
			return err
		}
	}
	return nil
}

// Args are the caps as the setup request carries them: a JSON object.
func (c Caps) Args() map[string]any {
	out := map[string]any{}
	for _, f := range CapFields {
		out[f.Key] = c.Get(f.Key)
	}
	return out
}

// Validate checks the caps together: every one in range, the daily ceiling
// set, and no more runs per hour than per day.
func (c Caps) Validate() error {
	for _, f := range CapFields {
		if v := c.Get(f.Key); v < f.Min || v > f.Max {
			if f.Key == "daily_tokens" && v == 0 {
				return errors.New("the daily token ceiling is required: it is your bill (daily_tokens)")
			}
			return fmt.Errorf("%s must be %d–%d", f.Key, f.Min, f.Max)
		}
	}
	if c.RunsPerDay < c.RunsPerHour {
		return errors.New("runs_per_day can't be less than runs_per_hour")
	}
	return nil
}

// DisclosureVersion names the disclosure text below. A changed text gets a
// new version: the server records which one each owner confirmed.
const DisclosureVersion = "v1"

// Disclosure is the egress disclosure (§11.1 step 5) for provider.
func Disclosure(provider Provider) string {
	return "When the model is on, ClawBridge sends issue titles and description excerpts (up to 4 KiB), comment " +
		"excerpts, session names, and your chat questions to " + provider.Name + ", under your key and your account's " +
		"terms with them. It never sends emails (they are replaced by opaque references), database contents, or other " +
		"projects' data. Your key is stored encrypted on the ClawBridge server, which can decrypt it to hand it to " +
		"your agent. The setup test is one request of a few dozen tokens, charged to your key."
}

// Outcomes of a setup test (§11.3), with what the front ends say.
var outcomeText = map[string]string{
	"active":            "The test passed: the model is set up.",
	"invalid_key":       "The provider rejected the key (401). Check it and run setup again.",
	"no_model_access":   "The key works, but this model isn't available to it (403/404). Pick another model or check your provider plan.",
	"quota":             "The provider refused for billing or quota (402, or 429 with a quota error). Check your provider account.",
	"rate_limited":      "The provider rate-limited the test. Try again in a minute.",
	"no_tool_use":       "The model answered but didn't call the tool. The dispatcher needs a model with tool use.",
	"egress_blocked":    "The agent couldn't reach PROVIDER_HOST. The server operator must allow it.",
	"provider_error":    "The provider failed (5xx). Try again later.",
	"timeout":           "No answer from the provider within 30 s.",
	"agent_unavailable": "No agent could start for you (the server's agent cap is reached, or the launcher is down). Ask the operator.",
	"pending":           "The test hasn't finished yet. Check later with: clawbridge dispatcher model status",
}

// OutcomeText says what a test outcome means. An unknown code is shown as
// itself: a fixed code from the server, never provider text.
func OutcomeText(code, baseURL string) string {
	text, ok := outcomeText[code]
	if !ok {
		return "The test failed: " + code + "."
	}
	if u, err := url.Parse(baseURL); err == nil && u.Host != "" {
		text = strings.ReplaceAll(text, "PROVIDER_HOST", u.Host)
	} else {
		text = strings.ReplaceAll(text, "PROVIDER_HOST", "the provider")
	}
	return text
}

// Exec runs one server operation with args and returns its data.
type Exec func(ctx context.Context, op string, args map[string]any) (map[string]any, error)

// Request is one setup.
type Request struct {
	Provider Provider
	// BaseURL is the custom provider's; ignored for a listed one.
	BaseURL string
	Model   string
	// Key is the credential. Setup zeroes it once sent.
	Key  []byte
	Caps Caps
	// EgressConfirmed: the owner confirmed Disclosure(Provider).
	EgressConfirmed bool
}

// errSetupFailed replaces a server error that carried any part of the key.
var errSetupFailed = errors.New("the setup request failed; run setup again")

// carriesKey reports whether text holds any 8-character run of key, so a
// truncated or split echo is caught too (the d4 redaction rule).
func carriesKey(text, key string) bool {
	if len(key) < 8 {
		return key != "" && strings.Contains(text, key)
	}
	for i := 0; i+8 <= len(key); i++ {
		if strings.Contains(text, key[i:i+8]) {
			return true
		}
	}
	return false
}

// Setup sends the setup and returns the config revision whose test is now
// pending. The key goes in this one request and is never in an error. The
// caller's buffer is zeroed afterwards; that is best effort only, since the
// request itself (the args map, its encoding) holds copies until the
// garbage collector reclaims them.
func Setup(ctx context.Context, exec Exec, r Request) (int64, error) {
	defer clear(r.Key)
	key := strings.TrimRight(string(r.Key), "\r\n")
	baseURL := r.Provider.BaseURL
	if r.Provider.Custom {
		baseURL = r.BaseURL
		if err := ValidateBaseURL(baseURL); err != nil {
			return 0, err
		}
	}
	switch {
	case CheckProvider(r.Provider.ID, r.Provider.API, baseURL) != nil:
		return 0, errors.New("pick a provider from the list")
	case ValidateModel(r.Model) != nil:
		return 0, ValidateModel(r.Model)
	case strings.TrimSpace(key) == "":
		return 0, errors.New("the key is empty")
	case len(key) > 16<<10:
		return 0, errors.New("the key is longer than 16 KiB")
	case strings.IndexFunc(key, func(r rune) bool { return r <= ' ' || r == 0x7f }) >= 0:
		// Every listed provider's key is one token: a space or a control
		// character is a paste gone wrong.
		return 0, errors.New("the key has a space or a control character in it; paste it again")
	case !r.EgressConfirmed:
		return 0, errors.New("setup needs the disclosure confirmed")
	}
	if err := r.Caps.Validate(); err != nil {
		return 0, err
	}
	data, err := exec(ctx, "dispatcher_model_setup", map[string]any{
		"provider_id": r.Provider.ID, "api": r.Provider.API, "base_url": baseURL, "model": r.Model, "key": key,
		"caps": r.Caps.Args(), "egress_text_version": DisclosureVersion, "egress_confirmed": true,
	})
	if err != nil {
		if carriesKey(err.Error(), key) {
			return 0, errSetupFailed
		}
		return 0, err
	}
	revision, ok := asInt64(data["config_revision"])
	if !ok || revision < 1 {
		return 0, errors.New("the server's answer had no config revision")
	}
	return revision, nil
}

// WaitForTest polls the setup test of revision every poll, for up to
// limit, and returns its outcome: "active", a failure code, or "pending"
// if it hasn't finished.
func WaitForTest(ctx context.Context, exec Exec, revision int64, poll, limit time.Duration) (string, error) {
	deadline := time.Now().Add(limit)
	for {
		data, err := exec(ctx, "dispatcher_model_status", map[string]any{"config_revision": revision})
		if err != nil {
			return "", err
		}
		outcome, _ := data["test"].(string)
		if outcome == "" {
			return "", errors.New("the server's answer had no test outcome")
		}
		if outcome != "pending" || !time.Now().Add(poll).Before(deadline) {
			return outcome, nil
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(poll):
		}
	}
}

func asInt64(v any) (int64, bool) {
	switch n := v.(type) {
	case int:
		return int64(n), true
	case int64:
		return n, true
	case float64:
		return int64(n), n == float64(int64(n))
	case json.Number:
		i, err := n.Int64()
		return i, err == nil
	}
	return 0, false
}

// credentialPrefixes start well-known provider keys.
var credentialPrefixes = []string{"sk-", "sk_", "AIza", "xai-", "gsk_", "ghp_", "github_pat_", "AKIA", "ya29.", "hf_"}

var uuidPattern = regexp.MustCompile(`(?i)[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}`)

// LooksLikeCredential reports whether a command-line argument, or a model
// ID, looks like a key. Both the whole text and what follows a flag's "="
// are checked, for any of:
//   - a known key prefix;
//   - a UUID, a shape some providers' keys take;
//   - an unbroken run of 24 or more letters and digits mixing both, or of
//     32 or more of either: a key has one, a model ID's words between
//     hyphens are short.
//
// Keys are read only at a hidden prompt or from stdin, so an argument that
// looks like one is refused before anything else.
func LooksLikeCredential(arg string) bool {
	values := []string{arg}
	if _, value, found := strings.Cut(arg, "="); found {
		values = append(values, value)
	}
	for _, value := range values {
		value = strings.TrimLeft(value, "-")
		for _, p := range credentialPrefixes {
			if strings.HasPrefix(value, p) && len(value) >= len(p)+8 {
				return true
			}
		}
		if uuidPattern.MatchString(value) {
			return true
		}
		for _, run := range strings.FieldsFunc(value, func(r rune) bool {
			return !(r >= '0' && r <= '9' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z')
		}) {
			mixed := strings.ContainsAny(run, "0123456789") && strings.IndexFunc(run, func(r rune) bool { return r > '9' }) >= 0
			if len(run) >= 32 || len(run) >= 24 && mixed {
				return true
			}
		}
	}
	return false
}
