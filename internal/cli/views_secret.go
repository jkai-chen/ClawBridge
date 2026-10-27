package cli

import (
	"encoding/json"
	"fmt"
	"slices"

	"clawbridge/internal/clirender"
	"clawbridge/internal/devicesign"
)

// The commands that print a secret have their own views, never the generic
// layout (live/CLI-OUTPUT-PLAN.md M3): each secret is printed exactly once,
// alone on its line and whole, as today's JSON has it once. When it went to
// a file (--out), only the file is named.
var secretViews = map[string]struct {
	title string
	// fields are the secret fields, in the order shown, with their labels.
	fields [][2]string
}{
	"invite_create": {"Invite created", [][2]string{
		{"link", "Invite link (shown once; give it privately):"},
		{"code", "Invite code (shown once; give it privately):"},
	}},
	"device_ci_create": {"CI device created", [][2]string{
		{devicesign.EnvKey, devicesign.EnvKey + " (shown once; store it in the CI secret store now):"},
	}},
	"intake_token_create": {"Intake token created", [][2]string{
		{"token", "Intake token (shown once):"},
	}},
}

func init() {
	for name, sv := range secretViews {
		clirender.RegisterSecret(name, secretView(sv.title, sv.fields))
	}
}

// resultData reads a {source, data} result's data as a map.
func resultData(v any) (map[string]any, bool) {
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, false
	}
	var r struct {
		Data map[string]any `json:"data"`
	}
	if json.Unmarshal(raw, &r) != nil || r.Data == nil {
		return nil, false
	}
	return r.Data, true
}

func secretView(title string, fields [][2]string) clirender.View {
	return func(v any) (clirender.Doc, bool) {
		data, ok := resultData(v)
		if !ok {
			return clirender.Doc{}, false
		}
		var d clirender.Doc
		d.Title(title)
		skip := map[string]bool{"note": true}
		for _, f := range fields {
			skip[f[0]] = true
		}
		keys := make([]string, 0, len(data))
		for k := range data {
			if !skip[k] {
				keys = append(keys, k)
			}
		}
		slices.Sort(keys)
		for _, k := range keys {
			d.KV(clirender.Humanize(k), shortValue(data[k]))
		}
		for _, f := range fields {
			// The link carries the code: the code is shown only without one.
			if link, _ := data["link"].(string); f[0] == "code" && link != "" {
				continue
			}
			if s, _ := data[f[0]].(string); s != "" {
				d.Text("")
				d.Secret(f[1], s)
			}
		}
		if note, _ := data["note"].(string); note != "" {
			d.Text("")
			d.Note(note)
		}
		return d, true
	}
}

// shortValue is a field's value on one line: nested values are left to
// --json.
func shortValue(v any) string {
	switch x := v.(type) {
	case nil:
		return "-"
	case string:
		if x == "" {
			return "-"
		}
		return x
	case bool:
		if x {
			return "yes"
		}
		return "no"
	case map[string]any, []any:
		return "(… see --json)"
	}
	return fmt.Sprint(v)
}
