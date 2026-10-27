package dispatchermodel

import (
	"strings"
	"testing"
	"unicode"
)

// models.json lists exactly the listed, non-custom providers, and every
// model in it is one setup accepts: an ID the shared rule takes (not one
// that looks like a key), a printable name, sane numbers.
func TestModelHintsFile(t *testing.T) {
	for _, p := range Providers() {
		if _, ok := modelHints[p.ID]; ok == p.Custom {
			t.Fatalf("%s: listed %v, custom %v", p.ID, ok, p.Custom)
		}
	}
	for id, list := range modelHints {
		if p, ok := LookupProvider(id); !ok || p.Custom {
			t.Fatalf("models.json lists %q, not a listed provider", id)
		}
		if len(list) == 0 {
			t.Fatalf("%s has no models", id)
		}
		for _, m := range list {
			if err := ValidateModel(m.ID); err != nil {
				t.Fatalf("%s/%s: %v", id, m.ID, err)
			}
			if m.Name == "" || strings.IndexFunc(m.Name, func(r rune) bool { return !unicode.IsPrint(r) }) >= 0 {
				t.Fatalf("%s/%s: name %q", id, m.ID, m.Name)
			}
			if m.ContextWindow <= 0 || m.InputPerMTok < 0 || m.OutputPerMTok < 0 {
				t.Fatalf("%s/%s: %+v", id, m.ID, m)
			}
		}
	}
}

// The list is the cheapest priced models, at most HintsShown, and says how
// many more there are; free ones are only left out of the list.
func TestModelHints(t *testing.T) {
	saved := modelHints
	t.Cleanup(func() { modelHints = saved })
	modelHints = map[string][]ModelHint{"anthropic": {
		{ID: "m-dear", Name: "Dear", ContextWindow: 200_000, InputPerMTok: 15, OutputPerMTok: 75},
		{ID: "m-free", Name: "Free", ContextWindow: 8000},
		{ID: "m-b", Name: "B", ContextWindow: 1_000_000, InputPerMTok: 1, OutputPerMTok: 5},
		{ID: "m-a", Name: "A", ContextWindow: 128_000, InputPerMTok: 3, OutputPerMTok: 4},
	}}
	shown, more := ModelHints("anthropic")
	var ids []string
	for _, m := range shown {
		ids = append(ids, m.ID)
	}
	if strings.Join(ids, ",") != "m-b,m-a,m-dear" || more != 1 {
		t.Fatal(ids, more)
	}
	if got := HintLine(shown[0]); got != "B (m-b) · $1 in / $5 out per million tokens · 1M context" {
		t.Fatal(got)
	}
	if got := HintsNote(more); !strings.HasPrefix(got, "1 more in the catalogue: type any model ID.") || !strings.Contains(got, "not a quote") {
		t.Fatal(got)
	}
	// One past the list: the cheapest HintsShown, and the one left counted.
	modelHints["openai"] = nil
	for i := 0; i <= HintsShown; i++ {
		modelHints["openai"] = append(modelHints["openai"], ModelHint{ID: "o" + strings.Repeat("o", i), Name: "O", ContextWindow: 1, InputPerMTok: float64(i + 1)})
	}
	if shown, more := ModelHints("openai"); len(shown) != HintsShown || more != 1 || shown[HintsShown-1].InputPerMTok != HintsShown {
		t.Fatal(len(shown), more)
	}
	for i := 0; i < 2*HintsShown; i++ {
		modelHints["anthropic"] = append(modelHints["anthropic"], ModelHint{ID: "x" + strings.Repeat("y", i), Name: "X", ContextWindow: 1, InputPerMTok: 100})
	}
	if shown, more := ModelHints("anthropic"); len(shown) != HintsShown || more != len(modelHints["anthropic"])-HintsShown {
		t.Fatal(len(shown), more)
	}
	if shown, more := ModelHints("openai-compatible"); len(shown) != 0 || more != 0 {
		t.Fatal(shown, more)
	}
	if HintsNote(0) != "List prices from the agent's catalogue, not a quote." {
		t.Fatal(HintsNote(0))
	}
}

// A number picks a listed model; anything else is the ID as typed.
func TestModelAnswer(t *testing.T) {
	shown := []ModelHint{{ID: "first"}, {ID: "second"}}
	for answer, want := range map[string]string{"1": "first", "2": "second", "0": "0", "3": "3", "-1": "-1", "my-model": "my-model", "": ""} {
		if got := ModelAnswer(answer, shown); got != want {
			t.Fatalf("%q: %q", answer, got)
		}
	}
	if ModelAnswer("1", nil) != "1" {
		t.Fatal("a number with no list")
	}
}
