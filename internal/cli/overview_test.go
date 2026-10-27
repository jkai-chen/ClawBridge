package cli

import (
	"encoding/json"
	"strings"
	"testing"
)

// project overview passes the op's answer through, with what could drive
// a terminal taken out of the names other people chose.
func TestCLIProjectOverview(t *testing.T) {
	isolated(t)
	invoke(t, "setup", "--local", "--workspace", "w")
	invoke(t, "project", "create", "p", "--name", "Web\u202e app")
	t.Setenv("CLAWBRIDGE_PROJECT", "p")
	invoke(t, "register", "eng", "--instance", "eng-i")
	got := invoke(t, "project", "overview", "--json")
	project := got["project"].(map[string]any)
	if project["slug"] != "p" || project["name"] != "Web app" || project["role"] != "owner" {
		t.Fatal(project)
	}
	if rows := got["sessions"].([]any); len(rows) != 1 || rows[0].(map[string]any)["name"] != "eng" || strings.Contains(jsonText(rows[0]), "eng-i") {
		t.Fatal(rows)
	}
	if got["counts"].(map[string]any)["active"] != float64(1) || got["linear"].(map[string]any)["configured"] != false {
		t.Fatal(got)
	}
	rejectCLI(t, "project", "overview", "extra")
}

func jsonText(v any) string {
	raw, _ := json.Marshal(v)
	return string(raw)
}
