package secretenv

import (
	"reflect"
	"testing"
)

// G9 F3: every ClawBridge secret's name is dropped; its settings, and
// everything that isn't ClawBridge's, stay.
func TestScrubDropsClawBridgeSecretsOnly(t *testing.T) {
	secrets := []string{
		"CLAWBRIDGE_DEVICE_KEY", "CLAWBRIDGE_DATABASE_URL", "CLAWBRIDGE_LAUNCHER_TOKEN",
		"CLAWBRIDGE_LINEAR_APP_CLIENT_SECRET", "CLAWBRIDGE_LINEAR_APP_TOKEN_KEY",
		"CLAWBRIDGE_MODEL_KEY_SEAL_KEY", "CLAWBRIDGE_MODEL_KEY_SEAL_KEY_PREVIOUS",
		"CLAWBRIDGE_OAUTH_CLIENT_SECRET", "CLAWBRIDGE_OAUTH_DEVICE_CLIENT_SECRET",
		"CLAWBRIDGE_TEST_POSTGRES_DSN",
	}
	kept := []string{
		"CLAWBRIDGE_KEY_STORE", "CLAWBRIDGE_PROJECT", "CLAWBRIDGE_DEVICE_ID", "CLAWBRIDGE_PRINCIPAL",
		"CLAWBRIDGE_OAUTH_CLIENT_ID", "CLAWBRIDGE_SERVER", "CLAWBRIDGE_CREDENTIAL_SOURCE",
		"PATH", "HOME", "AWS_SECRET_ACCESS_KEY", "ANTHROPIC_API_KEY", "GITHUB_TOKEN",
	}
	env := []string{}
	for _, n := range append(append([]string{}, secrets...), kept...) {
		env = append(env, n+"=v")
	}
	want := []string{}
	for _, n := range kept {
		want = append(want, n+"=v")
	}
	if got := Scrub(env); !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v", got)
	}
	// R122-L1: Windows' names are case-insensitive.
	if got := Scrub([]string{"CLAWBRIDGE_DEVICE_KEY=a=b", "clawbridge_device_key=c", "Clawbridge_Oauth_Client_Secret=d", "X=1"}); !reflect.DeepEqual(got, []string{"X=1"}) {
		t.Fatal(got)
	}
}
