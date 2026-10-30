package cli

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"clawbridge/internal/bus"
	"clawbridge/internal/clirender"
)

// project linear-budget show adds one line of the hour's top kinds, at
// most six, each with its events; --json carries kinds and last_hour
// whole; a result with no kinds (an older server) keeps the usual layout.
func TestLinearBudgetShowsTheTopKinds(t *testing.T) {
	kinds := []bus.LinearBudgetKind{{Kind: "watch.comments", Calls: 40, Events: 12}, {Kind: "app_write.update", Calls: 32, Events: 2}, {Kind: "watch.health", Calls: 10, Events: 1},
		{Kind: "watch.markers", Calls: 6, Events: 3}, {Kind: "profile.create", Calls: 4, Events: 2}, {Kind: "watch.docs", Calls: 2, Events: 2}, {Kind: "issue_move", Calls: 1, Events: 1}}
	data := map[string]any{"budget_used": 95, "budget_limit": 0, "capped": false, "budget_source": "default", "kinds": kinds,
		"last_hour": bus.LinearBudgetHour{Hour: "2026-10-30T03", Kinds: []bus.LinearBudgetKind{{Kind: "watch.comments", Calls: 9, Events: 3}}}}
	var out bytes.Buffer
	if err := clirender.Write(&out, clirender.Human, "comment_watch_status", bus.Result{Source: "server", Data: data}); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	want := "By kind watch.comments 40 (12), app_write.update 32 (2), watch.health 10 (1), watch.markers 6 (3), profile.create 4 (2), watch.docs 2 (2)"
	if !strings.Contains(strings.Join(strings.Fields(got), " "), want) || strings.Contains(got, "issue_move") || !strings.Contains(got, "Budget used") || strings.Contains(got, "2026-10-30T03") {
		t.Fatalf("%s", got)
	}
	out.Reset()
	if err := clirender.Write(&out, clirender.JSON, "comment_watch_status", bus.Result{Source: "server", Data: data}); err != nil {
		t.Fatal(err)
	}
	var decoded struct {
		Data struct {
			Kinds    []bus.LinearBudgetKind `json:"kinds"`
			LastHour bus.LinearBudgetHour   `json:"last_hour"`
		} `json:"data"`
	}
	if err := json.Unmarshal(out.Bytes(), &decoded); err != nil || len(decoded.Data.Kinds) != 7 || decoded.Data.LastHour.Hour != "2026-10-30T03" {
		t.Fatalf("%s %v", out.String(), err)
	}
	delete(data, "kinds")
	delete(data, "last_hour")
	out.Reset()
	if err := clirender.Write(&out, clirender.Human, "comment_watch_status", bus.Result{Source: "server", Data: data}); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), "By kind") || !strings.Contains(out.String(), "Budget used") {
		t.Fatalf("%s", out.String())
	}
}
