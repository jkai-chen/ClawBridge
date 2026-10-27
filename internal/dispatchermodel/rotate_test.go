package dispatchermodel

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func configExec(answer map[string]any) Exec {
	return func(_ context.Context, op string, _ map[string]any) (map[string]any, error) {
		if op != "dispatcher_model_config" {
			panic(op)
		}
		return answer, nil
	}
}

// The active setup is read, or else the pending one, from a local store's
// Go values and from a remote answer's decoded JSON alike.
func TestCurrentConfig(t *testing.T) {
	caps := DefaultCaps()
	caps.DailyTokens = 500000
	stored := map[string]any{"provider_id": "openai-compatible", "api": "openai-completions", "base_url": "https://llm.example.test/v1",
		"model": "m1", "caps": caps.Args(), "egress_text_version": "v1", "config_revision": int64(7), "created_at": "2026-10-27T10:00:00Z"}
	var remote map[string]any
	b, _ := json.Marshal(stored)
	_ = json.Unmarshal(b, &remote)
	for name, answer := range map[string]map[string]any{
		"local active":  {"active": stored, "pending": nil},
		"remote active": {"active": remote, "pending": nil},
		"pending only":  {"active": nil, "pending": stored},
	} {
		cfg, pending, err := CurrentConfig(context.Background(), configExec(answer))
		if err != nil || cfg.Model != "m1" || cfg.Caps != caps || cfg.ConfigRevision != 7 || cfg.BaseURL != "https://llm.example.test/v1" || pending != (name == "pending only") {
			t.Fatalf("%s: %+v %v %v", name, cfg, pending, err)
		}
	}
	if _, _, err := CurrentConfig(context.Background(), configExec(map[string]any{"active": nil, "pending": nil})); err == nil || !strings.Contains(err.Error(), "dispatcher model setup") {
		t.Fatal(err)
	}
	if _, _, err := CurrentConfig(context.Background(), configExec(map[string]any{"active": map[string]any{"model": "m1"}})); err == nil {
		t.Fatal("a setup without its provider")
	}
}

// A rotation keeps everything but the key, and asks for the disclosure
// again only for an older text; a provider no longer offered as it was
// stops it.
func TestPlanRotation(t *testing.T) {
	cfg := Config{ProviderID: "openai-compatible", API: "openai-completions", BaseURL: "https://llm.example.test/v1", Model: "m1", Caps: DefaultCaps(), EgressTextVersion: DisclosureVersion}
	r, err := PlanRotation(cfg)
	if err != nil || r.Disclose {
		t.Fatal(r, err)
	}
	req := r.Request([]byte("k"))
	if req.Provider.ID != "openai-compatible" || req.BaseURL != cfg.BaseURL || req.Model != "m1" || string(req.Key) != "k" || !req.EgressConfirmed {
		t.Fatalf("%+v", req)
	}
	cfg.EgressTextVersion = "v0"
	if r, _ := PlanRotation(cfg); !r.Disclose {
		t.Fatal("an older disclosure")
	}
	for _, bad := range []Config{{ProviderID: "retired", API: "openai-completions"}, {ProviderID: "anthropic", API: "openai-completions"}} {
		if _, err := PlanRotation(bad); err == nil || !strings.Contains(err.Error(), "no longer offered") {
			t.Fatal(bad, err)
		}
	}
}
