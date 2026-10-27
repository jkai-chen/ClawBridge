package bus

import (
	"context"
	"strings"
	"testing"
)

// A git remote is stored normalized (host and path only), so no output
// that carries it, the project list, the project itself, its overview or
// its history, can show a user, password or token from the remote given.
func TestGitRemoteCredentialsNeverStoredOrShown(t *testing.T) {
	h := newHarness(t)
	created := h.call("project_create", "", map[string]any{"slug": "cred", "git_remote": "https://user:secret-token@git.example.com/org/repo.git"})
	if asJSON(created) == "" || strings.Contains(asJSON(created), "secret-token") {
		t.Fatal(created)
	}
	if got := created.(map[string]any)["git_remote"]; got != "git.example.com/org/repo" {
		t.Fatal(got)
	}
	ssh := h.call("project_create", "", map[string]any{"slug": "cred2", "git_remote": "ssh://user:secret-token@git.example.com:22/org/repo"})
	if got := ssh.(map[string]any)["git_remote"]; got != "git.example.com:22/org/repo" {
		t.Fatal(got)
	}
	run := func(r Request) any {
		t.Helper()
		got, err := h.s.Execute(context.Background(), r)
		if err != nil {
			t.Fatal(r.Operation, err)
		}
		return got.Data
	}
	for name, out := range map[string]any{
		"list_projects": h.call("list_projects", "", nil),
		"whoami":        run(Request{Operation: "whoami", Workspace: "w", Project: "cred", Args: map[string]any{}}),
		"overview":      run(Request{Operation: "project_overview", Workspace: "w", Project: "cred", Args: map[string]any{}}),
		"history":       run(Request{Operation: "history", Workspace: "w", Project: "cred", Args: map[string]any{}}),
	} {
		text := asJSON(out)
		if strings.Contains(text, "secret-token") || strings.Contains(text, "user:") || strings.Contains(text, "://") {
			t.Fatal(name, text)
		}
	}
	// A remote with a query or fragment (where a token could also hide) is refused.
	h.fail("project_create", "", map[string]any{"slug": "cred3", "git_remote": "https://git.example.com/org/repo.git?private_token=secret-token"})
}
