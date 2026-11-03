package cli

import (
	"context"
	"errors"
	"testing"

	"clawbridge/internal/bus"
	"clawbridge/internal/doctor"
)

func TestUIProfileCarriesWhoAndWhere(t *testing.T) {
	got := uiProfile(Config{Workspace: "ws_example", Project: "prj_example", Principal: "mara@example.test", DeviceID: "dev_secretish", Server: "https://s.example.test"}, true, "mara-mbp", "ws_example", "prj_example")
	want := map[string]any{"workspace": "ws_example", "project": "prj_example", "waiting_for_project": true, "principal": "mara@example.test", "device": "mara-mbp"}
	if len(got) != len(want) {
		t.Fatalf("profile %v", got)
	}
	for k, v := range want {
		if got[k] != v {
			t.Fatalf("%s = %v, want %v", k, got[k], v)
		}
	}
}

type whoamiExecutor struct {
	doctor.Executor
	data any
	err  error
	req  bus.Request
}

func (w *whoamiExecutor) Execute(_ context.Context, r bus.Request) (bus.Result, error) {
	w.req = r
	return bus.Result{Data: w.data}, w.err
}

func TestUIScopeNamesShowNamesAndFallBackToIDs(t *testing.T) {
	c := Config{Workspace: "ws_example", Project: "prj_example"}
	named := &whoamiExecutor{data: map[string]any{
		"workspace": map[string]any{"id": "ws_example", "name": "Acme"},
		"project":   map[string]any{"id": "prj_example", "slug": "web", "name": "Website"},
	}}
	if w, p := uiScopeNames(context.Background(), named, c); w != "Acme" || p != "Website" {
		t.Fatalf("names = %q, %q", w, p)
	}
	if named.req.Operation != "whoami" || named.req.Workspace != "ws_example" || named.req.Project != "prj_example" {
		t.Fatalf("request %+v", named.req)
	}
	slugOnly := &whoamiExecutor{data: map[string]any{"workspace": map[string]any{"name": ""}, "project": map[string]any{"slug": "web"}}}
	if w, p := uiScopeNames(context.Background(), slugOnly, c); w != "ws_example" || p != "web" {
		t.Fatalf("slug fallback = %q, %q", w, p)
	}
	failing := &whoamiExecutor{err: errors.New("offline")}
	if w, p := uiScopeNames(context.Background(), failing, c); w != "ws_example" || p != "prj_example" {
		t.Fatalf("error fallback = %q, %q", w, p)
	}
	if w, p := uiScopeNames(context.Background(), nil, c); w != "ws_example" || p != "prj_example" {
		t.Fatalf("nil executor = %q, %q", w, p)
	}
}
