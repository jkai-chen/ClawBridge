package cli

import (
	"path/filepath"
	"testing"
)

func TestRuntimeIdentityOnlyTheFlagPins(t *testing.T) {
	t.Setenv("CLAWBRIDGE_SESSION", "")
	if !runtimeIdentity(Config{}) {
		t.Error("no flag: runtime mode")
	}
	if runtimeIdentity(Config{Session: "a"}) {
		t.Error("--session pins")
	}
	// An exported CLAWBRIDGE_SESSION, a CLAWBRIDGE_INSTANCE and a session in a
	// config file are not the flag: they never pin (R2-M1, F7).
	t.Setenv("CLAWBRIDGE_SESSION", "x")
	t.Setenv("CLAWBRIDGE_INSTANCE", "inst")
	if !runtimeIdentity(Config{}) {
		t.Error("CLAWBRIDGE_SESSION must not pin")
	}
}

func TestSuggestedSession(t *testing.T) {
	t.Setenv("CLAWBRIDGE_SESSION", "from-env")
	if got := suggestedSession(Config{Session: "from-env"}); got != "from-env" {
		t.Errorf("env: %q", got)
	}
	t.Setenv("CLAWBRIDGE_SESSION", "")
	if got := suggestedSession(Config{Session: "from-file"}); got != "from-file" {
		t.Errorf("file: %q", got)
	}
}

func TestProjectSource(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "r")
	repo := Config{Project: "p", Sources: map[string]string{"project": ".clawbridge.toml at " + filepath.Join(dir, ".clawbridge.toml")}}
	if got := projectSource(repo, "git@example.test:o/r.git"); got != ".clawbridge.toml at "+dir {
		t.Errorf("repo file: %q", got)
	}
	if got := projectSource(Config{}, "git@example.test:o/r.git"); got != "the git remote" {
		t.Errorf("remote: %q", got)
	}
	file := Config{Project: "p", Sources: map[string]string{"project": "config file /h/config.toml"}}
	if got := projectSource(file, "git@example.test:o/r.git"); got != "profile default" {
		t.Errorf("profile: %q", got)
	}
}
