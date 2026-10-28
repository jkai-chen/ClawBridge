package playbook

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func run(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null", "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.test", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.test")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

// checkout makes a clone of an origin whose default branch has the files
// given, so origin/HEAD names it.
func checkout(t *testing.T, files map[string]string) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	base := t.TempDir()
	origin := filepath.Join(base, "origin")
	if err := os.MkdirAll(filepath.Join(origin, "docs"), 0o755); err != nil {
		t.Fatal(err)
	}
	run(t, origin, "init", "-q", "-b", "main")
	if err := os.WriteFile(filepath.Join(origin, "README.md"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(origin, filepath.FromSlash(name)), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	run(t, origin, "add", "-A")
	run(t, origin, "commit", "-q", "-m", "init")
	work := filepath.Join(base, "work")
	run(t, base, "clone", "-q", origin, work)
	return work
}

func coordinatorWhoami() map[string]any {
	return map[string]any{"project": map[string]any{"coordinator_session": "ses_1"}, "session": map[string]any{"id": "ses_1"}}
}

// The playbook counts only as merged on the default branch: a working-tree
// copy is reported, never taken for the merged one.
func TestFindReadsTheDefaultBranch(t *testing.T) {
	ctx := context.Background()
	merged := checkout(t, map[string]string{Path: "rules\n"})
	if s := Find(ctx, filepath.Join(merged, "docs")); !s.Merged || !s.Local || s.Ref != "origin/main" || s.Err != nil {
		t.Fatalf("%+v", s)
	}
	none := checkout(t, nil)
	if s := Find(ctx, none); s.Merged || s.Local || s.Ref != "origin/main" {
		t.Fatalf("%+v", s)
	}
	// An edit that isn't merged: here, but not on origin/main.
	if err := os.MkdirAll(filepath.Join(none, "docs"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(none, filepath.FromSlash(Path)), []byte("my own rules\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if s := Find(ctx, none); s.Merged || !s.Local {
		t.Fatalf("an unmerged edit counted as merged: %+v", s)
	}
	if s := Find(ctx, t.TempDir()); s.Root != "" || s.Merged || s.Local {
		t.Fatalf("outside a checkout: %+v", s)
	}
}

func TestFindWithoutOriginHEAD(t *testing.T) {
	work := checkout(t, map[string]string{Path: "rules\n"})
	run(t, work, "remote", "set-head", "origin", "--delete")
	s := Find(context.Background(), work)
	if s.Merged || s.Ref != "" || s.Err == nil || !s.Local {
		t.Fatalf("%+v", s)
	}
	if v := s.View(); v["note"] == nil || v["read"] != nil {
		t.Fatal(v)
	}
}

func TestCoordinatorReadsTheProjectsField(t *testing.T) {
	for name, w := range map[string]any{
		"another session": map[string]any{"project": map[string]any{"coordinator_session": "ses_2"}, "session": map[string]any{"id": "ses_1"}},
		"no coordinator":  map[string]any{"project": map[string]any{"coordinator_session": nil}, "session": map[string]any{"id": "ses_1"}},
		"no session":      map[string]any{"project": map[string]any{"coordinator_session": "ses_1"}, "session": nil},
		"workspace only":  map[string]any{"project": nil, "session": nil},
		"empty both":      map[string]any{"project": map[string]any{"coordinator_session": ""}, "session": map[string]any{"id": ""}},
		"not a whoami":    "x",
	} {
		if Coordinator(w) {
			t.Fatal(name)
		}
	}
	if !Coordinator(coordinatorWhoami()) {
		t.Fatal("the coordinator was missed")
	}
}

func TestAnnotateOnlyTheCoordinator(t *testing.T) {
	ctx := context.Background()
	work := checkout(t, map[string]string{Path: "rules\n"})
	other := map[string]any{"project": map[string]any{"coordinator_session": "ses_2"}, "session": map[string]any{"id": "ses_1"}}
	if _, ok := Annotate(ctx, other, work).(map[string]any)["coordinator_playbook"]; ok {
		t.Fatal("another session got the playbook")
	}
	got := Annotate(ctx, coordinatorWhoami(), work).(map[string]any)["coordinator_playbook"].(map[string]any)
	if got["path"] != Path || got["merged"] != true || got["read"] != "git show origin/main:"+Path {
		t.Fatal(got)
	}
	got = Annotate(ctx, coordinatorWhoami(), t.TempDir()).(map[string]any)["coordinator_playbook"].(map[string]any)
	if got["note"] == nil || got["merged"] != nil {
		t.Fatal(got)
	}
}

// origin/HEAD's target is the remote's choice: a ref name a shell would run
// gets no command, only a note.
func TestViewGivesNoCommandForAShellRef(t *testing.T) {
	for _, ref := range []string{"origin/$(touch x)", "origin/a;b", "origin/`id`", "origin/a|b", "origin/a&b", "origin/a b", "-c", "--output=x"} {
		v := State{Root: "/r", Ref: ref}.View()
		if v["read"] != nil || !strings.Contains(v["note"].(string), "no command is given") || v["ref"] != ref {
			t.Fatalf("%q: %v", ref, v)
		}
	}
	v := State{Root: "/r", Ref: "origin/release-1.2_x"}.View()
	if v["read"] != "git show origin/release-1.2_x:"+Path || !strings.Contains(v["note"].(string), "git fetch first") {
		t.Fatal(v)
	}
}
