package dispatchermodel

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
)

// Config is an owner's stored model setup, as dispatcher_model_config
// returns it: everything but the key.
type Config struct {
	ProviderID        string `json:"provider_id"`
	API               string `json:"api"`
	BaseURL           string `json:"base_url"`
	Model             string `json:"model"`
	Caps              Caps   `json:"caps"`
	EgressTextVersion string `json:"egress_text_version"`
	ConfigRevision    int64  `json:"config_revision"`
	CreatedAt         string `json:"created_at"`
}

// errNoSetup: there is nothing to rotate.
var errNoSetup = errors.New("you have no model setup to rotate the key of: run clawbridge dispatcher model setup")

// CurrentConfig reads the caller's own setup (§11.6): the active one, or
// the one whose test is pending when none is active. pending says which.
func CurrentConfig(ctx context.Context, exec Exec) (cfg Config, pending bool, err error) {
	data, err := exec(ctx, "dispatcher_model_config", map[string]any{})
	if err != nil {
		return Config{}, false, err
	}
	raw, pending := data["active"], false
	if raw == nil {
		raw, pending = data["pending"], true
	}
	if raw == nil {
		return Config{}, false, errNoSetup
	}
	// A local store answers with Go values, a remote one with decoded JSON:
	// one round trip reads both.
	b, err := json.Marshal(raw)
	if err != nil || json.Unmarshal(b, &cfg) != nil || cfg.ProviderID == "" || cfg.Model == "" {
		return Config{}, false, errors.New("the server's answer had no readable model setup")
	}
	return cfg, pending, nil
}

// Rotation is a key rotation: the stored setup, unchanged, with a new key.
type Rotation struct {
	Config   Config
	Provider Provider
	// Disclose: the stored disclosure is an older text, so the owner
	// confirms the current one again.
	Disclose bool
}

// PlanRotation checks that the stored setup can still be sent as it is:
// its provider must still be on the list, with the same API.
func PlanRotation(cfg Config) (Rotation, error) {
	p, ok := LookupProvider(cfg.ProviderID)
	if !ok || p.API != cfg.API {
		return Rotation{}, fmt.Errorf("your setup's provider %q is no longer offered as it was: run clawbridge dispatcher model setup", cfg.ProviderID)
	}
	return Rotation{Config: cfg, Provider: p, Disclose: cfg.EgressTextVersion != DisclosureVersion}, nil
}

// Request is the setup request a rotation sends, with key.
func (r Rotation) Request(key []byte) Request {
	return Request{Provider: r.Provider, BaseURL: r.Config.BaseURL, Model: r.Config.Model, Key: key, Caps: r.Config.Caps, EgressConfirmed: true}
}

// Describe says what is being rotated, without the key.
func (r Rotation) Describe(pending bool) string {
	what := "your active setup"
	if pending {
		what = "your setup whose test is pending"
	}
	return fmt.Sprintf("Rotating the key of %s: %s, model %s (revision %d). The provider, model and caps stay as they are.", what, r.Provider.Name, r.Config.Model, r.Config.ConfigRevision)
}
