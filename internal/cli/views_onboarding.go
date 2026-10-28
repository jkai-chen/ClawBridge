package cli

import (
	"fmt"
	"strings"

	"clawbridge/internal/clirender"
	"clawbridge/internal/install"
)

// Onboarding's previews as text for people (TUI-UX-AUDIT U51): the MCP
// entry before it is written, and what turning the dispatcher on does.
// They were JSON dumps.

func init() {
	clirender.Register("onboarding_mcp_entry", mcpEntryView)
	clirender.Register("onboarding_dispatcher_preview", dispatcherPreviewView)
}

func mcpEntryView(v any) (clirender.Doc, bool) {
	r, ok := v.(install.Result)
	if !ok || r.Entry == nil {
		return clirender.Doc{}, false
	}
	var d clirender.Doc
	d.KV("File", r.Path)
	d.KV("Command", previewWord(r.Entry["command"]))
	d.KV("Arguments", previewWords(r.Entry["args"], " "))
	if t := previewWord(r.Entry["type"]); t != "-" {
		d.KV("Type", t)
	}
	return d, true
}

// dispatcherPreviewView is dispatcher enable's preview, in what it changes.
func dispatcherPreviewView(v any) (clirender.Doc, bool) {
	p, ok := v.(map[string]any)
	if !ok || p["preview"] != true {
		return clirender.Doc{}, false
	}
	var d clirender.Doc
	d.KV("Engine", previewWord(p["mode"]))
	before := previewWord(p["coordinator_before"])
	if before == "-" {
		before = "none"
	}
	d.KV("Coordinator", before+" → "+previewWord(p["coordinator_after"]))
	d.KV("Sessions it runs", previewWords(p["sessions"], ", "))
	domains := previewWords(p["domains"], ", ")
	if p["default_domain_created"] == true {
		domains += " (created now: all the task board's Linear projects)"
	}
	d.KV("Domains", domains)
	confirms := previewWords(p["auto_confirm_domains"], ", ")
	if confirms == "-" {
		confirms = "none yet: a domain confirms claims once it has trusted sessions; until then claims come to you"
	}
	d.KV("Confirms claims in", confirms)
	if off := previewWords(p["turns_off"], ", "); off != "-" {
		d.KV("Turns off", off)
	}
	return d, true
}

// previewWord is one value as text; "-" when there is none.
func previewWord(v any) string {
	switch x := v.(type) {
	case nil:
		return "-"
	case string:
		if x == "" {
			return "-"
		}
		return x
	}
	return fmt.Sprint(v)
}

// previewWords is a list as text, joined by sep; "-" when it is empty.
func previewWords(v any, sep string) string {
	var words []string
	switch x := v.(type) {
	case []string:
		words = x
	case []any:
		for _, w := range x {
			words = append(words, previewWord(w))
		}
	}
	if len(words) == 0 {
		return "-"
	}
	return strings.Join(words, sep)
}
