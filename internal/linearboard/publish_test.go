package linearboard

import (
	"context"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"clawbridge/internal/bus"
	"clawbridge/internal/linearprovider"
)

// pubFake adds the document reads to the seed fake, and lets a test edit
// a document as Linear reads it back.
type pubFake struct {
	*docFake
	gets  map[string]int
	onGet func(id string, n int)
}

func (f *pubFake) GetDocument(ctx context.Context, id string) (linearprovider.Document, error) {
	f.gets[id]++
	if f.onGet != nil {
		f.onGet(id, f.gets[id])
	}
	return f.docFake.GetDocument(ctx, id)
}
func (f *pubFake) ReadDocumentWithParents(context.Context, string) (linearprovider.ReadDocument, error) {
	return linearprovider.ReadDocument{}, nil
}
func (f *pubFake) DocumentSummaries(context.Context, string, string, string, int) (linearprovider.Page[linearprovider.DocSummary], int, error) {
	return linearprovider.Page[linearprovider.DocSummary]{Complete: true}, 0, nil
}
func (f *pubFake) InitiativeProjects(context.Context, string) (linearprovider.Initiative, []linearprovider.Project, error) {
	return linearprovider.Initiative{}, nil, nil
}

func pubGit(dir string, trimmed bool, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com")
	out, err := cmd.Output()
	if trimmed {
		return strings.TrimSpace(string(out)), err
	}
	return string(out), err
}

type pubEnv struct {
	*seedEnv
	pf         *pubFake
	home, root string
}

// pubTest is the seed env with a source repository under a fake home and
// a document policy whose source_root is it.
func pubTest(t *testing.T) *pubEnv {
	t.Helper()
	env := &pubEnv{seedEnv: seedTest(t)}
	env.pf = &pubFake{docFake: env.f, gets: map[string]int{}}
	env.s.cfg.Open = func(context.Context, *http.Client, bool) (Provider, error) { env.opens++; return env.pf, nil }
	env.home = t.TempDir()
	env.root = filepath.Join(env.home, "src", "example")
	if err := os.MkdirAll(env.root, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := pubGit(env.root, true, "init", "-q", "-b", "main"); err != nil {
		t.Fatal(err)
	}
	policy := map[string]any{"source_root": "~/src/example", "deny_patterns": []any{map[string]any{"id": "internal-name", "regex": `internal-name-[0-9]+`}}, "initiatives": []any{}}
	if _, err := env.run(context.Background(), bus.Request{Operation: "doc_policy_set", Workspace: "w", Project: "p", Args: map[string]any{"expected_revision": 0, "doc_policy": policy}}); err != nil {
		t.Fatal(err)
	}
	return env
}

func (e *pubEnv) commit(t *testing.T, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(e.root, "plan.md"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"add", "plan.md"}, {"commit", "-q", "-m", "plan"}} {
		if _, err := pubGit(e.root, true, args...); err != nil {
			t.Fatal(args, err)
		}
	}
}

func (e *pubEnv) publish(t *testing.T, in PublishInput) PublishResult {
	t.Helper()
	in.File, in.Home, in.Git = filepath.Join(e.root, "plan.md"), e.home, pubGit
	if in.ParentKind == "" {
		in.ParentKind, in.ParentID = "team", teamID
	}
	if in.OperationID == "" {
		in.OperationID = "publish-1"
	}
	r, err := e.s.PublishV2(context.Background(), e.scope, in)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func parts(r PublishResult) string {
	var out []string
	for _, p := range r.Parts {
		s := p.Action
		if p.Outcome != "" {
			s += "/" + p.Outcome
		}
		out = append(out, s)
	}
	return strings.Join(out, " ")
}

func (e *pubEnv) creates() int {
	n := 0
	for _, c := range e.f.calls {
		if strings.HasPrefix(c, "create ") || strings.HasPrefix(c, "update ") {
			n++
		}
	}
	return n
}

// A preview writes nothing; a publish creates the document from the
// committed bytes with the footer, reads it back and records it; the same
// source again does nothing; a new commit updates the untouched document.
func TestPublishCreateNoneUpdate(t *testing.T) {
	e := pubTest(t)
	e.commit(t, "# Plan\n\nfirst\n")
	if r := e.publish(t, PublishInput{}); parts(r) != "create" || r.Confirmed || e.creates() != 0 || r.SourcePath != "plan.md" {
		t.Fatal("preview", parts(r), r)
	}
	r := e.publish(t, PublishInput{Confirm: true})
	if parts(r) != "create/created" || !r.Confirmed {
		t.Fatal(parts(r), r)
	}
	doc := e.f.docs[r.Parts[0].DocumentID]
	if doc.Title != "Plan" || !strings.Contains(doc.Content, "first") || !strings.Contains(doc.Content, "Published by ClawBridge from example@") || strings.Contains(doc.Content, e.home) {
		t.Fatal(doc)
	}
	if r = e.publish(t, PublishInput{Confirm: true}); parts(r) != "none" || e.creates() != 1 {
		t.Fatal("unchanged source", parts(r))
	}
	e.commit(t, "# Plan\n\nsecond\n")
	if r = e.publish(t, PublishInput{Confirm: true}); parts(r) != "update/updated" || !strings.Contains(doc.Content, "second") {
		t.Fatal("update", parts(r), doc.Content)
	}
	if r = e.publish(t, PublishInput{Confirm: true}); parts(r) != "none" {
		t.Fatal("the update was not recorded", parts(r))
	}
	// Two ordered lists back to back: a warning, never a refusal.
	e.commit(t, "# Plan\n\n1. a\n2. b\n\n1. c\n")
	if r = e.publish(t, PublishInput{Confirm: true}); parts(r) != "update/updated" || len(r.Warnings) != 1 || !strings.Contains(r.Warnings[0], "line 6") || r.Refused != "" {
		t.Fatal("merged lists", parts(r), r.Warnings, r.Refused)
	}
}

// A document edited in Linear is never overwritten: the publish is refused
// until the owner recreates it deliberately. An edit landing between the
// plan's read and the update's own read is caught too.
func TestPublishNeverOverwritesAnEdit(t *testing.T) {
	e := pubTest(t)
	e.commit(t, "# Plan\n\nfirst\n")
	id := e.publish(t, PublishInput{Confirm: true}).Parts[0].DocumentID
	e.f.docs[id].Content += "\na person's note\n"
	e.commit(t, "# Plan\n\nsecond\n")
	r := e.publish(t, PublishInput{Confirm: true})
	if parts(r) != "edited" || r.Refused == "" || e.creates() != 1 || strings.Contains(e.f.docs[id].Content, "second") {
		t.Fatal("an edit was overwritten", parts(r), r.Refused)
	}
	r = e.publish(t, PublishInput{Confirm: true, Recreate: true})
	if parts(r) != "create/created" || r.Parts[0].DocumentID == id || !strings.Contains(e.f.docs[id].Content, "a person's note") {
		t.Fatal("recreate", parts(r))
	}
	// Drift: the document changes after the plan read it.
	fresh := r.Parts[0].DocumentID
	e.commit(t, "# Plan\n\nthird\n")
	e.pf.gets = map[string]int{}
	e.pf.onGet = func(doc string, n int) {
		if doc == fresh && n == 2 {
			e.f.docs[fresh].Content += "\nlate edit\n"
		}
	}
	if r = e.publish(t, PublishInput{Confirm: true}); parts(r) != "update/drifted" || strings.Contains(e.f.docs[fresh].Content, "third") {
		t.Fatal("drift", parts(r))
	}
}

// A deny pattern refuses the publish, naming the pattern and line only;
// the owner may publish over it for one operation, which is audited.
func TestPublishDenyScanAndOverride(t *testing.T) {
	e := pubTest(t)
	e.commit(t, "# Plan\n\nsee internal-name-42\n")
	r := e.publish(t, PublishInput{Confirm: true})
	if r.Refused == "" || len(r.Hits) != 1 || r.Hits[0] != (PublishHit{Pattern: "internal-name", Where: "content", Line: 3}) || e.creates() != 0 || len(r.Parts) != 0 {
		t.Fatal("a deny hit was published", r)
	}
	if strings.Contains(r.Refused, "internal-name-42") {
		t.Fatal("the refusal shows the matched text")
	}
	if _, err := e.s.PublishV2(context.Background(), e.scope, PublishInput{File: filepath.Join(e.root, "plan.md"), Home: e.home, Git: pubGit, ParentKind: "team", ParentID: teamID, OperationID: "op", AllowPatterns: []string{"internal-name"}}); err == nil {
		t.Fatal("--allow-pattern without --confirm")
	}
	r = e.publish(t, PublishInput{Confirm: true, AllowPatterns: []string{"internal-name"}, OperationID: "publish-override"})
	if parts(r) != "create/created" || len(r.Overridden) != 1 {
		t.Fatal("override", parts(r), r.Refused)
	}
	history, err := e.run(context.Background(), bus.Request{Operation: "history", Workspace: "w", Project: "p", Args: map[string]any{"entity": "linear_doc_publish_override", "key": "publish-override"}})
	if err != nil || len(anyRows(history.Data.(map[string]any)["events"])) != 1 {
		t.Fatal("the override is not audited", history.Data, err)
	}
}

// An over-size file is refused unless --split; split parts are created
// and a change in their number is refused without --recreate.
func TestPublishSizeAndSplit(t *testing.T) {
	e := pubTest(t)
	section := func(name string) string { return "## " + name + "\n" + strings.Repeat("word ", 6000) + "\n" }
	e.commit(t, "# Big\n"+section("A")+section("B"))
	if r := e.publish(t, PublishInput{Confirm: true}); r.Refused == "" || !strings.Contains(r.Refused, "--split") || e.creates() != 0 {
		t.Fatal("over-size", r)
	}
	r := e.publish(t, PublishInput{Confirm: true, Split: true})
	if parts(r) != "create/created create/created" || r.Parts[0].Title != "Big (1/2)" {
		t.Fatal(parts(r), r)
	}
	e.commit(t, "# Big\n"+section("A")+section("B")+section("C"))
	if r = e.publish(t, PublishInput{Confirm: true, Split: true}); !strings.Contains(r.Refused, "--recreate") {
		t.Fatal("a part count change", parts(r), r.Refused)
	}
}

// A create whose answer is lost is found again by its token.
func TestPublishSettlesALostCreate(t *testing.T) {
	e := pubTest(t)
	e.commit(t, "# Plan\n\nfirst\n")
	e.f.createLost = true
	if r := e.publish(t, PublishInput{Confirm: true}); parts(r) != "create/uncertain" {
		t.Fatal(parts(r))
	}
	e.f.createLost = false
	if r := e.publish(t, PublishInput{Confirm: true}); parts(r) != "settle/settled" || len(e.f.order) != 1 {
		t.Fatal("the lost create was sent again", parts(r), e.f.order)
	}
}

// The parent must be in scope and the caller may not be a plain member.
func TestPublishRefusesOutsideTheBinding(t *testing.T) {
	e := pubTest(t)
	e.commit(t, "# Plan\n")
	for name, in := range map[string]PublishInput{
		"unbound team":   {ParentKind: "team", ParentID: unboundTeamID},
		"issue parent":   {ParentKind: "issue", ParentID: issueID},
		"bad operation":  {OperationID: "has space"},
		"outside source": {Commit: "zzz"},
	} {
		in.File, in.Home, in.Git = filepath.Join(e.root, "plan.md"), e.home, pubGit
		if in.ParentKind == "" {
			in.ParentKind, in.ParentID = "team", teamID
		}
		if in.OperationID == "" {
			in.OperationID = "op"
		}
		in.Confirm = true
		if r, err := e.s.PublishV2(context.Background(), e.scope, in); err == nil && r.Refused == "" {
			t.Fatal(name, "published", parts(r))
		}
	}
	if e.creates() != 0 {
		t.Fatal("a refused publish wrote")
	}
}

// An initiative the policy lists, which the bus accepts, is still proven
// in Linear: one holding no bound project is refused before any write.
func TestPublishProvesAnInitiativeInLinear(t *testing.T) {
	e := pubTest(t)
	policy := map[string]any{"source_root": "~/src/example", "deny_patterns": []any{}, "initiatives": []any{initiativeA}}
	if _, err := e.run(context.Background(), bus.Request{Operation: "doc_policy_set", Workspace: "w", Project: "p", Args: map[string]any{"expected_revision": 1, "doc_policy": policy}}); err != nil {
		t.Fatal(err)
	}
	e.commit(t, "# Plan\n")
	_, err := e.s.PublishV2(context.Background(), e.scope, PublishInput{File: filepath.Join(e.root, "plan.md"), Home: e.home, Git: pubGit, ParentKind: "initiative", ParentID: initiativeA, OperationID: "op", Confirm: true})
	if err == nil || !strings.Contains(err.Error(), "outside_scope") || e.creates() != 0 {
		t.Fatal("an initiative without a bound project was published to:", err)
	}
}
