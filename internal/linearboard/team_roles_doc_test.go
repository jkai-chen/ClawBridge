package linearboard

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"clawbridge/internal/bus"
	"clawbridge/internal/linearprovider"
)

const (
	rolesAda = "aaaaaaaa-0000-4000-8000-00000000000a"
	rolesBen = "bbbbbbbb-0000-4000-8000-00000000000b"
	rolesCy  = "cccccccc-0000-4000-8000-00000000000c"
)

// rolesFake is the publish fake with the workspace's users: Ada and Ben;
// Cy left the workspace.
type rolesFake struct {
	*pubFake
	users []linearprovider.WorkspaceUser
}

func (f *rolesFake) WorkspaceUsers(context.Context, linearprovider.PageOptions) (linearprovider.Page[linearprovider.WorkspaceUser], error) {
	return linearprovider.Page[linearprovider.WorkspaceUser]{Items: f.users, Complete: true}, nil
}

type rolesEnv struct {
	*pubEnv
	rf *rolesFake
}

// rolesDocTest is the publish env with team roles set at revision 1: Ada
// leads teamID, Ben engineers in it, Cy reviews in it and in the other
// team. Ada's principal is linked, which must never reach the document.
func rolesDocTest(t *testing.T) *rolesEnv {
	t.Helper()
	env := &rolesEnv{pubEnv: pubTest(t)}
	env.rf = &rolesFake{pubFake: env.pf, users: []linearprovider.WorkspaceUser{
		{ID: rolesAda, Name: "Ada Example", Email: "ada@example.test", Active: true, ActiveKnown: true, GuestKnown: true},
		{ID: rolesBen, Name: "Ben *Bold*", Email: "ben@example.test", Active: true, ActiveKnown: true, GuestKnown: true},
	}}
	env.s.cfg.Open = func(context.Context, *http.Client, bool) (Provider, error) { env.opens++; return env.rf, nil }
	env.setRoles(t, 0, map[string]any{
		"teams": []any{map[string]any{"team_id": teamID, "leads": []any{rolesAda}}},
		"people": []any{
			map[string]any{"linear_user_id": rolesAda, "role": "lead", "teams": []any{teamID}},
			map[string]any{"linear_user_id": rolesBen, "role": "engineer", "teams": []any{teamID}},
			map[string]any{"linear_user_id": rolesCy, "role": "reviewer", "teams": []any{teamID, otherTeamID}},
		},
	})
	return env
}

func (e *rolesEnv) setRoles(t *testing.T, rev int, mapping map[string]any) {
	t.Helper()
	if _, err := e.run(context.Background(), bus.Request{Operation: "team_roles_set", Workspace: "w", Project: "p", Args: map[string]any{"expected_revision": rev, "confirm": true, "mapping": mapping}}); err != nil {
		t.Fatal(err)
	}
}

func (e *rolesEnv) publishRoles(t *testing.T, in RolesPublishInput) RolesPublishResult {
	t.Helper()
	r, err := e.s.PublishTeamRoles(context.Background(), e.scope, in)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func action(r RolesPublishResult) string {
	if r.Outcome != "" {
		return r.Action + "/" + r.Outcome
	}
	return r.Action
}

// A preview writes nothing; a publish creates the document with display
// names only, reads it back and records it; again does nothing; a roles
// change updates it; an edit in Linear is overwritten (§4's exception).
func TestPublishTeamRolesCreateNoneUpdateOverwrite(t *testing.T) {
	e := rolesDocTest(t)
	if _, err := e.s.PublishTeamRoles(context.Background(), e.scope, RolesPublishInput{}); err == nil || !strings.Contains(err.Error(), "--team or --linear-project") {
		t.Fatal("no parent:", err)
	}
	in := RolesPublishInput{ParentKind: "team", ParentID: teamID}
	if r := e.publishRoles(t, in); action(r) != "create" || r.Confirmed || e.creates() != 0 {
		t.Fatal("preview", action(r), r)
	}
	in.Confirm = true
	r := e.publishRoles(t, in)
	if action(r) != "create/created" || r.Revision != 1 {
		t.Fatal(action(r), r)
	}
	doc := e.f.docs[r.DocumentID]
	for _, want := range []string{"Ada Example", "Ben Bold", "(a former user)", "**Leads:** Ada Example", "**Engineers:** Ben Bold", "revision 1", "overwritten at the next publish"} {
		if !strings.Contains(doc.Content, want) {
			t.Fatal(want, doc.Content)
		}
	}
	for _, never := range []string{"@example.test", rolesAda, rolesBen, "local:"} {
		if strings.Contains(doc.Content, never) {
			t.Fatal("the document shows", never, doc.Content)
		}
	}
	if doc.Title != TeamRolesDocTitle {
		t.Fatal(doc.Title)
	}
	// The parent is remembered.
	if r = e.publishRoles(t, RolesPublishInput{Confirm: true}); action(r) != "none" || e.creates() != 1 {
		t.Fatal("unchanged", action(r))
	}
	e.setRoles(t, 1, map[string]any{
		"teams":  []any{map[string]any{"team_id": teamID, "leads": []any{rolesAda}}},
		"people": []any{map[string]any{"linear_user_id": rolesAda, "role": "lead", "teams": []any{teamID}}},
	})
	if r = e.publishRoles(t, RolesPublishInput{Confirm: true}); action(r) != "update/updated" || strings.Contains(doc.Content, "Ben Bold") || !strings.Contains(doc.Content, "revision 2") {
		t.Fatal("update", action(r), doc.Content)
	}
	doc.Content += "\na person's note\n"
	r = e.publishRoles(t, RolesPublishInput{})
	if action(r) != "update" || !strings.Contains(r.Note, "overwrites the edit") {
		t.Fatal("edited preview", action(r), r.Note)
	}
	if r = e.publishRoles(t, RolesPublishInput{Confirm: true}); action(r) != "update/updated" || strings.Contains(doc.Content, "a person's note") {
		t.Fatal("the edit stayed", action(r))
	}
	if r = e.publishRoles(t, RolesPublishInput{Confirm: true}); action(r) != "none" {
		t.Fatal("the overwrite was not recorded", action(r))
	}
}

// Moving the document needs --recreate; the old one is left and named for
// archiving by hand (review V2). An archived document needs --recreate.
func TestPublishTeamRolesMoveAndGone(t *testing.T) {
	e := rolesDocTest(t)
	first := e.publishRoles(t, RolesPublishInput{ParentKind: "team", ParentID: teamID, Confirm: true}).DocumentID
	if _, err := e.s.PublishTeamRoles(context.Background(), e.scope, RolesPublishInput{ParentKind: "team", ParentID: otherTeamID, Confirm: true}); err == nil || !strings.Contains(err.Error(), "--recreate") {
		t.Fatal("moved without --recreate:", err)
	}
	r := e.publishRoles(t, RolesPublishInput{ParentKind: "team", ParentID: otherTeamID, Recreate: true, Confirm: true})
	if action(r) != "create/created" || r.Archive != first || r.DocumentID == first {
		t.Fatal(action(r), r)
	}
	state, err := e.s.TeamRolesDoc(context.Background(), e.scope)
	if err != nil || state.Published.Replaced != first || state.Published.ParentID != otherTeamID {
		t.Fatal(state.Published, err)
	}
	e.f.docs[r.DocumentID].Archived = true
	if r = e.publishRoles(t, RolesPublishInput{Confirm: true}); action(r) != "gone" || r.Refused == "" {
		t.Fatal("archived", action(r))
	}
}

// A deny pattern in a name refuses the publish with no override; a
// mention in a name is refused before anything is sent.
func TestPublishTeamRolesDenyAndMention(t *testing.T) {
	e := rolesDocTest(t)
	e.rf.users[1].Name = "Ben internal-name-7"
	r := e.publishRoles(t, RolesPublishInput{ParentKind: "team", ParentID: teamID, Confirm: true})
	if r.Refused == "" || len(r.Hits) != 1 || r.Hits[0] != "internal-name" || e.creates() != 0 {
		t.Fatal(r)
	}
	e.rf.users[1].Name = "@channel"
	if _, err := e.s.PublishTeamRoles(context.Background(), e.scope, RolesPublishInput{ParentKind: "team", ParentID: teamID, Confirm: true}); err == nil || e.creates() != 0 {
		t.Fatal("a mention was sent:", err)
	}
}

// A create whose answer was lost is settled by its token, never by title:
// a document with the same title and no token is never adopted (T4).
func TestPublishTeamRolesSettlesByTokenOnly(t *testing.T) {
	e := rolesDocTest(t)
	decoy := func() {
		e.f.docs["d0c00000-0000-4000-8000-0000000000ff"] = &linearprovider.Document{ID: "d0c00000-0000-4000-8000-0000000000ff", Title: TeamRolesDocTitle, Content: "someone's own page"}
		e.f.parents["d0c00000-0000-4000-8000-0000000000ff"] = "team:" + teamID
		e.f.order = append(e.f.order, "d0c00000-0000-4000-8000-0000000000ff")
	}
	e.f.createLost = true
	r := e.publishRoles(t, RolesPublishInput{ParentKind: "team", ParentID: teamID, Confirm: true})
	if action(r) != "create/uncertain" {
		t.Fatal(action(r))
	}
	e.f.createLost = false
	// Listed after the lost create, so a match by title would take it.
	decoy()
	r = e.publishRoles(t, RolesPublishInput{Confirm: true})
	if action(r) != "settle/settled" || r.DocumentID == "d0c00000-0000-4000-8000-0000000000ff" || e.f.docs["d0c00000-0000-4000-8000-0000000000ff"].Content != "someone's own page" {
		t.Fatal(action(r), r)
	}
}

// Doctor's drift: nothing published, then in step, edited, behind the
// mapping, archived; a recreate leaves the old document named.
func TestTeamRolesDocDrift(t *testing.T) {
	e := rolesDocTest(t)
	drift := func() RolesDocDrift {
		t.Helper()
		d, err := e.s.TeamRolesDocDrift(context.Background(), e.scope)
		if err != nil {
			t.Fatal(err)
		}
		return d
	}
	if d := drift(); d.Published || d.Revision != 1 {
		t.Fatal(d)
	}
	id := e.publishRoles(t, RolesPublishInput{ParentKind: "team", ParentID: teamID, Confirm: true}).DocumentID
	if d := drift(); !d.Published || d.Edited || d.PublishedRevision != 1 || d.Revision != 1 {
		t.Fatal(d)
	}
	e.f.docs[id].Content += "\nnote\n"
	e.setRoles(t, 1, map[string]any{"teams": []any{}, "people": []any{}})
	if d := drift(); !d.Edited || d.PublishedRevision != 1 || d.Revision != 2 {
		t.Fatal(d)
	}
	e.f.docs[id].Archived = true
	if d := drift(); !d.Archived {
		t.Fatal(d)
	}
	e.publishRoles(t, RolesPublishInput{Recreate: true, Confirm: true})
	if d := drift(); d.Replaced != id || d.Edited || d.Archived {
		t.Fatal(d)
	}
}

// A display or team name is plain text in the document (review L1): no
// link, heading, emphasis or HTML can come from a name.
func TestRenderTeamRolesNamesArePlain(t *testing.T) {
	m := teamRolesMapping{Revision: 1, People: []bus.TeamRolePerson{{LinearUserID: rolesAda, Role: "engineer", Teams: []string{teamID}}}}
	body := renderTeamRoles(m, []string{teamID}, map[string]string{teamID: "# Team\n<b>x</b>"}, []linearprovider.WorkspaceUser{{ID: rolesAda, Name: "[click](https://evil.example) **bold** `code`\n## heading"}})
	for _, bad := range []string{"](", "**bold", "`code", "\n## heading", "<b>", "## # Team", "://"} {
		if strings.Contains(body, bad) {
			t.Fatal(bad, body)
		}
	}
	if !strings.Contains(body, "click(https:/ /evil.example) bold code heading") || !strings.Contains(body, "## Team bx/b") {
		t.Fatal(body)
	}
}

// Two bound teams with the same name render in one order every time (by
// ID after the name), so an unchanged mapping publishes as "none" rather
// than an update caused by map order.
func TestRenderTeamRolesSameNameIsStable(t *testing.T) {
	m := teamRolesMapping{Revision: 1, People: []bus.TeamRolePerson{
		{LinearUserID: rolesAda, Role: "engineer", Teams: []string{otherTeamID}},
		{LinearUserID: rolesBen, Role: "reviewer", Teams: []string{teamID}},
	}}
	names := map[string]string{teamID: "Team", otherTeamID: "Team"}
	users := []linearprovider.WorkspaceUser{{ID: rolesAda, Name: "Ada"}, {ID: rolesBen, Name: "Ben"}}
	first := renderTeamRoles(m, []string{teamID, otherTeamID}, names, users)
	for range 200 {
		if got := renderTeamRoles(m, []string{otherTeamID, teamID}, names, users); got != first {
			t.Fatal("the same roles rendered in another order:\n", first, "\n---\n", got)
		}
	}
	// teamID sorts before otherTeamID, so its reviewer comes first.
	if strings.Index(first, "Ben") > strings.Index(first, "Ada") {
		t.Fatal("same-named teams aren't ordered by ID:", first)
	}
}
