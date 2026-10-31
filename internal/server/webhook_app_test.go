package server

import (
	"strings"
	"testing"
)

const (
	appSecret1 = "appsec-synthetic-0123456789"
	appSecret2 = "appsec-synthetic-9876543210"
)

func TestParseAppWebhookSecret(t *testing.T) {
	if got, err := parseAppWebhookSecret(""); err != nil || got != nil {
		t.Fatal("empty:", got, err)
	}
	if got, err := parseAppWebhookSecret(appSecret1); err != nil || len(got) != 1 || got[0] != appSecret1 {
		t.Fatal("one:", got, err)
	}
	if got, err := parseAppWebhookSecret(appSecret1 + "," + appSecret2); err != nil || len(got) != 2 || got[0] != appSecret1 || got[1] != appSecret2 {
		t.Fatal("two:", got, err)
	}
	// The limits are inclusive.
	for _, n := range []int{16, 256} {
		if _, err := parseAppWebhookSecret(strings.Repeat("x", n)); err != nil {
			t.Fatal(n, "was refused:", err)
		}
	}
	for name, v := range map[string]string{
		"empty first":  "," + appSecret1,
		"empty second": appSecret1 + ",",
		"only a comma": ",",
		"three":        appSecret1 + "," + appSecret2 + ",appsec-synthetic-5555555555",
		"short":        "appsec-short",
		"short second": appSecret1 + ",appsec-short",
		"long":         "appsec-" + strings.Repeat("x", 256),
		"space":        "appsec synthetic 0123456789",
		"control":      "appsec-synthetic-\t0123456789",
		"non-ascii":    "appsec-synthetic-é0123456789",
		"equal":        appSecret1 + "," + appSecret1,
	} {
		_, err := parseAppWebhookSecret(v)
		if err == nil {
			t.Fatal(name, "was accepted")
		}
		if CheckAppWebhookSecret(v) == nil {
			t.Fatal(name, "passed the check")
		}
		for _, leak := range []string{"appsec", "synthetic", "xxxx", "short"} {
			if strings.Contains(err.Error(), leak) {
				t.Fatal(name, "error shows the secret:", err)
			}
		}
	}
	if err := CheckAppWebhookSecret(appSecret1 + "," + appSecret2); err != nil {
		t.Fatal(err)
	}
	// An error names the position.
	if _, err := parseAppWebhookSecret(appSecret1 + ",appsec-short"); err == nil || !strings.Contains(err.Error(), "value 2") {
		t.Fatal("position:", err)
	}
}

func TestParseServeAppWebhook(t *testing.T) {
	base := []string{"--bootstrap-owner", "owner@example.test", "--auth", "device", "--public-origin", "https://cb.example.test"}
	env := func(kv map[string]string, app bool) func(string) string {
		return func(k string) string {
			if k == "CLAWBRIDGE_LINEAR_APP_CLIENT_ID" && app {
				return "client-id"
			}
			return kv[k]
		}
	}
	secret := func(v string) map[string]string { return map[string]string{"CLAWBRIDGE_LINEAR_APP_WEBHOOK_SECRET": v} }
	s, err := ParseServe(base, env(secret(appSecret1+","+appSecret2), true))
	if err != nil || len(s.AppWebhookSecrets) != 2 || s.AppWebhookSecrets[0] != appSecret1 || s.AppWebhookSecrets[1] != appSecret2 {
		t.Fatal(s.AppWebhookSecrets, err)
	}
	if s, err = ParseServe(base, env(nil, false)); err != nil || len(s.AppWebhookSecrets) != 0 {
		t.Fatal("none:", err)
	}
	// A bad secret refuses the start, without showing it.
	_, err = ParseServe(base, env(secret(appSecret1+","+appSecret1), true))
	if err == nil || strings.Contains(err.Error(), "appsec") {
		t.Fatal("bad secret:", err)
	}
	// The secret needs the Linear app.
	_, err = ParseServe(base, env(secret(appSecret1), false))
	if err == nil || !strings.Contains(err.Error(), "Linear app") || strings.Contains(err.Error(), "appsec") {
		t.Fatal("secret without an app:", err)
	}
	// The gate.
	gate := func(v string) func(string) string {
		return env(map[string]string{"CLAWBRIDGE_APP_WEBHOOK_PROVEN": v}, false)
	}
	for v, want := range map[string]bool{"": false, "0": false, "false": false, "1": true, "true": true} {
		if s, err = ParseServe(base, gate(v)); err != nil || s.AppWebhookProven != want {
			t.Fatal(v, s.AppWebhookProven, err)
		}
	}
	if _, err = ParseServe(base, gate("yes")); err == nil || !strings.Contains(err.Error(), "CLAWBRIDGE_APP_WEBHOOK_PROVEN") {
		t.Fatal("yes was accepted:", err)
	}
	if s, err = ParseServe(append(base, "--app-webhook-proven"), gate("")); err != nil || !s.AppWebhookProven {
		t.Fatal("flag:", err)
	}
	// It is separate from the per-team gate.
	if s, err = ParseServe(base, gate("1")); err != nil || s.WebhooksProven {
		t.Fatal("the app gate turned on the team gate:", err)
	}
}

// An app secret that is also a team's would verify a delivery on the other
// route.
func TestParseServeAppWebhookSharesNoTeamSecret(t *testing.T) {
	base := []string{"--bootstrap-owner", "owner@example.test", "--auth", "device", "--public-origin", "https://cb.example.test"}
	const team1, team2 = "11111111-1111-4111-8111-1111111111a1", "22222222-2222-4222-8222-2222222222b2"
	const teamSecret1, teamSecret2, teamSecret3 = "teamsec-synthetic-0000000001", "teamsec-synthetic-0000000002", "teamsec-synthetic-0000000003"
	env := func(app, teams string) func(string) string {
		return func(k string) string {
			switch k {
			case "CLAWBRIDGE_LINEAR_APP_CLIENT_ID":
				return "client-id"
			case "CLAWBRIDGE_LINEAR_APP_WEBHOOK_SECRET":
				return app
			case "CLAWBRIDGE_LINEAR_WEBHOOK_SECRETS":
				return teams
			}
			return ""
		}
	}
	teams := team1 + "=" + teamSecret1 + "," + team1 + "=" + teamSecret2 + "," + team2 + "=" + teamSecret3
	if s, err := ParseServe(base, env(appSecret1+","+appSecret2, teams)); err != nil || len(s.AppWebhookSecrets) != 2 || len(s.WebhookSecrets) != 2 {
		t.Fatal("distinct secrets:", err)
	}
	for name, app := range map[string]string{
		"first value":       teamSecret1,
		"second value":      appSecret1 + "," + teamSecret3,
		"a team's second":   teamSecret2,
		"another team":      teamSecret3 + "," + appSecret1,
		"rotation both ids": teamSecret2 + "," + teamSecret1,
	} {
		_, err := ParseServe(base, env(app, teams))
		if err == nil || !strings.Contains(err.Error(), "different secret per route") {
			t.Fatal(name, "was accepted:", err)
		}
		for _, leak := range []string{"teamsec", "appsec", "synthetic"} {
			if strings.Contains(err.Error(), leak) {
				t.Fatal(name, "error shows a secret:", err)
			}
		}
	}
}
