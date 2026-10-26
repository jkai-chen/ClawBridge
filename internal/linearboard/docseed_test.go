package linearboard

import (
	"context"
	"fmt"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"clawbridge/internal/bus"
	"clawbridge/internal/docseed"
	"clawbridge/internal/linearprovider"
)

// docFake is a V2 provider with Linear's document tools. Like Linear, it
// re-serialises the Markdown it stores, so the content read back is never
// byte-equal to what was sent.
type docFake struct {
	*providerV2
	docs    map[string]*linearprovider.Document
	parents map[string]string
	order   []string
	calls   []string
	// createLost creates the document but loses the answer; createErr
	// refuses without creating; getErr fails every read.
	createLost, createErr, getErr bool
}

func normalise(content string) string { return strings.ReplaceAll(content, "\n\n", "\n\n\n") + "\n" }

func (f *docFake) DocumentReason() string { return "" }
func (f *docFake) CreateDocument(_ context.Context, kind, parent, title, content string) (linearprovider.Document, error) {
	f.calls = append(f.calls, "create "+kind+" "+parent+" "+title)
	if f.createErr {
		return linearprovider.Document{}, &linearprovider.Error{Kind: "provider_rejected"}
	}
	id := fmt.Sprintf("dc000000-0000-4000-8000-%012d", len(f.order)+1)
	f.docs[id] = &linearprovider.Document{ID: id, Title: title, Content: normalise(content)}
	f.parents[id] = kind + ":" + parent
	f.order = append(f.order, id)
	if f.createLost {
		return linearprovider.Document{}, &linearprovider.Error{Kind: "unavailable"}
	}
	return linearprovider.Document{ID: id, Title: title}, nil
}
func (f *docFake) UpdateDocumentContent(_ context.Context, id, content string) (linearprovider.Document, error) {
	f.calls = append(f.calls, "update "+id)
	f.docs[id].Content = normalise(content)
	return linearprovider.Document{ID: id}, nil
}
func (f *docFake) GetDocument(_ context.Context, id string) (linearprovider.Document, error) {
	f.calls = append(f.calls, "get "+id)
	d, ok := f.docs[id]
	if f.getErr || !ok {
		return linearprovider.Document{}, &linearprovider.Error{Kind: "provider_rejected"}
	}
	return *d, nil
}
func (f *docFake) DocumentsUnder(_ context.Context, kind, parent string) ([]linearprovider.Document, bool, error) {
	f.calls = append(f.calls, "list "+kind+" "+parent)
	var out []linearprovider.Document
	for _, id := range f.order {
		if f.parents[id] == kind+":"+parent {
			out = append(out, *f.docs[id])
		}
	}
	return out, true, nil
}

type seedEnv struct {
	s     *Service
	f     *docFake
	scope bus.Request
	opens int
	now   time.Time
	run   func(context.Context, bus.Request) (bus.Result, error)
}

// seedTest links a real local bus to the V2 fakes' board: two teams, the
// default one first, and two projects, so the guides go to the team.
func seedTest(t *testing.T) *seedEnv {
	t.Helper()
	store, e := bus.Open(filepath.Join(t.TempDir(), "bus.db"))
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { store.Close() })
	for _, r := range []bus.Request{
		{Operation: "workspace_init", Workspace: "w", Args: map[string]any{"mode": "local"}},
		{Operation: "project_create", Workspace: "w", Args: map[string]any{"slug": "p"}},
		{Operation: "taskboard_set_v2", Workspace: "w", Project: "p", Args: map[string]any{"expected_revision": 0, "mode": "project_set", "linear_team_ids": []string{teamID, otherTeamID}, "linear_default_team_id": teamID, "linear_project_ids": []string{projectA, projectB}}},
	} {
		if _, e = store.Execute(context.Background(), r); e != nil {
			t.Fatal(r.Operation, e)
		}
	}
	s, _, p, _ := testV2(t)
	env := &seedEnv{s: s, scope: bus.Request{Workspace: "w", Project: "p", Args: map[string]any{}}, now: time.Now(), run: store.Execute}
	env.f = &docFake{providerV2: p, docs: map[string]*linearprovider.Document{}, parents: map[string]string{}}
	s.cfg.Executor = &storeAuthority{run: store.Execute}
	s.cfg.Authority = "local-seed"
	s.cfg.Now = func() time.Time { return env.now }
	s.cfg.Open = func(context.Context, *http.Client, bool) (Provider, error) { env.opens++; return env.f, nil }
	return env
}

func (e *seedEnv) seed(t *testing.T, in SeedInput) SeedResult {
	t.Helper()
	in.Version = "v0.3.1"
	in.ServiceURL = "https://clawbridge.example.test"
	r, err := e.s.SeedDocs(context.Background(), e.scope, in)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func actions(r SeedResult) string {
	var out []string
	for _, d := range r.Documents {
		s := d.Template + "=" + d.Action
		if d.Outcome != "" {
			s += "/" + d.Outcome
		}
		out = append(out, s)
	}
	return strings.Join(out, " ")
}

func (e *seedEnv) rows(t *testing.T) []map[string]any {
	t.Helper()
	r, err := e.run(context.Background(), bus.Request{Operation: "doc_seed_get", Workspace: "w", Project: "p", Args: map[string]any{"parent_kind": "team", "parent_id": teamID}})
	if err != nil {
		t.Fatal(err)
	}
	return r.Data.([]map[string]any)
}

// The preview of an unseeded board reads nothing from Linear; a confirmed
// run creates both guides under the default team, records the digest of
// the content Linear returned (DS-M1), and a re-run finds them untouched.
func TestSeedDocsCreatesOnceAndRecognisesUntouchedDocuments(t *testing.T) {
	e := seedTest(t)
	if r := e.seed(t, SeedInput{}); actions(r) != "developer=create reviewer=create" || r.ParentKind != "team" || r.ParentID != teamID || e.opens != 0 {
		t.Fatal(actions(r), r, e.opens)
	}
	r := e.seed(t, SeedInput{Confirm: true})
	if actions(r) != "developer=create/created reviewer=create/created" || len(e.f.order) != 2 {
		t.Fatal(actions(r), e.f.calls)
	}
	if e.f.calls[0] != "create team "+teamID+" "+docseed.Templates[0].Title {
		t.Fatal(e.f.calls)
	}
	rows := e.rows(t)
	if len(rows) != 2 || rows[0]["state"] != "seeded" || rows[0]["seeded_sha256"] != contentDigest(e.f.docs[rows[0]["document_id"].(string)].Content) {
		t.Fatal(rows)
	}
	if !strings.Contains(e.f.docs[e.f.order[0]].Content, "seed: "+rows[0]["seed_token"].(string)) {
		t.Fatal("the token is not in the document")
	}
	calls := len(e.f.calls)
	if r = e.seed(t, SeedInput{Confirm: true}); actions(r) != "developer=none reviewer=none" || len(e.f.order) != 2 {
		t.Fatal("a re-run did something:", actions(r), e.f.calls[calls:])
	}
}

// An edited document is never written; an archived or unreadable one is
// reported, and created again only with --reseed.
func TestSeedDocsLeavesEditedAndReportsGone(t *testing.T) {
	e := seedTest(t)
	e.seed(t, SeedInput{Confirm: true})
	dev, rev := e.f.order[0], e.f.order[1]
	e.f.docs[dev].Content += "\nOur team's own practice.\n"
	e.f.docs[rev].Archived = true
	if r := e.seed(t, SeedInput{Confirm: true, Update: true}); actions(r) != "developer=edited reviewer=gone" || len(e.f.order) != 2 {
		t.Fatal(actions(r))
	}
	r := e.seed(t, SeedInput{Confirm: true, Reseed: true})
	if actions(r) != "developer=edited reviewer=create/created" || len(e.f.order) != 3 || r.Documents[1].DocumentID != e.f.order[2] {
		t.Fatal(actions(r), e.f.order)
	}
	for _, c := range e.f.calls {
		if strings.HasPrefix(c, "update ") {
			t.Fatal("a document was overwritten:", c)
		}
	}
	// DS-L4: a failed read is never taken as gone.
	e.f.getErr = true
	if r = e.seed(t, SeedInput{Confirm: true}); actions(r) != "developer=unreadable reviewer=unreadable" || len(e.f.order) != 3 {
		t.Fatal(actions(r))
	}
}

// A newer template is offered for an untouched document, and applied only
// with --update.
func TestSeedDocsUpdatesOnlyUntouchedDocuments(t *testing.T) {
	e := seedTest(t)
	e.seed(t, SeedInput{Confirm: true})
	saved := docseed.Templates[1].Version
	docseed.Templates[1].Version = saved + 1
	defer func() { docseed.Templates[1].Version = saved }()
	if r := e.seed(t, SeedInput{Confirm: true}); actions(r) != "developer=none reviewer=update_available" {
		t.Fatal(actions(r))
	}
	if r := e.seed(t, SeedInput{Confirm: true, Update: true}); actions(r) != "developer=none reviewer=update/updated" {
		t.Fatal(actions(r))
	}
	rows := e.rows(t)
	if fmt.Sprint(rows[1]["template_version"]) != fmt.Sprint(saved+1) || rows[1]["seeded_sha256"] != contentDigest(e.f.docs[e.f.order[1]].Content) {
		t.Fatal(rows[1])
	}
	if r := e.seed(t, SeedInput{Confirm: true, Update: true}); actions(r) != "developer=none reviewer=none" || len(e.f.order) != 2 {
		t.Fatal(actions(r))
	}
}

// DS-M2: an uncertain create is found again by its token under the
// parent; one that never happened waits, then is created again, never
// twice blindly.
func TestSeedDocsSettlesAnUncertainCreate(t *testing.T) {
	e := seedTest(t)
	e.f.createLost = true
	if r := e.seed(t, SeedInput{Confirm: true}); actions(r) != "developer=create/uncertain reviewer=create/uncertain" || len(e.f.order) != 2 {
		t.Fatal(actions(r))
	}
	e.f.createLost = false
	if r := e.seed(t, SeedInput{Confirm: true}); actions(r) != "developer=settle/settled reviewer=settle/settled" || len(e.f.order) != 2 {
		t.Fatal(actions(r))
	}
	if rows := e.rows(t); rows[0]["state"] != "seeded" || rows[1]["state"] != "seeded" {
		t.Fatal(rows)
	}
	e2 := seedTest(t)
	e2.f.createErr = true
	e2.seed(t, SeedInput{Confirm: true})
	e2.f.createErr = false
	if r := e2.seed(t, SeedInput{Confirm: true}); actions(r) != "developer=pending_recent reviewer=pending_recent" || len(e2.f.order) != 0 {
		t.Fatal("a recent uncertain create was redone:", actions(r))
	}
	old := e2.rows(t)[0]["seed_token"]
	e2.now = e2.now.Add(seedPendingFor + time.Minute)
	if r := e2.seed(t, SeedInput{Confirm: true}); actions(r) != "developer=create/created reviewer=create/created" || len(e2.f.order) != 2 {
		t.Fatal(actions(r))
	}
	if rows := e2.rows(t); rows[0]["seed_token"] == old || rows[0]["state"] != "seeded" {
		t.Fatal("the old reservation was reused", rows)
	}
}

// Seeding is a lead's or owner's, on a linked board.
func TestSeedDocsRefusals(t *testing.T) {
	s, a, _, scope := adminTest(t)
	a.role = "member"
	if _, err := s.SeedDocs(context.Background(), scope, SeedInput{Version: "v1"}); err == nil || !strings.Contains(err.Error(), "lead or owner") {
		t.Fatal(err)
	}
	a.authorityV2.b.Configured = false
	if _, err := s.SeedDocs(context.Background(), scope, SeedInput{Version: "v1"}); err == nil {
		t.Fatal("an unlinked board seeded")
	}
	// A confirmed run needs the ClawBridge version for the footer.
	e := seedTest(t)
	if r, err := e.s.SeedDocs(context.Background(), e.scope, SeedInput{Confirm: true}); err != nil || actions(r) != "developer=create/failed reviewer=create/failed" || len(e.f.order) != 0 {
		t.Fatal(actions(r), err)
	}
}
