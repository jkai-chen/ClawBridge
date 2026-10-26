// Package docpublish prepares a file from git for `clawbridge linear doc
// publish` (live/LINEAR-DOCS-DESIGN.md §2.2–2.4), before any Linear call:
// it proves the file is in the policy's source root, in that repository
// itself, reads the exact bytes at one commit with git show, checks them,
// renders the footer, scans every line against the deny patterns, and
// splits by heading only when asked. All git here runs in the owner's own
// source repository, which is trusted.
package docpublish

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"unicode/utf8"
)

// MaxContent is the most rendered content one document takes: Linear
// accepted 49,075 bytes in the size probe (§5a).
const MaxContent = 49_000

// Git runs git in a directory and returns its output; trimmed is false for
// git show, whose bytes are published exactly.
type Git func(dir string, trimmed bool, args ...string) (string, error)

// Pattern is one deny pattern of the document policy.
type Pattern struct {
	ID    string
	Regex string
}

// Source is a file as it will be published.
type Source struct {
	// Root is the source repository's real top level; Repo its directory
	// name, the only part of a local path a document ever shows.
	Root, Repo string
	// Path is the file's path inside the repository, with forward slashes.
	Path string
	// Commit is the full commit the bytes were read at; Short its first
	// twelve characters.
	Commit, Short string
	Content       string
	SHA256        string
	// WorkingDiffers is set when the working copy is not what is published.
	WorkingDiffers bool
}

var commitPattern = regexp.MustCompile(`^[0-9a-f]{7,64}$`)

// expand resolves a leading ~ against home.
func expand(p, home string) string {
	if p == "~" {
		return home
	}
	if strings.HasPrefix(p, "~/") {
		return filepath.Join(home, p[2:])
	}
	return p
}

func within(p, base string) bool {
	rel, err := filepath.Rel(base, p)
	return err == nil && rel != "." && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// Read proves file is in the source root's repository and reads it at
// commit (HEAD when empty; otherwise an ancestor of HEAD).
func Read(git Git, sourceRoot, home, file, commit string) (Source, error) {
	if sourceRoot == "" {
		return Source{}, errors.New("the document policy has no source_root; the project owner sets one with linear doc-policy set")
	}
	root, err := filepath.EvalSymlinks(expand(sourceRoot, home))
	if err != nil {
		return Source{}, errors.New("the policy's source_root does not exist on this machine")
	}
	abs, err := filepath.Abs(file)
	if err != nil {
		return Source{}, err
	}
	real, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return Source{}, errors.New("the file does not exist")
	}
	if !within(real, root) {
		return Source{}, errors.New("the file is outside the policy's source_root")
	}
	top, err := git(root, true, "rev-parse", "--show-toplevel")
	if err != nil {
		return Source{}, errors.New("the policy's source_root is not a git repository")
	}
	if top, err = filepath.EvalSymlinks(top); err != nil || top != root {
		return Source{}, errors.New("the policy's source_root must be the top level of its git repository")
	}
	// The file's own repository must be the root's: no submodule and no
	// nested repository.
	fileTop, err := git(filepath.Dir(real), true, "rev-parse", "--show-toplevel")
	if err != nil {
		return Source{}, errors.New("the file is not in a git repository")
	}
	if fileTop, err = filepath.EvalSymlinks(fileTop); err != nil || fileTop != root {
		return Source{}, errors.New("the file is in a submodule or a nested repository, not in the source_root's own")
	}
	rel, err := filepath.Rel(root, real)
	if err != nil {
		return Source{}, err
	}
	path := filepath.ToSlash(rel)
	head, err := git(root, true, "rev-parse", "--verify", "HEAD^{commit}")
	if err != nil {
		return Source{}, errors.New("the source repository has no commit")
	}
	at := head
	if commit != "" {
		if !commitPattern.MatchString(commit) {
			return Source{}, errors.New("--commit must be a commit ID")
		}
		if at, err = git(root, true, "rev-parse", "--verify", commit+"^{commit}"); err != nil {
			return Source{}, errors.New("--commit does not name a commit of the source repository")
		}
		if _, err = git(root, true, "merge-base", "--is-ancestor", at, head); err != nil {
			return Source{}, errors.New("--commit must be HEAD or one of its ancestors")
		}
	}
	content, err := git(root, false, "show", at+":"+path)
	if err != nil {
		return Source{}, fmt.Errorf("%s is not in commit %s; commit it first", path, at[:12])
	}
	sum := sha256.Sum256([]byte(content))
	s := Source{Root: root, Repo: filepath.Base(root), Path: path, Commit: at, Short: at[:12], Content: content, SHA256: hex.EncodeToString(sum[:])}
	if working, err := os.ReadFile(real); err != nil || string(working) != content {
		s.WorkingDiffers = true
	}
	return s, nil
}

// CheckContent refuses what Linear should never be sent: invalid UTF-8,
// or a NUL.
func CheckContent(content string) error {
	if !utf8.ValidString(content) {
		return errors.New("the file is not valid UTF-8")
	}
	if strings.IndexByte(content, 0) >= 0 {
		return errors.New("the file contains a NUL byte")
	}
	return nil
}

// NewToken is a publish token: 12 lowercase hex characters.
func NewToken() (string, error) {
	var b [6]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

var tokenPattern = regexp.MustCompile(`^[0-9a-f]{12}$`)

// TokenLine is the footer line that carries a publish token.
func TokenLine(token string) string { return "publish: " + token }

// HasToken reports whether content, as Linear returns it, carries token.
func HasToken(content, token string) bool {
	return tokenPattern.MatchString(token) && strings.Contains(content, TokenLine(token))
}

// Footer is what every published part ends with. It names the repository
// by its directory name and the path inside it, never a local path.
func Footer(s Source, token string) string {
	return "\n\n---\n\nPublished by ClawBridge from " + s.Repo + "@" + s.Short + ":" + s.Path + ". Edit it in git: an edit here blocks the next publish.\n\n" + TokenLine(token) + "\n"
}

// Hit is one deny pattern's match: its ID and the 1-based line, never the
// matched text.
type Hit struct {
	Pattern string `json:"pattern"`
	Line    int    `json:"line"`
}

// Scan checks every line of the rendered text against every pattern.
func Scan(text string, patterns []Pattern) ([]Hit, error) {
	var hits []Hit
	compiled := make([]*regexp.Regexp, len(patterns))
	for i, p := range patterns {
		re, err := regexp.Compile(p.Regex)
		if err != nil {
			return nil, fmt.Errorf("deny pattern %s does not compile", p.ID)
		}
		compiled[i] = re
	}
	for n, line := range strings.Split(text, "\n") {
		for i, re := range compiled {
			if re.MatchString(line) {
				hits = append(hits, Hit{Pattern: patterns[i].ID, Line: n + 1})
			}
		}
	}
	return hits, nil
}

// Part is one document of a publish.
type Part struct {
	Title string
	Body  string
}

// Split cuts content at "## " headings into parts whose bodies, with room
// for the footer, fit within max. Part 1 starts with a contents list that
// names the others. It refuses a single section over the limit, and
// content with no heading to split at.
func Split(title, content string, max, footer int) ([]Part, error) {
	lines := strings.SplitAfter(content, "\n")
	var sections []string
	current := ""
	for _, line := range lines {
		if strings.HasPrefix(line, "## ") && current != "" {
			sections = append(sections, current)
			current = ""
		}
		current += line
	}
	if current != "" {
		sections = append(sections, current)
	}
	if len(sections) < 2 {
		return nil, errors.New("the file has no ## heading to split at")
	}
	// Room for the contents list in part 1: a line per part.
	contents := func(n int) int { return 64 + n*(len(title)+32) }
	var bodies []string
	body := ""
	for _, section := range sections {
		budget := max - footer
		if len(bodies) == 0 {
			budget -= contents(len(sections))
		}
		if len(section) > budget {
			return nil, errors.New("one section is over the size limit by itself; split it in git")
		}
		if body != "" && len(body)+len(section) > budget {
			bodies = append(bodies, body)
			body = ""
		}
		body += section
	}
	bodies = append(bodies, body)
	n := len(bodies)
	parts := make([]Part, n)
	for i, b := range bodies {
		parts[i] = Part{Title: fmt.Sprintf("%s (%d/%d)", title, i+1, n), Body: b}
	}
	var list strings.Builder
	list.WriteString("Contents:\n\n")
	for i := range parts {
		fmt.Fprintf(&list, "%d. %s\n", i+1, parts[i].Title)
	}
	parts[0].Body = list.String() + "\n" + parts[0].Body
	return parts, nil
}

// Title is the document's title: the given one, else the file's first
// "# " heading, else the file's name.
func Title(given, content, path string) string {
	if t := strings.TrimSpace(given); t != "" {
		return t
	}
	for _, line := range strings.Split(content, "\n") {
		if strings.HasPrefix(line, "# ") {
			if t := strings.TrimSpace(line[2:]); t != "" {
				return t
			}
		}
	}
	return strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
}

var orderedItem = regexp.MustCompile(`^(\d{1,9})[.)][ \t]`)

// MergedLists returns the 1-based lines where an ordered list restarts its
// numbering right after another ordered list, with only blank or indented
// lines between. Markdown, and Linear with it, merges the two into one list
// and renumbers it, which a document's author rarely means. Fenced code is
// skipped.
func MergedLists(content string) []int {
	var lines []int
	inList, fenced := false, false
	last := 0
	for n, line := range strings.Split(content, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "```") || strings.HasPrefix(trimmed, "~~~") {
			fenced = !fenced
			inList = false
			continue
		}
		if fenced {
			continue
		}
		if m := orderedItem.FindStringSubmatch(line); m != nil {
			num := 0
			fmt.Sscan(m[1], &num)
			if inList && num <= last {
				lines = append(lines, n+1)
			}
			inList, last = true, num
			continue
		}
		// Blank and indented lines continue a list; anything else ends it.
		if trimmed != "" && line[0] != ' ' && line[0] != '\t' {
			inList = false
		}
	}
	return lines
}
