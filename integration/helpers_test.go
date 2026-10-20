package integration

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"

	"clawbridge/internal/bus"
)

// Each fixture uses a private local database; no test resolves a user's config.
type fixture struct {
	t     *testing.T
	store *bus.Store
	path  string
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	path := filepath.Join(t.TempDir(), "bus.db")
	s, err := bus.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Error(err)
		}
	})
	return &fixture{t: t, store: s, path: path}
}

func (f *fixture) request(op, workspace, project, session string, args map[string]any) bus.Request {
	return bus.Request{Operation: op, Workspace: workspace, Project: project, Session: session, Args: args}
}

func (f *fixture) call(op, workspace, project, session string, args map[string]any) any {
	f.t.Helper()
	result, err := f.store.Execute(context.Background(), f.request(op, workspace, project, session, args))
	if err != nil {
		f.t.Fatalf("%s [%s/%s/%s]: %v", op, workspace, project, session, err)
	}
	if result.Source != "local-authority" {
		f.t.Fatalf("%s source=%q, want local-authority", op, result.Source)
	}
	raw, err := json.Marshal(result.Data)
	if err != nil {
		f.t.Fatal(err)
	}
	var data any
	if err := json.Unmarshal(raw, &data); err != nil {
		f.t.Fatal(err)
	}
	return data
}

func (f *fixture) reject(op, workspace, project, session string, args map[string]any) error {
	f.t.Helper()
	result, err := f.store.Execute(context.Background(), f.request(op, workspace, project, session, args))
	if err == nil {
		f.t.Fatalf("%s [%s/%s/%s] unexpectedly succeeded: %+v", op, workspace, project, session, result)
	}
	return err
}

func object(t *testing.T, value any) map[string]any {
	t.Helper()
	out, ok := value.(map[string]any)
	if !ok {
		t.Fatalf("expected object, got %T: %v", value, value)
	}
	return out
}

func stringField(t *testing.T, value any, key string) string {
	t.Helper()
	field, ok := object(t, value)[key].(string)
	if !ok || field == "" {
		t.Fatalf("missing/non-string %s in %v", key, value)
	}
	return field
}
