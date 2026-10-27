package tui

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"

	"clawbridge/internal/bus"
	tea "github.com/charmbracelet/bubbletea"
)

const testCode = "CBJ-4K7P-WQ9D-2MXH-T6RB"

// accountAuthority answers the account operations as the server would for
// a workspace whose owner is owner.
type accountAuthority struct {
	mu        sync.Mutex
	principal string
	owner     string
	transfer  map[string]any
	newDevice bool
	// dormant gives cy a dormant device too (§17.4).
	dormant bool
	events  []any
	calls   []bus.Request
}

func (a *accountAuthority) Execute(_ context.Context, r bus.Request) (bus.Result, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.calls = append(a.calls, r)
	var data map[string]any
	switch r.Operation {
	case "whoami":
		data = map[string]any{"principal": a.principal, "workspace": map[string]any{"name": "pilot", "owner_principal": a.owner}, "online": true}
	case "device_list":
		data = map[string]any{"devices": []any{
			map[string]any{"device_id": "dev_this", "principal": a.principal, "kind": "device", "label": "laptop", "status": "active", "key_store": "keyring"},
			map[string]any{"device_id": "dev_old", "principal": a.principal, "kind": "device", "label": "old\x1b[31m", "status": "revoked"},
		}}
		if a.newDevice {
			// A device added elsewhere sorts first.
			data["devices"] = append([]any{map[string]any{"device_id": "dev_new", "principal": a.principal, "kind": "device", "label": "new", "status": "active"}}, data["devices"].([]any)...)
		}
	case "owner_transfer_get":
		data = map[string]any{"pending": a.transfer}
	case "device_events":
		data = map[string]any{"events": a.events}
	case "device_ack":
		a.events = nil
		data = map[string]any{"acknowledged": len(a.events)}
		a.events = nil
	case "invite_list":
		data = map[string]any{"invites": []any{
			map[string]any{"invite_id": "inv_1", "email": "ada@example.com", "purpose": "user", "status": "pending", "grants": map[string]any{"prj_1": "member"}, "expires_at": "2026-10-29"},
			map[string]any{"invite_id": "inv_2", "email": "bob@example.com", "purpose": "user", "status": "burnt", "revoked_at": "2026-10-26"},
		}}
	case "user_list":
		if a.principal != a.owner {
			return bus.Result{}, fmt.Errorf("only the workspace owner may list users")
		}
		data = map[string]any{"users": []any{
			map[string]any{"principal": a.owner, "owner": true, "devices": []any{}},
			map[string]any{"principal": "cy@example.com", "invited_by": a.owner, "roles": []any{map[string]any{"slug": "api", "role": "member"}},
				"devices": []any{map[string]any{"device_id": "dev_cy", "principal": "cy@example.com", "kind": "device", "label": "cy's", "status": "active"}}},
		}}
		if a.dormant {
			cy := data["users"].([]any)[1].(map[string]any)
			cy["devices"] = append(cy["devices"].([]any), map[string]any{"device_id": "dev_cyold", "principal": "cy@example.com", "kind": "device", "label": "cy's old", "status": "dormant", "dormant_at": "2026-10-01"})
		}
	case "invite_create":
		data = map[string]any{"code": testCode, "invite_id": "inv_new", "email": r.Args["email"], "purpose": r.Args["purpose"], "expires_at": "2026-10-28"}
	case "device_revoke":
		data = map[string]any{"device_id": r.Args["device_id"], "status": "revoked"}
	case "device_label":
		data = map[string]any{"device_id": r.Args["device_id"], "label": r.Args["label"]}
	case "device_confirm":
		data = map[string]any{"device_id": r.Args["device_id"], "status": "active", "was_dormant": true}
	case "invite_revoke":
		data = map[string]any{"invite_id": r.Args["invite_id"], "status": "revoked"}
	case "user_revoke":
		data = map[string]any{"principal": r.Args["principal"], "revoked_devices": []any{"dev_cy"}}
	case "owner_transfer_offer":
		data = map[string]any{"from": a.principal, "to": r.Args["to"]}
	case "owner_transfer_respond":
		data = map[string]any{"status": r.Args["action"].(string) + "ed"}
	default:
		return bus.Result{}, fmt.Errorf("unexpected operation %s", r.Operation)
	}
	return bus.Result{Source: "server", Data: data}, nil
}

// writes is every operation that isn't a read of the screen.
func (a *accountAuthority) writes() []bus.Request {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := []bus.Request{}
	for _, r := range a.calls {
		switch r.Operation {
		case "whoami", "device_list", "owner_transfer_get", "invite_list", "user_list", "device_events":
		default:
			out = append(out, r)
		}
	}
	return out
}

func (a *accountAuthority) did(op string) bool {
	for _, r := range a.writes() {
		if r.Operation == op {
			return true
		}
	}
	return false
}

// settle runs a command and every command its message leads to.
func settle(t *testing.T, m Model, cmd tea.Cmd) Model {
	t.Helper()
	for i := 0; cmd != nil && i < 10; i++ {
		m, cmd = m.UpdateModel(cmd())
	}
	return m
}

func press(t *testing.T, m Model, keys ...string) Model {
	t.Helper()
	for _, k := range keys {
		var msg tea.KeyMsg
		switch k {
		case "enter":
			msg = tea.KeyMsg{Type: tea.KeyEnter}
		case "esc":
			msg = tea.KeyMsg{Type: tea.KeyEsc}
		case "tab":
			msg = tea.KeyMsg{Type: tea.KeyTab}
		case "down":
			msg = tea.KeyMsg{Type: tea.KeyDown}
		default:
			msg = tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(k)}
		}
		var cmd tea.Cmd
		m, cmd = m.UpdateModel(msg)
		m = settle(t, m, cmd)
	}
	return m
}

func typed(t *testing.T, m Model, text string) Model {
	t.Helper()
	for _, r := range text {
		m = press(t, m, string(r))
	}
	return m
}

func accountModel(t *testing.T, a *accountAuthority) Model {
	t.Helper()
	cfg := testConfig(a)
	cfg.DeviceID = "dev_this"
	m, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	m = m.SetSize(140, 60)
	m, cmd := m.Show(ScreenAccount)
	return settle(t, m, cmd)
}

// selectRow moves the selection to the row with this kind and ID.
func selectRow(t *testing.T, m Model, row string) Model {
	t.Helper()
	for i, id := range m.AccountState().Rows {
		if id == row {
			return m.Select(i)
		}
	}
	t.Fatalf("no row %s in %v", row, m.AccountState().Rows)
	return m
}

// The workspace owner sees their devices, with this one marked, every
// other member with their devices, and the invites, burnt ones marked.
func TestAccountShowsDevicesMembersAndInvites(t *testing.T) {
	a := &accountAuthority{principal: "owner@example.com", owner: "owner@example.com"}
	m := accountModel(t, a)
	s := m.AccountState()
	if !s.MainOwner || strings.Join(s.Rows, " ") != "device:dev_this device:dev_old user:cy@example.com device:dev_cy invite:inv_1 invite:inv_2" {
		t.Fatalf("%+v", s)
	}
	view := Render(m)
	for _, want := range []string{"7 Account", "owner@example.com (workspace owner)", "dev_this (this device)", "cy@example.com · api=member", "burnt after failed attempts", "t offer ownership", `old\x1b[31m`} {
		if !strings.Contains(view, want) {
			t.Fatalf("view missing %q:\n%s", want, view)
		}
	}
	for _, r := range a.calls {
		if r.Workspace != "ws_1" {
			t.Fatal("scope drifted:", r)
		}
	}
	// A member sees no member list, and never asks for it.
	a = &accountAuthority{principal: "cy@example.com", owner: "owner@example.com"}
	m = accountModel(t, a)
	if m.AccountState().MainOwner || len(m.AccountState().Rows) != 4 || strings.Contains(Render(m), "Members") {
		t.Fatalf("%+v", m.AccountState())
	}
	for _, r := range a.calls {
		if r.Operation == "user_list" {
			t.Fatal("a member's screen asked for the member list")
		}
	}
}

// Adding a device issues a device invite for one's own email, and its code
// shows once, in a modal that holds the screen until Enter or Esc; then it
// is gone from the model and never drawn again.
func TestAccountCodeShowsOnce(t *testing.T) {
	a := &accountAuthority{principal: "owner@example.com", owner: "owner@example.com"}
	m := accountModel(t, a)
	m = press(t, m, "a")
	if len(a.writes()) != 0 || m.AccountState().Pending != "invite_create" {
		t.Fatal("a code was minted on one key")
	}
	m = typed(t, m, "add")
	m = press(t, m, "enter")
	w := a.writes()
	if len(w) != 1 || w[0].Operation != "invite_create" || w[0].Args["purpose"] != "device" || w[0].Args["email"] != "owner@example.com" {
		t.Fatal(w)
	}
	view := Render(m)
	if !strings.Contains(view, testCode) || !strings.Contains(view, "shown once") || !strings.Contains(view, "join --email owner@example.com") {
		t.Fatal(view)
	}
	if fmt.Sprintf("%+v", m.AccountState()) == "" || strings.Contains(fmt.Sprintf("%+v %+v", m.AccountState(), m.State()), testCode) {
		t.Fatal("a snapshot carries the code")
	}
	// Navigation, reload and actions wait for the modal.
	m = press(t, m, "1", "tab", "r", "x")
	if m.CurrentScreen() != ScreenAccount || !strings.Contains(Render(m), testCode) || len(a.writes()) != 1 {
		t.Fatal("the modal let a key through")
	}
	m = press(t, m, "enter")
	if strings.Contains(Render(m), testCode) || m.account.code != "" || m.AccountState().CodeShown {
		t.Fatal("the code outlived its modal")
	}
	m = press(t, m, "r")
	if strings.Contains(Render(m), testCode) {
		t.Fatal("a reload redrew the code")
	}
}

// Revoking a device waits for "revoke" to be typed; anything else, or
// Esc, changes nothing. Revoking this device says so.
func TestAccountRevokeDeviceNeedsTypedConfirmation(t *testing.T) {
	a := &accountAuthority{principal: "owner@example.com", owner: "owner@example.com"}
	m := accountModel(t, a)
	m = press(t, m, "x")
	if m.AccountState().Pending != "device_revoke" || !strings.Contains(Render(m), "THIS device") {
		t.Fatal(Render(m))
	}
	m = typed(t, m, "yes")
	m = press(t, m, "enter")
	if len(a.writes()) != 0 || !strings.Contains(m.State().Error, "doesn't match") {
		t.Fatal(a.writes(), m.State().Error)
	}
	m = press(t, m, "esc")
	if m.AccountState().Pending != "" || len(a.writes()) != 0 {
		t.Fatal("esc left the action")
	}
	m = selectRow(t, m, "device:dev_cy")
	m = press(t, m, "x")
	m = typed(t, m, "revoke")
	m = press(t, m, "enter")
	w := a.writes()
	if len(w) != 1 || w[0].Operation != "device_revoke" || w[0].Args["device_id"] != "dev_cy" {
		t.Fatal(w)
	}
	// A revoked device and a burnt invite have nothing to revoke.
	for _, row := range []string{"device:dev_old", "invite:inv_2"} {
		m = selectRow(t, m, row)
		m = press(t, m, "x")
		if m.AccountState().Pending != "" {
			t.Fatal(row, "offered a revoke")
		}
	}
	m = selectRow(t, m, "invite:inv_1")
	m = press(t, m, "x")
	m = typed(t, m, "revoke")
	m = press(t, m, "enter")
	if !a.did("invite_revoke") {
		t.Fatal(a.writes())
	}
}

// §17.4: w confirms another device, a member's naming them, after a typed
// word; never this device, nor a revoked one. A dormant device can still be
// revoked.
func TestAccountConfirmsADormantDevice(t *testing.T) {
	a := &accountAuthority{principal: "owner@example.com", owner: "owner@example.com", dormant: true}
	m := accountModel(t, a)
	if !strings.Contains(Render(m), "dormant since 2026-10-01") {
		t.Fatal(Render(m))
	}
	for _, row := range []string{"device:dev_this", "device:dev_old"} {
		m = selectRow(t, m, row)
		m = press(t, m, "w")
		if m.AccountState().Pending != "" {
			t.Fatal(row, "offered a confirm")
		}
	}
	m = selectRow(t, m, "device:dev_cyold")
	m = press(t, m, "x")
	if m.AccountState().Pending != "device_revoke" {
		t.Fatal("a dormant device can't be revoked")
	}
	m = press(t, m, "esc")
	m = press(t, m, "w")
	m = typed(t, m, "confirm")
	m = press(t, m, "enter")
	w := a.writes()
	if len(w) != 1 || w[0].Operation != "device_confirm" || w[0].Args["device_id"] != "dev_cyold" || w[0].Args["principal"] != "cy@example.com" {
		t.Fatal(w)
	}
}

// Removing a member takes their email typed out; only the workspace
// owner is offered it.
func TestAccountRevokeUserTakesTheEmail(t *testing.T) {
	a := &accountAuthority{principal: "owner@example.com", owner: "owner@example.com"}
	m := accountModel(t, a)
	m = selectRow(t, m, "user:cy@example.com")
	m = press(t, m, "x")
	m = typed(t, m, "revoke")
	m = press(t, m, "enter")
	if a.did("user_revoke") {
		t.Fatal("revoked a member on the wrong phrase")
	}
	m = press(t, m, "esc", "x")
	m = typed(t, m, "cy@example.com")
	m = press(t, m, "enter")
	w := a.writes()
	if len(w) != 1 || w[0].Operation != "user_revoke" || w[0].Args["principal"] != "cy@example.com" {
		t.Fatal(w)
	}
}

// An invite is typed as EMAIL PROJECT=ROLE..., and its code shows once.
func TestAccountIssuesAnInvite(t *testing.T) {
	a := &accountAuthority{principal: "lead@example.com", owner: "owner@example.com"}
	m := accountModel(t, a)
	m = press(t, m, "i")
	m = typed(t, m, "ada@example.com api")
	m = press(t, m, "enter")
	if len(a.writes()) != 0 || !strings.Contains(m.State().Error, "PROJECT=ROLE") {
		t.Fatal(a.writes(), m.State().Error)
	}
	m = press(t, m, "i")
	m = typed(t, m, "Ada@Example.com api=member web=reader")
	m = press(t, m, "enter")
	w := a.writes()
	if len(w) != 1 || w[0].Args["email"] != "ada@example.com" || w[0].Args["purpose"] != "user" || fmt.Sprint(w[0].Args["grants"]) != "map[api:member web:reader]" {
		t.Fatal(w)
	}
	if !strings.Contains(Render(m), testCode) || !strings.Contains(m.account.codeNote, "for ada@example.com only") {
		t.Fatal(Render(m))
	}
}

// Labelling is for one's own active devices.
func TestAccountLabelsOwnDevices(t *testing.T) {
	a := &accountAuthority{principal: "owner@example.com", owner: "owner@example.com"}
	m := accountModel(t, a)
	m = selectRow(t, m, "device:dev_cy")
	m = press(t, m, "l")
	if m.inputMode != inputNone {
		t.Fatal("offered to label another member's device")
	}
	m = selectRow(t, m, "device:dev_this")
	m = press(t, m, "l")
	m = typed(t, m, " 2")
	m = press(t, m, "enter")
	w := a.writes()
	if len(w) != 1 || w[0].Operation != "device_label" || w[0].Args["label"] != "laptop 2" {
		t.Fatal(w)
	}
}

// The main owner offers ownership with "transfer" typed; the recipient
// sees a banner and accepts or declines with the word typed; nobody else
// can answer it.
func TestAccountOwnershipTransfer(t *testing.T) {
	a := &accountAuthority{principal: "owner@example.com", owner: "owner@example.com"}
	m := accountModel(t, a)
	m = press(t, m, "t")
	m = typed(t, m, "cy@example.com")
	m = press(t, m, "enter")
	if len(a.writes()) != 0 || m.AccountState().Pending != "owner_transfer_offer" {
		t.Fatal("offered before the confirmation")
	}
	m = typed(t, m, "transfer")
	m = press(t, m, "enter")
	if w := a.writes(); len(w) != 1 || w[0].Args["to"] != "cy@example.com" {
		t.Fatal(w)
	}
	a = &accountAuthority{principal: "cy@example.com", owner: "owner@example.com", transfer: map[string]any{"from": "owner@example.com", "to": "cy@example.com", "expires_at": "2026-10-29"}}
	m = accountModel(t, a)
	if !strings.Contains(Render(m), "Ownership offered to you by owner@example.com") {
		t.Fatal(Render(m))
	}
	m = press(t, m, "t")
	if m.inputMode != inputNone {
		t.Fatal("a member was offered the transfer")
	}
	m = press(t, m, "y")
	m = typed(t, m, "accept")
	m = press(t, m, "enter")
	if w := a.writes(); len(w) != 1 || w[0].Operation != "owner_transfer_respond" || w[0].Args["action"] != "accept" {
		t.Fatal(w)
	}
	// d declines; n, a global "new message" elsewhere, doesn't here.
	a = &accountAuthority{principal: "cy@example.com", owner: "owner@example.com", transfer: map[string]any{"from": "owner@example.com", "to": "cy@example.com", "expires_at": "2026-10-29"}}
	m = accountModel(t, a)
	if !strings.Contains(Render(m), "y accept ownership · d decline ownership") {
		t.Fatal(Render(m))
	}
	if m = press(t, m, "n"); m.AccountState().Pending != "" {
		t.Fatal("n answered the offer")
	}
	m = press(t, m, "d")
	m = typed(t, m, "decline")
	m = press(t, m, "enter")
	if w := a.writes(); len(w) != 1 || w[0].Args["action"] != "decline" {
		t.Fatal(w)
	}
	// Someone else sees no banner and can't answer.
	a = &accountAuthority{principal: "dee@example.com", owner: "owner@example.com", transfer: nil}
	m = accountModel(t, a)
	m = press(t, m, "y", "d", "c")
	if m.AccountState().Pending != "" {
		t.Fatal("a bystander answered the offer")
	}
}

// Local mode has no account, and asks nothing.
func TestAccountLocalModeAsksNothing(t *testing.T) {
	a := &accountAuthority{principal: "owner@example.com", owner: "owner@example.com"}
	cfg := testConfig(a)
	cfg.Mode = ModeLocal
	m, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	m, cmd := m.Show(ScreenAccount)
	m = settle(t, m, cmd)
	m = press(t, m, "a", "i", "x")
	if len(a.calls) != 0 || !strings.Contains(Render(m), "local mode has no invites or devices") {
		t.Fatal(a.calls, Render(m))
	}
}

// R108-M2: a label is for the device selected when l was pressed, even if
// a reload in between moves the rows.
func TestAccountLabelKeepsItsDevice(t *testing.T) {
	a := &accountAuthority{principal: "owner@example.com", owner: "owner@example.com"}
	m := accountModel(t, a)
	m = selectRow(t, m, "device:dev_this")
	m = press(t, m, "l")
	a.mu.Lock()
	a.newDevice = true
	a.mu.Unlock()
	m.request++
	m, cmd := m.accountRefresh()
	m = settle(t, m, cmd)
	if m.AccountState().Rows[0] != "device:dev_new" || m.inputMode != inputAccountLabel {
		t.Fatal(m.AccountState().Rows, m.inputMode)
	}
	m = typed(t, m, "!")
	m = press(t, m, "enter")
	if w := a.writes(); len(w) != 1 || w[0].Args["device_id"] != "dev_this" || w[0].Args["label"] != "laptop!" {
		t.Fatal(w)
	}
}

// R108-L1, L2: no write starts while the screen loads or a write is in
// flight, and an action awaiting its word survives a load.
func TestAccountOneWriteAtATime(t *testing.T) {
	a := &accountAuthority{principal: "owner@example.com", owner: "owner@example.com"}
	m := accountModel(t, a)
	// A revoke in flight: its answer hasn't come back.
	m = press(t, m, "x")
	m = typed(t, m, "revoke")
	m, inFlight := m.UpdateModel(tea.KeyMsg{Type: tea.KeyEnter})
	for _, k := range []string{"a", "x", "i", "t"} {
		m, _ = m.UpdateModel(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(k)})
		if m.AccountState().Pending != "" || m.inputMode != inputNone {
			t.Fatal(k, "started a write while one was in flight")
		}
	}
	m = settle(t, m, inFlight)
	if w := a.writes(); len(w) != 1 || w[0].Operation != "device_revoke" {
		t.Fatal(w)
	}
	// An action begun before a load lands keeps its word through it.
	m.request++
	m, load := m.accountRefresh()
	m.loading = false
	m = press(t, m, "a")
	m.loading = true
	m = settle(t, m, load)
	m = typed(t, m, "add")
	m = press(t, m, "enter")
	if !a.did("invite_create") || !m.AccountState().CodeShown {
		t.Fatal("a load dropped the pending action:", a.writes())
	}
}

// R108-L1: Enter on a confirmation while a load is in flight sends
// nothing, and keeps the confirmation for once the load lands.
func TestAccountConfirmationWaitsForTheLoad(t *testing.T) {
	a := &accountAuthority{principal: "owner@example.com", owner: "owner@example.com"}
	m := accountModel(t, a)
	m = selectRow(t, m, "invite:inv_1")
	m = press(t, m, "x")
	m = typed(t, m, "revoke")
	m.request++
	m.loading = true
	m, load := m.accountRefresh()
	m, _ = m.UpdateModel(tea.KeyMsg{Type: tea.KeyEnter})
	if len(a.writes()) != 0 || m.AccountState().Pending != "invite_revoke" || !strings.Contains(m.State().Error, "press Enter again") {
		t.Fatal(a.writes(), m.AccountState(), m.State().Error)
	}
	m = settle(t, m, load)
	m = press(t, m, "enter")
	if !a.did("invite_revoke") {
		t.Fatal(a.writes())
	}
}

// A device added to or revoked from this user shows as a banner until it
// is marked seen with e, which acknowledges exactly the events shown
// (§17.3); the label's escapes are shown, not run (R1).
func TestAccountShowsDeviceEvents(t *testing.T) {
	a := &accountAuthority{principal: "ada@example.com", owner: "owner@example.com", events: []any{
		map[string]any{"event_id": "dve_1", "kind": "added", "label": "evil\x1b[2K", "device_id": "dev_new", "principal": "ada@example.com", "route": "device_add", "created_at": "2026-10-27T14:02"},
		map[string]any{"event_id": "dve_2", "kind": "revoked", "label": "desk", "device_id": "dev_desk", "principal": "ada@example.com", "actor": "ada@example.com", "created_at": "2026-10-27T14:05"},
		map[string]any{"event_id": "dve_3", "kind": "from_a_later_server", "label": "old\x1b[2K", "device_id": "dev_old", "principal": "ada@example.com", "created_at": "2026-10-27T14:06"},
		map[string]any{"event_id": "dve_4", "kind": "dormant_soon", "label": "idle", "device_id": "dev_idle", "principal": "ada@example.com", "created_at": "2026-10-27T14:07", "due_at": "2026-11-06T14:07"},
	}}
	m := accountModel(t, a)
	view := m.View()
	if strings.Count(view, "New device") != 1 || !strings.Contains(view, `Device "old\x1b[2K" (dev_old) of ada@example.com: from a later server`) || !strings.Contains(view, `Device "idle" (dev_idle) of ada@example.com goes dormant at 2026-11-06T14:07 unless used`) || !strings.Contains(view, `New device "evil\x1b[2K" (dev_new) joined ada@example.com`) || !strings.Contains(view, `Device "desk" (dev_desk) of ada@example.com was revoked`) || strings.Contains(view, "\x1b[2K") {
		t.Fatal(view)
	}
	m = press(t, m, "e")
	writes := a.writes()
	if len(writes) != 1 || writes[0].Operation != "device_ack" || writes[0].Args["all"] != true {
		t.Fatal(writes)
	}
	if strings.Contains(m.View(), "New device") {
		t.Fatal("the banner stayed after the ack")
	}
	// A flood shows the newest five and a count (M2); dates are minutes.
	for i := 0; i < 7; i++ {
		a.events = append(a.events, map[string]any{"event_id": fmt.Sprintf("dve_%d", i), "kind": "added", "label": fmt.Sprintf("d%d", i), "device_id": "dev_x", "principal": "ada@example.com", "route": "invite", "created_at": "2026-10-27T14:02:00.123456789Z"})
	}
	m = press(t, m, "r")
	view = m.View()
	if strings.Count(view, "New device") != 5 || !strings.Contains(view, "…and 2 more") || strings.Contains(view, ".123456789") {
		t.Fatal(view)
	}
	// A Google sign-in has no device events to read.
	g := &accountAuthority{principal: "ada@example.com", owner: "owner@example.com"}
	cfg := testConfig(g)
	gm, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	gm, cmd := gm.SetSize(140, 60).Show(ScreenAccount)
	settle(t, gm, cmd)
	for _, r := range g.calls {
		if r.Operation == "device_events" {
			t.Fatal("a Google sign-in read device events")
		}
	}
}

// With the server known, the one-time box comes with the invite link
// (§17.1), unwrapped below it; it goes when the box does.
func TestAccountCodeShowsTheLink(t *testing.T) {
	a := &accountAuthority{principal: "ada@example.com", owner: "owner@example.com"}
	cfg := testConfig(a)
	cfg.DeviceID, cfg.Server, cfg.Workspace = "dev_this", "https://cb.example.com", "acme"
	m, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	m, cmd := m.SetSize(140, 60).Show(ScreenAccount)
	m = settle(t, m, cmd)
	m = press(t, m, "a")
	m = typed(t, m, "add")
	m = press(t, m, "enter")
	link := "https://cb.example.com/join#cbj1.acme.YWRhQGV4YW1wbGUuY29t.CBJ-4K7P-WQ9D-2MXH-T6RB"
	if view := Render(m); !strings.Contains(view, "Invite link:") || !strings.Contains(view, link) || !strings.Contains(view, "clawbridge join, then paste it") {
		t.Fatal(view)
	}
	m = press(t, m, "esc")
	if strings.Contains(Render(m), "cbj1.") || m.account.link != "" {
		t.Fatal("the link outlived its box")
	}
}
