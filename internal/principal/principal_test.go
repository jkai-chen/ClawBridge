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

func TestNormalDomainIsOneRuleForEveryLayer(t *testing.T) {
	for in, want := range map[string]string{
		" Corp.Example. ":                     "corp.example",
		"corp.example":                        "corp.example",
		"localhost":                           "",
		"not a domain":                        "",
		"-corp.example":                       "",
		"corp..example":                       "",
		"":                                    "",
		strings.Repeat("a.", 127) + "example": "",
	} {
		got, ok := NormalDomain(in)
		if got != want || ok != (want != "") {
			t.Errorf("%q: %q %v, want %q", in, got, ok, want)
		}
	}
	for p, want := range map[string]bool{
		"bot@project.iam.gserviceaccount.com":       true,
		"123-compute@developer.gserviceaccount.com": true,
		"project@appspot.gserviceaccount.com":       true,
		"bot@iam.gserviceaccount.com.example":       false,
		"bot@gserviceaccount.com.example":           false,
		"bot@example.com":                           false,
	} {
		if ServiceAccount(p) != want {
			t.Errorf("%s: %v, want %v", p, !want, want)
		}
	}
}
