package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"clawbridge/internal/bus"
	"clawbridge/internal/linearapp"
	"clawbridge/internal/linearprovider"
	"clawbridge/internal/watcher"
)

const (
	hrProject = "e0000000-0000-4000-8000-0000000000a1"
	hrUpdate  = "e0000000-0000-4000-8000-0000000000b1"
)

// fakeHealthStore scripts the operator's bus reads and records.
type fakeHealthStore struct {
	linearapp.Store
	pidErr    error
	post      bus.HealthPostRef
	postErr   error
	recordErr error
	recorded  []bus.HealthPostRef
}

func (f *fakeHealthStore) OperatorHealthProject(context.Context, string) (string, error) {
	return "proj_1", f.pidErr
}
func (f *fakeHealthStore) OperatorForcedHealthPost(_ context.Context, project, update string) (bus.HealthPostRef, error) {
	return f.post, f.postErr
}
func (f *fakeHealthStore) OperatorForcedHealthPostDeleted(_ context.Context, post bus.HealthPostRef) error {
	f.recorded = append(f.recorded, post)
	return f.recordErr
}

type fakeForcer struct {
	run          watcher.HealthRun
	err          error
	pid, project string
	forceNew     bool
}

func (f *fakeForcer) ForceHealth(_ context.Context, pid, lp string, forceNew bool) (watcher.HealthRun, error) {
	f.pid, f.project, f.forceNew = pid, lp, forceNew
	return f.run, f.err
}

func TestHealthRunNeedsAProjectAndConfirm(t *testing.T) {
	t.Setenv("CLAWBRIDGE_TEST_EMPTY_DSN", "")
	for _, c := range []struct {
		args []string
		want string
	}{
		{[]string{"health-run", "--confirm"}, "needs --project"},
		{[]string{"health-run", "x", "--project", hrProject, "--confirm"}, "usage"},
		{[]string{"health-run", "--project", hrProject, "--force-new", "--delete", hrUpdate, "--confirm"}, "not both"},
		{[]string{"health-run", "--project", hrProject, "--p8", "--confirm"}, "flag provided but not defined"},
		{[]string{"health-run", "--project", hrProject}, "may post one project update to it; repeat with --confirm"},
		{[]string{"health-run", "--project", hrProject, "--delete", hrUpdate}, "only if a forced health run made it; repeat with --confirm"},
		{[]string{"health-run", "--project", hrProject, "--confirm", "--database-url-env", "CLAWBRIDGE_TEST_EMPTY_DSN"}, "environment variable is empty"},
	} {
		var out bytes.Buffer
		if err := RunOperator(context.Background(), c.args, &out); err == nil || !strings.Contains(err.Error(), c.want) || out.Len() != 0 {
			t.Errorf("%v: %v", c.args, err)
		}
	}
}

// The run is the watcher's; the command prints it, and a failed or
// unreadable post is an error.
func TestHealthRunPrintsTheRun(t *testing.T) {
	ctx := context.Background()
	store := &fakeHealthStore{}
	f := &fakeForcer{run: watcher.HealthRun{LinearProject: hrProject, Health: "atRisk", Acted: "forced", Posted: "accepted", UpdateID: hrUpdate, Calls: 3}}
	var out bytes.Buffer
	if err := healthRun(ctx, store, f, strings.ToUpper(hrProject), true, &out); err != nil {
		t.Fatal(err)
	}
	var got struct {
		Run watcher.HealthRun `json:"health_run"`
	}
	if json.Unmarshal(out.Bytes(), &got) != nil || got.Run != f.run || f.pid != "proj_1" || f.project != hrProject || !f.forceNew {
		t.Fatal(out.String(), f)
	}
	for name, c := range map[string]struct {
		run  watcher.HealthRun
		err  error
		want string
	}{
		"failed in Linear": {run: watcher.HealthRun{Error: "too_many_issues"}, want: "the run failed (too_many_issues)"},
		"uncertain post":   {run: watcher.HealthRun{Acted: "forced", Posted: "pending"}, want: "stays pending"},
		"refused by bus":   {err: errors.New("lease held"), want: "health-run: failed"},
		"deadline":         {err: context.DeadlineExceeded, want: "health-run: deadline"},
	} {
		out.Reset()
		if err := healthRun(ctx, store, &fakeForcer{run: c.run, err: c.err}, hrProject, false, &out); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v", name, err)
		}
	}
	store.pidErr = errors.New("project health is not enabled for that Linear project")
	f = &fakeForcer{}
	if err := healthRun(ctx, store, f, hrProject, false, &out); err == nil || f.pid != "" {
		t.Fatal("ran without an enabled project:", err)
	}
}

// fakeDeleter is the app's status-update tools and its GraphQL read for
// the delete.
type fakeDeleter struct {
	deleteErr error
	// nodes is the by-ID read's answer (nil: no projectUpdates at all).
	nodes   []string
	noField bool
	readErr error
	calls   []string
}

func (f *fakeDeleter) StatusUpdater(context.Context) (statusUpdater, error) { return f, nil }
func (f *fakeDeleter) StatusUpdateSchemas() map[string]any                  { return nil }
func (f *fakeDeleter) StatusUpdatePreflight(string) error                   { return nil }
func (f *fakeDeleter) SaveStatusUpdate(context.Context, string, string, string) (string, error) {
	panic("the delete never saves")
}
func (f *fakeDeleter) StatusUpdates(context.Context, string, string) ([]linearprovider.StatusUpdateRef, error) {
	panic("the delete never trusts a page of updates")
}
func (f *fakeDeleter) DeleteStatusUpdate(_ context.Context, id string) error {
	f.calls = append(f.calls, "delete "+id)
	return f.deleteErr
}
func (f *fakeDeleter) Close() error { return nil }
func (f *fakeDeleter) GraphQL(_ context.Context, query string, variables map[string]any, out any) error {
	if query != healthUpdateQuery || variables["id"] != hrUpdate {
		panic("unexpected read input")
	}
	f.calls = append(f.calls, "read")
	if f.readErr != nil {
		return f.readErr
	}
	nodes := []any{}
	for _, id := range f.nodes {
		nodes = append(nodes, map[string]any{"id": id})
	}
	answer := map[string]any{"projectUpdates": map[string]any{"nodes": nodes}}
	if f.noField {
		answer = map[string]any{}
	}
	raw, _ := json.Marshal(answer)
	return json.Unmarshal(raw, out)
}

func withDeleter(t *testing.T, f *fakeDeleter) {
	saved := newHealthPostApp
	t.Cleanup(func() { newHealthPostApp = saved })
	newHealthPostApp = func(linearapp.Store) (healthPostApp, error) { return f, nil }
}

// R91-H1: the delete removes only the update the bus says a forced run
// made, and records it only when the delete succeeded and a read of that
// update by ID finds none. Anything else is an ALERT, recording nothing.
func TestHealthRunDeletesOnlyItsOwnPost(t *testing.T) {
	ctx := context.Background()
	ref := bus.HealthPostRef{ProjectID: "proj_1", LinearProject: hrProject, Day: "2026-10-26", UpdateID: hrUpdate}

	store := &fakeHealthStore{postErr: errors.New("that update is not a post a forced health run made on that Linear project; nothing was deleted")}
	f := &fakeDeleter{}
	withDeleter(t, f)
	var out bytes.Buffer
	if err := healthRunDelete(ctx, store, hrProject, hrUpdate, &out); err == nil || len(f.calls) != 0 || out.Len() != 0 {
		t.Fatal("deleted an update that is not its own:", err, f.calls)
	}

	store = &fakeHealthStore{post: ref}
	f = &fakeDeleter{}
	withDeleter(t, f)
	out.Reset()
	if err := healthRunDelete(ctx, store, hrProject, hrUpdate, &out); err != nil || len(store.recorded) != 1 || strings.Join(f.calls, ",") != "delete "+hrUpdate+",read" || !strings.Contains(out.String(), `"recorded":true`) {
		t.Fatal(err, store.recorded, f.calls, out.String())
	}

	for name, f := range map[string]*fakeDeleter{
		"still there":            {nodes: []string{hrUpdate}},
		"delete failed, gone":    {deleteErr: &linearprovider.Error{Kind: "unavailable"}},
		"delete failed, there":   {deleteErr: &linearprovider.Error{Kind: "unavailable"}, nodes: []string{hrUpdate}},
		"read failed":            {readErr: errors.New("query_failed")},
		"read answered nothing":  {noField: true},
		"read answered another":  {nodes: []string{"e0000000-0000-4000-8000-0000000000b2"}},
		"read answered too many": {nodes: []string{hrUpdate, hrUpdate}},
	} {
		store = &fakeHealthStore{post: ref}
		withDeleter(t, f)
		out.Reset()
		if err := healthRunDelete(ctx, store, hrProject, hrUpdate, &out); err == nil || !strings.HasPrefix(err.Error(), "ALERT") || len(store.recorded) != 0 || !strings.Contains(out.String(), `"recorded":false`) {
			t.Errorf("%s: %v %v %s", name, err, store.recorded, out.String())
		}
	}

	store = &fakeHealthStore{post: ref, recordErr: errors.New("authority unavailable")}
	withDeleter(t, &fakeDeleter{})
	out.Reset()
	if err := healthRunDelete(ctx, store, hrProject, hrUpdate, &out); err == nil || !strings.Contains(err.Error(), "could not be recorded") || !strings.Contains(out.String(), `"recorded":false`) {
		t.Fatal(err, out.String())
	}
}

// The by-ID read is a constant read-only query the app's GraphQL accepts.
func TestHealthUpdateQueryIsReadOnly(t *testing.T) {
	if !strings.HasPrefix(healthUpdateQuery, "query ") || strings.Contains(strings.ToLower(healthUpdateQuery), "mutation") || !strings.Contains(healthUpdateQuery, "filter: { id: { eq: $id } }") {
		t.Fatal(healthUpdateQuery)
	}
}
