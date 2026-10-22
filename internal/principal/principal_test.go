package principal

import (
	"strings"
	"testing"
)

func TestValidIsOneRuleForEveryLayer(t *testing.T) {
	for p, want := range map[string]bool{
		"alice@example.test":                       true,
		"a.b+tag@sub.example.test":                 true,
		"<alice@example.test>":                     false, // net/mail would strip the brackets
		"Alice <alice@example.test>":               false,
		"a@b@example.test":                         false,
		"@example.test":                            false,
		"alice@":                                   false,
		"alice @example.test":                      false,
		"alice@example.test\x00":                   false,
		"alice@[1.2.3.4":                           false,
		strings.Repeat("a", 310) + "@example.test": false,
	} {
		if Valid(p) != want {
			t.Errorf("%q: %v, want %v", p, !want, want)
		}
	}
	if Normal("  Alice@Example.TEST ") != "alice@example.test" {
		t.Fatal("principal not normalised")
	}
}
