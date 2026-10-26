package watcher

import (
	"context"
	"testing"
)

// The issue facts come from the app's own read, and only a complete,
// matching answer is authoritative.
func TestIssueFacts(t *testing.T) {
	e := setup(t)
	const proj = "33333333-3333-4333-8333-333333333333"
	labels := func(n int) []any {
		out := []any{}
		for i := 0; i < n; i++ {
			out = append(out, map[string]any{"name": "l"})
		}
		return out
	}
	e.f.dispatchIssues = map[string]any{
		issueA:     map[string]any{"id": issueA, "project": map[string]any{"id": proj}, "labels": map[string]any{"nodes": []any{map[string]any{"name": "payments"}}}, "state": map[string]any{"type": "started"}},
		issueB:     map[string]any{"id": issueOut, "project": nil, "labels": map[string]any{"nodes": []any{}}, "state": map[string]any{"type": "started"}},
		issueOut:   map[string]any{"id": issueOut, "project": nil, "labels": map[string]any{"nodes": labels(50)}, "state": map[string]any{"type": "started"}},
		humanNote:  map[string]any{"id": humanNote, "project": nil, "labels": map[string]any{"nodes": []any{}}, "state": nil},
		labelNeeds: map[string]any{"id": labelNeeds, "project": nil, "labels": map[string]any{"nodes": []any{}}, "state": map[string]any{"type": ""}},
	}
	got, err := e.w.IssueFacts(context.Background(), issueA)
	if err != nil || !got.Authoritative || got.ProjectID != proj || got.StateType != "started" || len(got.Labels) != 1 {
		t.Fatal(got, err)
	}
	for name, id := range map[string]string{"another issue": issueB, "a cut label list": issueOut, "no state": humanNote, "empty state type": labelNeeds, "unreadable": strangerID, "not a UUID": "FIX-1"} {
		if got, err = e.w.IssueFacts(context.Background(), id); err == nil || got.Authoritative {
			t.Fatal(name, got)
		}
	}
	last := e.f.queries[len(e.f.queries)-1]
	if last != dispatchIssueQuery {
		t.Fatal("not the constant query:", last)
	}
}
