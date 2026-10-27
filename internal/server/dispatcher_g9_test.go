package server

import "testing"

// G9-3: the launcher's routes refuse plain HTTP from another machine, as
// the agent's do; over HTTPS through the proxy they answer.
func TestLauncherRefusesPlainHTTP(t *testing.T) {
	e := newModelEnv(t)
	for _, path := range []string{LauncherWantedPath, ModelHelloPath} {
		if status, body := e.do("GET", path, e.launcher, nil, "X-Forwarded-For", "198.51.100.1"); status != 403 || body["error"] != "unauthorized" {
			t.Fatal(path, "over plain HTTP:", status, body)
		}
	}
	if status, _ := e.do("POST", LauncherCredentialPath, e.launcher, map[string]any{"owner_ref": "p_0000000000000000"}, "X-Forwarded-For", "198.51.100.1"); status != 403 {
		t.Fatal("a mint over plain HTTP:", status)
	}
	if status, _ := e.do("GET", LauncherWantedPath, e.launcher, nil, "X-Forwarded-For", "198.51.100.1", "X-Forwarded-Proto", "https"); status != 200 {
		t.Fatal("over HTTPS:", status)
	}
}

// G9-3: at most maxMintsPerHour credentials an hour per owner; the next
// is 429 rate_limited.
func TestLauncherMintLimit(t *testing.T) {
	e := newModelEnv(t)
	_, wanted := e.do("GET", LauncherWantedPath, e.launcher, nil)
	ref := wanted["agents"].([]any)[0].(map[string]any)["owner_ref"].(string)
	for i := range 6 {
		if status, _ := e.do("POST", LauncherCredentialPath, e.launcher, map[string]any{"owner_ref": ref}); status != 200 {
			t.Fatal("mint", i, status)
		}
	}
	if status, body := e.do("POST", LauncherCredentialPath, e.launcher, map[string]any{"owner_ref": ref}); status != 429 || body["error"] != "rate_limited" {
		t.Fatal("the seventh mint:", status, body)
	}
}
