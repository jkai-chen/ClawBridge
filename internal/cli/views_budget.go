package cli

import (
	"encoding/json"
	"fmt"
	"strings"

	"clawbridge/internal/bus"
	"clawbridge/internal/clirender"
)

// byKindTop is how many kinds the by-kind line names.
const byKindTop = 6

func init() { clirender.Register("comment_watch_status", linearBudgetView) }

// linearBudgetView is project linear-budget show: the usual layout, then
// one line of the hour's top kinds, "by kind: KIND CALLS (EVENTS), …".
// --json carries kinds and last_hour whole.
func linearBudgetView(v any) (clirender.Doc, bool) {
	out, ok := asMap(v)
	if !ok {
		return clirender.Doc{}, false
	}
	data, ok := out["data"].(map[string]any)
	if !ok || (data["kinds"] == nil && data["last_hour"] == nil) {
		return clirender.Doc{}, false
	}
	var kinds []bus.LinearBudgetKind
	raw, _ := json.Marshal(data["kinds"])
	json.Unmarshal(raw, &kinds)
	rest := map[string]any{}
	for k, v := range data {
		if k != "kinds" && k != "last_hour" {
			rest[k] = v
		}
	}
	// A copy, so the caller's value keeps its kinds.
	shown := map[string]any{}
	for k, v := range out {
		shown[k] = v
	}
	shown["data"] = rest
	d, err := clirender.Generic(shown)
	if err != nil {
		return clirender.Doc{}, false
	}
	if line := byKindLine(kinds, byKindTop); line != "" {
		d.KV("By kind", line)
	}
	return d, true
}

// byKindLine is the first n kinds as "KIND CALLS (EVENTS)", comma-separated;
// they come the most calls first.
func byKindLine(kinds []bus.LinearBudgetKind, n int) string {
	var parts []string
	for _, k := range kinds {
		if len(parts) == n {
			break
		}
		parts = append(parts, fmt.Sprintf("%s %d (%d)", k.Kind, k.Calls, k.Events))
	}
	return strings.Join(parts, ", ")
}
