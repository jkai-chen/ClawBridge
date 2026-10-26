package docseed

import (
	"bytes"
	"os"
	"regexp"
	"strings"
	"testing"
	"text/template"
)

// docs/templates is the readable source of the embedded templates.
func TestEmbeddedTemplatesMatchTheDocs(t *testing.T) {
	for _, tpl := range Templates {
		doc, err := os.ReadFile("../../docs/templates/" + tpl.File)
		if err != nil {
			t.Fatal(err)
		}
		embedded, err := files.ReadFile("templates/" + tpl.File)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(doc, embedded) {
			t.Fatalf("%s differs from docs/templates; copy the docs version into internal/docseed/templates", tpl.File)
		}
	}
}

var uuidShape = regexp.MustCompile(`(?i)[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}`)

// The repository is public and the documents are written into a team's
// Linear: no mention, no ID, every field known, and the seed token last.
func TestTemplatesRenderGenericAndComplete(t *testing.T) {
	names := map[string]bool{}
	for _, tpl := range Templates {
		if names[tpl.Name] || tpl.Version < 1 || tpl.Title == "" {
			t.Fatal("template list", tpl)
		}
		names[tpl.Name] = true
		raw, _ := files.ReadFile("templates/" + tpl.File)
		if strings.Contains(string(raw), "@") || uuidShape.Match(raw) {
			t.Fatalf("%s holds a mention or an ID", tpl.File)
		}
		// Every placeholder is a known field: an unknown one fails here.
		parsed := template.Must(template.New(tpl.Name).Option("missingkey=error").Parse(string(raw)))
		if err := parsed.Execute(&bytes.Buffer{}, fields{}); err != nil {
			t.Fatalf("%s: %v", tpl.File, err)
		}
		for _, url := range []string{"https://clawbridge.example.test", ""} {
			out, err := Render(tpl, Values{ServiceURL: url, Version: "v0.3.1"}, "0123456789ab")
			if err != nil {
				t.Fatal(tpl.Name, err)
			}
			if strings.Contains(out, "{{") || strings.Contains(out, "<no value>") || strings.Contains(out, "@") || len(out) > 32<<10 {
				t.Fatalf("%s rendered badly:\n%s", tpl.Name, out)
			}
			if !strings.HasSuffix(out, "seed: 0123456789ab\n") || !strings.Contains(out, "`v0.3.1`") || !HasToken(out, "0123456789ab") || HasToken(out, "0123456789ac") {
				t.Fatalf("%s footer:\n%s", tpl.Name, out)
			}
			if tpl.Name == "developer" && (url != "") != strings.Contains(out, "`"+url+"`") {
				t.Fatalf("service URL shown wrongly:\n%s", out)
			}
		}
	}
	if !names["developer"] || !names["reviewer"] {
		t.Fatal("both guides are seeded")
	}
}

// Values are shown as code spans; one that would break out is refused.
func TestRenderRefusesUnsafeValues(t *testing.T) {
	for name, v := range map[string]Values{
		"backtick":   {ServiceURL: "https://x.test/`[click](https://evil.test)`", Version: "v1"},
		"newline":    {ServiceURL: "https://x.test\n# owned", Version: "v1"},
		"no version": {ServiceURL: "https://x.test"},
	} {
		if _, err := Render(Templates[0], v, "0123456789ab"); err == nil {
			t.Fatal(name, "accepted")
		}
	}
	if _, err := Render(Templates[0], Values{Version: "v1"}, "XYZ"); err == nil {
		t.Fatal("a bad token accepted")
	}
	a, _ := NewToken()
	b, _ := NewToken()
	if !tokenPattern(a) || a == b {
		t.Fatal("tokens", a, b)
	}
}
