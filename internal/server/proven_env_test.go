package server

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"clawbridge/internal/bus"
)

// Every CLAWBRIDGE_*_PROVEN key the server reads must be passed through by
// deploy/compose.yaml, or on a Compose host its gate can never turn on.
func TestEveryProvenGateReachesCompose(t *testing.T) {
	sources, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	read := regexp.MustCompile(`getenv\("(CLAWBRIDGE_[A-Z_]+_PROVEN)"\)`)
	keys := map[string]bool{}
	for _, f := range sources {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range read.FindAllStringSubmatch(string(src), -1) {
			keys[m[1]] = true
		}
	}
	if len(keys) < 2 {
		t.Fatal("found too few gates; the pattern no longer matches how the server reads them:", keys)
	}
	compose, err := os.ReadFile(filepath.Join("..", "..", "deploy", "compose.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	for k := range keys {
		if !strings.Contains(string(compose), "      "+k+": ${"+k+":-}\n") {
			t.Errorf("deploy/compose.yaml doesn't pass %s to the api service", k)
		}
	}
}

// An empty gate, as Compose passes an unset one, is off.
func TestServeEmptyGatesAreOff(t *testing.T) {
	base := []string{"--bootstrap-owner", "owner@example.test", "--auth", "device", "--public-origin", "https://cb.example.test"}
	settings, err := ParseServe(base, func(string) string { return "" })
	if err != nil || settings.ProjectFlowProven || settings.IssueSubscribersProven {
		t.Fatal(settings.ProjectFlowProven, settings.IssueSubscribersProven, err)
	}
}

// Every CLAWBRIDGE_*_PROVEN key the server reads is a gate the owner's
// overview shows, by the key's lower-case name.
func TestEveryProvenGateIsShown(t *testing.T) {
	src, err := os.ReadFile("server.go")
	if err != nil {
		t.Fatal(err)
	}
	shown := map[string]bool{}
	for _, name := range bus.ProvenGateNames {
		shown[name] = true
	}
	keys := regexp.MustCompile(`getenv\("CLAWBRIDGE_([A-Z_]+)_PROVEN"\)`).FindAllStringSubmatch(string(src), -1)
	if len(keys) != len(shown) {
		t.Fatalf("the server reads %d gates and the overview shows %d", len(keys), len(shown))
	}
	for _, k := range keys {
		if !shown[strings.ToLower(k[1])] {
			t.Errorf("CLAWBRIDGE_%s_PROVEN isn't in bus.ProvenGateNames", k[1])
		}
	}
}
