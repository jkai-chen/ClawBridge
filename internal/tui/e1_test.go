package tui

import (
	"strings"
	"testing"

	"clawbridge/internal/bus"
)

func (s *testLinearSource) ops() string {
	out := []string{}
	for _, c := range s.calls {
		out = append(out, c.Operation)
	}
	return strings.Join(out, ",")
}

func linkedLinear(t *testing.T) (Model, *testLinearSource) {
	t.Helper()
	source := &testLinearSource{binding: bus.TaskboardBinding{Configured: true, Revision: 4, LinearTeamIDs: []string{linearTestTeam}, LinearDefaultTeamID: linearTestTeam}}
	cfg := testConfig(&fakeAuthority{})
	cfg.Linear = source
	m, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	m = m.SetSize(120, 40)
	m, cmd := m.Show(ScreenLinear)
	return quickSettle(m, cmd), source
}

// U34: unlinking the shared board takes the project's slug typed; the
// wrong one sends nothing.
func TestLinearUnlinkTakesTheSlug(t *testing.T) {
	m, source := linkedLinear(t)
	m.linear.step = "unlink"
	m = do(m, "enter")
	if m.inputMode != inputLinearUnlink || !strings.Contains(Render(m), "affects everyone in this project") {
		t.Fatal("no slug asked:", Render(m))
	}
	before := source.ops()
	m.draft = "wrong"
	m = do(m, "enter")
	if source.ops() != before || !strings.Contains(m.status.text, "nothing was unlinked") {
		t.Fatal("unlinked on the wrong slug:", source.ops(), m.status.text)
	}
	m.linear.step = "unlink"
	m = do(m, "enter")
	m.draft = m.linearUnlinkSlug()
	m = do(m, "enter")
	if !strings.HasSuffix(source.ops(), "unlink") {
		t.Fatal("the right slug didn't unlink:", source.ops())
	}
	// With no slug known, nothing typed matches, not even nothing.
	m, source = linkedLinear(t)
	m.project.Slug, m.cfg.Project = "", ""
	m.linear.step = "unlink"
	m = do(m, "enter")
	m.draft = ""
	if m = do(m, "enter"); strings.Contains(source.ops(), "unlink") {
		t.Fatal("unlinked with no slug to type:", source.ops())
	}
}

// U31: each step of linking says which of five it is.
func TestLinearStepTitles(t *testing.T) {
	m, _ := linkedLinear(t)
	for step, want := range map[string]string{"teams": "step 2 of 5: choose the teams", "default": "step 3 of 5", "projects": "step 4 of 5", "preview": "step 5 of 5: review"} {
		m.linear.step = step
		if got := linearStepTitle(m); !strings.HasPrefix(got, "Link a board · ") || !strings.Contains(got, want) {
			t.Fatal(step, got)
		}
	}
}

// U34: x asks before logging a connection out; only y does.
func TestLinearLogoutAsks(t *testing.T) {
	m, source := linkedLinear(t)
	m.linear.step = "profiles"
	m = do(m, "x")
	if m.linear.logoutAsk == "" || !strings.Contains(Render(m), "y logs out") || strings.Contains(source.ops(), "logout") {
		t.Fatal("x logged out at once:", source.ops())
	}
	m = do(m, "n")
	if m.linear.logoutAsk != "" || strings.Contains(source.ops(), "logout") {
		t.Fatal("n logged out")
	}
	m = do(m, "x", "y")
	if !strings.Contains(source.ops(), "logout") {
		t.Fatal("y didn't log out:", source.ops())
	}
}

// U31: the board in words, the revision and IDs behind d.
func TestLinearBoardInWords(t *testing.T) {
	m, _ := linkedLinear(t)
	view := Render(m)
	if !strings.Contains(view, "Board: linked · 1 team · the team board") || strings.Contains(view, linearTestTeam) || strings.Contains(view, "revision") || strings.Contains(view, "configured=") {
		t.Fatalf("%s", view)
	}
	if view = Render(do(m, "d")); !strings.Contains(view, "Revision 4") || !strings.Contains(view, linearTestTeam) {
		t.Fatalf("d didn't show the IDs:\n%s", view)
	}
}

// U54: the invite is an email, then a role per project; Esc goes back to
// the email.
func TestAccountInviteChecklist(t *testing.T) {
	a := &accountAuthority{principal: "owner@example.com", owner: "owner@example.com"}
	m := accountModel(t, a)
	m = press(t, m, "i")
	m = typed(t, m, "bob@example.com")
	m = press(t, m, "enter")
	if m.account.invite == nil || !strings.Contains(Render(m), "Invite bob@example.com") || !strings.Contains(Render(m), "[—]") {
		t.Fatal(Render(m))
	}
	m = press(t, m, "esc")
	if m.account.invite != nil || m.inputMode != inputAccountInvite || m.draft != "bob@example.com" {
		t.Fatal("esc didn't go back to the email:", m.draft)
	}
	m = press(t, m, "enter", " ", " ")
	if !strings.Contains(Render(m), "[member]") || len(a.writes()) != 0 {
		t.Fatal(Render(m), a.writes())
	}
	// The second project: round all five roles back to none, then reader.
	m = press(t, m, "down", " ", " ", " ", " ", " ")
	if _, kept := m.account.invite.roles["web"]; kept {
		t.Fatal("none kept as a grant:", m.account.invite.roles)
	}
	m = press(t, m, " ")
	m = press(t, m, "enter")
	w := a.writes()
	grants, _ := w[0].Args["grants"].(map[string]any)
	if len(w) != 1 || w[0].Operation != "invite_create" || w[0].Args["email"] != "bob@example.com" || grants["api"] != "member" || grants["web"] != "reader" || len(grants) != 2 {
		t.Fatal(w)
	}
}

// U54: the first step takes an email; anything else is said so.
func TestAccountInviteNeedsAnEmail(t *testing.T) {
	m := accountModel(t, &accountAuthority{principal: "owner@example.com", owner: "owner@example.com"})
	m = press(t, m, "i")
	m = typed(t, m, "bob")
	m = press(t, m, "enter")
	if m.account.invite != nil || m.status.text != "type the invitee's email" {
		t.Fatal(m.status.text)
	}
}

// R183 L1: an owner or lead grant is typed "invite" first; L2: nothing
// ticked asks once more.
func TestAccountInviteAsksForPowerAndForNothing(t *testing.T) {
	a := &accountAuthority{principal: "owner@example.com", owner: "owner@example.com"}
	m := accountModel(t, a)
	m = press(t, m, "i")
	m = typed(t, m, "bob@example.com")
	m = press(t, m, "enter", " ", " ", " ", " ", "enter")
	if !m.confirming || m.account.pending == nil || !strings.Contains(m.account.pending.prompt, "as owner of api") || len(a.writes()) != 0 {
		t.Fatal("an owner grant wasn't typed:", Render(m), a.writes())
	}
	m = typed(t, m, "invite")
	m = press(t, m, "enter")
	w := a.writes()
	if len(w) != 1 || w[0].Args["grants"].(map[string]any)["api"] != "owner" {
		t.Fatal(w)
	}
	// Nothing ticked: Enter asks, Enter again sends.
	m = press(t, m, "esc")
	m = press(t, m, "i")
	m = typed(t, m, "carol@example.com")
	m = press(t, m, "enter", "enter")
	if !strings.Contains(m.status.text, "No project is ticked") || len(a.writes()) != 1 || m.account.invite == nil {
		t.Fatal("sent with nothing ticked:", m.status.text, a.writes())
	}
	m = press(t, m, "enter")
	if w = a.writes(); len(w) != 2 || len(w[1].Args["grants"].(map[string]any)) != 0 {
		t.Fatal(w)
	}
	// A lead grant is typed too.
	m = press(t, m, "esc")
	m = press(t, m, "i")
	m = typed(t, m, "dave@example.com")
	m = press(t, m, "enter", "down", " ", " ", " ", "enter")
	if !m.confirming || m.account.pending == nil || !strings.Contains(m.account.pending.prompt, "as lead of web") {
		t.Fatal("a lead grant wasn't typed:", m.account.pending)
	}
}
