package docpublish

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func execGit(dir string, trimmed bool, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com")
	out, err := cmd.Output()
	if trimmed {
		return strings.TrimSpace(string(out)), err
	}
	return string(out), err
}

// repo makes a source repository under a fake home: plan.md committed
// twice, a nested repository inside it, and a file outside it.
func repo(t *testing.T) (home, root string, first string) {
	t.Helper()
	home = t.TempDir()
	root = filepath.Join(home, "src", "example")
	run := func(dir string, args ...string) string {
		t.Helper()
		out, err := execGit(dir, true, args...)
		if err != nil {
			t.Fatal(args, err)
		}
		return out
	}
	write := func(path, body string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(filepath.Join(root, "docs", "plan.md"), "# Plan\n\nfirst\n")
	run(home, "init", "-q", "-b", "main", root)
	run(root, "add", ".")
	run(root, "commit", "-q", "-m", "first")
	first = run(root, "rev-parse", "HEAD")
	write(filepath.Join(root, "docs", "plan.md"), "# Plan\n\nsecond\n")
	run(root, "commit", "-q", "-am", "second")
	nested := filepath.Join(root, "vendor", "inner")
	write(filepath.Join(nested, "x.md"), "inner\n")
	run(home, "init", "-q", "-b", "main", nested)
	run(nested, "add", ".")
	run(nested, "commit", "-q", "-m", "inner")
	write(filepath.Join(home, "elsewhere.md"), "outside\n")
	return home, root, first
}

func TestReadProvesTheSourceAndReadsTheCommit(t *testing.T) {
	home, root, first := repo(t)
	plan := filepath.Join(root, "docs", "plan.md")
	s, err := Read(execGit, "~/src/example", home, plan, "")
	if err != nil || s.Path != "docs/plan.md" || s.Repo != "example" || s.Content != "# Plan\n\nsecond\n" || s.WorkingDiffers || len(s.Short) != 12 || len(s.SHA256) != 64 {
		t.Fatal(s, err)
	}
	// The bytes come from the commit, never the working tree.
	if err = os.WriteFile(plan, []byte("# Plan\n\nuncommitted secret\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if s, err = Read(execGit, "~/src/example", home, plan, ""); err != nil || strings.Contains(s.Content, "uncommitted") || !s.WorkingDiffers {
		t.Fatal("published the working tree", s, err)
	}
	if s, err = Read(execGit, root, home, plan, first); err != nil || s.Content != "# Plan\n\nfirst\n" || s.Commit != first {
		t.Fatal("--commit", s, err)
	}
	for name, c := range map[string]struct{ root, file, commit string }{
		"no root":          {"", plan, ""},
		"missing root":     {"~/src/missing", plan, ""},
		"outside the root": {"~/src/example", filepath.Join(home, "elsewhere.md"), ""},
		"nested repo":      {"~/src/example", filepath.Join(root, "vendor", "inner", "x.md"), ""},
		"root not top":     {"~/src/example/docs", plan, ""},
		"not an ancestor":  {"~/src/example", plan, strings.Repeat("0", 40)},
		"bad commit":       {"~/src/example", plan, "HEAD~1"},
		"uncommitted file": {"~/src/example", filepath.Join(root, "docs", "new.md"), ""},
	} {
		if name == "uncommitted file" {
			if err := os.WriteFile(c.file, []byte("new\n"), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := Read(execGit, c.root, home, c.file, c.commit); err == nil {
			t.Fatal(name, "accepted")
		}
	}
	// A symbolic link that leaves the root is judged by its target.
	link := filepath.Join(root, "docs", "link.md")
	if err := os.Symlink(filepath.Join(home, "elsewhere.md"), link); err != nil {
		t.Fatal(err)
	}
	if _, err := Read(execGit, "~/src/example", home, link, ""); err == nil {
		t.Fatal("a link out of the root was read")
	}
}

func TestScanNamesPatternAndLineOnly(t *testing.T) {
	text := "intro\nsee internal-name-42 here\nok\n" + Footer(Source{Repo: "internal-name-7", Short: "abc", Path: "p.md"}, "0123456789ab")
	hits, err := Scan(text, []Pattern{{"internal-name", `internal-name-[0-9]+`}, {"never", `nomatch`}})
	if err != nil || len(hits) != 2 || hits[0] != (Hit{"internal-name", 2}) || hits[1].Line < 5 {
		t.Fatal("the footer must be scanned too", hits, err)
	}
	if _, err = Scan("x", []Pattern{{"bad", "("}}); err == nil {
		t.Fatal("a bad pattern compiled")
	}
}

func TestCheckContentAndFooter(t *testing.T) {
	if CheckContent("ok\n") != nil || CheckContent("a\x00b") == nil || CheckContent("\xff") == nil {
		t.Fatal("content checks")
	}
	f := Footer(Source{Repo: "example", Short: "0123456789ab", Path: "docs/plan.md"}, "aaaaaaaaaaaa")
	if !strings.Contains(f, "example@0123456789ab:docs/plan.md") || !HasToken("x"+f, "aaaaaaaaaaaa") || HasToken(f, "bbbbbbbbbbbb") || strings.Contains(f, "/Users/") {
		t.Fatal(f)
	}
	if Title("", "intro\n# The Plan\n", "docs/plan.md") != "The Plan" || Title(" Given ", "# X", "p.md") != "Given" || Title("", "none", "docs/plan.md") != "plan" {
		t.Fatal("titles")
	}
}

func TestSplitByHeadingOnly(t *testing.T) {
	section := func(name string, n int) string { return "## " + name + "\n" + strings.Repeat("x", n) + "\n" }
	content := "# Doc\n" + section("A", 400) + section("B", 400) + section("C", 400)
	parts, err := Split("Doc", content, 1200, 100)
	if err != nil || len(parts) != 2 || parts[0].Title != "Doc (1/2)" || !strings.HasPrefix(parts[0].Body, "Contents:") || !strings.Contains(parts[0].Body, "Doc (2/2)") {
		t.Fatal(parts, err)
	}
	for _, p := range parts {
		if len(p.Body)+100 > 1200 {
			t.Fatal("a part is over the limit", len(p.Body))
		}
	}
	if strings.Join([]string{strings.SplitN(parts[0].Body, "\n\n", 3)[2], parts[1].Body}, "") != content {
		t.Fatal("the parts do not rebuild the content")
	}
	if _, err = Split("Doc", "# Doc\n"+section("A", 2000)+section("B", 10), 1200, 100); err == nil {
		t.Fatal("a section over the limit was split")
	}
	if _, err = Split("Doc", strings.Repeat("x", 5000), 1200, 100); err == nil {
		t.Fatal("content without headings was split")
	}
}

// An ordered list that restarts right after another is reported by line; a
// separating paragraph or heading, a continuing list, and code are not.
func TestMergedLists(t *testing.T) {
	for content, want := range map[string]string{
		"1. a\n2. b\n\n1. c\n2. d\n":            "[4]",
		"1. a\n2. b\n1. c\n":                    "[3]",
		"1. a\n   more\n\n1) b\n":               "[4]",
		"1. a\n2. b\n\nText between.\n\n1. c\n": "[]",
		"1. a\n2. b\n\n## Next\n\n1. c\n":       "[]",
		"1. a\n2. b\n3. c\n":                    "[]",
		"1. a\n- bullet\n1. b\n":                "[]",
		"```\n1. a\n1. b\n```\n":                "[]",
		"3. a\n\n1. b\n\n1. c\n":                "[3 5]",
	} {
		if got := fmt.Sprint(MergedLists(content)); got != want && !(want == "[]" && got == "[]") {
			t.Fatalf("%q: got %s, want %s", content, got, want)
		}
	}
}
