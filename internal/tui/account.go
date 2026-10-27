package tui

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"clawbridge/internal/bus"
	"clawbridge/internal/devicekey"
	tea "github.com/charmbracelet/bubbletea"
)

// The Account screen (live/AUTH-INVITE-DESIGN.md §11): this user's devices,
// and for owners the members, their devices and the invites, plus the
// ownership transfer. Every write goes through the same operations as the
// CLI's invite, device, user and owner commands, and every destructive one
// waits for a typed confirmation.

const ScreenAccount Screen = "account"
const Account Screen = ScreenAccount

// accountRow is one selectable line: a device, a member or an invite.
type accountRow struct {
	kind   string // "device", "user" or "invite"
	id     string // device ID, member email or invite ID
	owner  string // a device's principal
	label  string
	status string
	detail string
}

// shownDeviceEvents is how many device events the banner lists: the
// newest, then a count of the rest.
const shownDeviceEvents = 5

type accountState struct {
	principal string
	mainOwner bool
	devices   []accountRow // this user's own, and the CI devices they made
	members   []accountRow // the main owner's view: members, each followed by their devices
	invites   []accountRow
	transfer  map[string]any
	// events are this device's unacknowledged device events (§17.3): a
	// device added to or revoked from this user, or, for the main owner, any
	// member. Only a device-key sign-in has them.
	events []map[string]any
	// code is an invite code just issued. It is shown once, in a modal,
	// and dropped when the modal closes; nothing redraws it.
	code     string
	codeNote string
	// link is the invite link for code (§17.1), shown with it, once.
	link string
	// pending is the destructive action waiting for its typed phrase.
	pending *accountAction
	// labelling is the device a label draft is for, fixed when l is
	// pressed, so a reload in between can't move it (R108-M2).
	labelling string
}

// accountAction is one write, with the phrase a person types to confirm it
// when it is destructive.
type accountAction struct {
	op     string
	args   map[string]any
	prompt string
	phrase string
}

// AccountSnapshot is the Account screen's display state, without any
// invite code.
type AccountSnapshot struct {
	Principal string
	MainOwner bool
	Rows      []string
	Transfer  map[string]any
	CodeShown bool
	Pending   string
}

func (m Model) AccountState() AccountSnapshot {
	s := AccountSnapshot{Principal: m.account.principal, MainOwner: m.account.mainOwner, Rows: m.accountRowIDs(), Transfer: m.account.transfer, CodeShown: m.account.code != ""}
	if m.account.pending != nil {
		s.Pending = m.account.pending.op
	}
	return s
}

type accountLoadedMsg struct {
	request uint64
	state   accountState
	err     error
}

type accountActionMsg struct {
	request uint64
	op      string
	result  bus.Result
	err     error
}

func (m Model) accountRows() []accountRow {
	rows := append([]accountRow(nil), m.account.devices...)
	rows = append(rows, m.account.members...)
	return append(rows, m.account.invites...)
}

func (m Model) accountRowIDs() []string {
	rows := m.accountRows()
	out := make([]string, len(rows))
	for i, r := range rows {
		out[i] = r.kind + ":" + r.id
	}
	return out
}

func (m Model) accountSelected() (accountRow, bool) {
	rows := m.accountRows()
	if m.selected < 0 || m.selected >= len(rows) {
		return accountRow{}, false
	}
	return rows[m.selected], true
}

func (m Model) accountRequest(op string, args map[string]any) bus.Request {
	return bus.Request{Operation: op, Workspace: m.cfg.Workspace, Args: args}
}

// accountRefresh reads the screen: who this is, their devices, a pending
// ownership offer, the invites they may see, and for the main owner every
// member with their devices.
func (m Model) accountRefresh() (Model, tea.Cmd) {
	requestID := m.request
	if m.cfg.Mode != ModeCloud {
		return m, func() tea.Msg {
			return accountLoadedMsg{request: requestID, err: errors.New("Account works on a hosted workspace; local mode has no invites or devices")}
		}
	}
	if m.cfg.Executor == nil || m.cfg.Workspace == "" {
		return m, func() tea.Msg {
			return accountLoadedMsg{request: requestID, err: errors.New("no workspace authority is configured; complete join setup first")}
		}
	}
	executor, ctx := m.cfg.Executor, m.cfg.Context
	read := func(op string, args map[string]any) (map[string]any, error) {
		result, err := executor.Execute(ctx, m.accountRequest(op, args))
		if err != nil {
			return nil, err
		}
		if result.Source == "offline-mirror" {
			return nil, errors.New("the authority is offline; Account needs the server")
		}
		return mapData(result.Data), nil
	}
	return m, func() tea.Msg {
		var s accountState
		who, err := read("whoami", map[string]any{})
		if err != nil {
			return accountLoadedMsg{request: requestID, err: err}
		}
		s.principal = text(who, "principal")
		s.mainOwner = s.principal != "" && text(mapData(who["workspace"]), "owner_principal") == s.principal
		devices, err := read("device_list", map[string]any{})
		if err != nil {
			return accountLoadedMsg{request: requestID, err: err}
		}
		for _, d := range records(devices["devices"]) {
			s.devices = append(s.devices, deviceRow(d))
		}
		transfer, err := read("owner_transfer_get", map[string]any{})
		if err != nil {
			return accountLoadedMsg{request: requestID, err: err}
		}
		s.transfer = mapData(transfer["pending"])
		if m.cfg.DeviceID != "" {
			events, err := read("device_events", map[string]any{})
			if err != nil {
				return accountLoadedMsg{request: requestID, err: err}
			}
			s.events = records(events["events"])
		}
		invites, err := read("invite_list", map[string]any{})
		if err != nil {
			return accountLoadedMsg{request: requestID, err: err}
		}
		for _, i := range records(invites["invites"]) {
			s.invites = append(s.invites, inviteRow(i))
		}
		if s.mainOwner {
			users, err := read("user_list", map[string]any{})
			if err != nil {
				return accountLoadedMsg{request: requestID, err: err}
			}
			for _, u := range records(users["users"]) {
				email := text(u, "principal")
				if email == s.principal {
					continue // this user's devices are listed above
				}
				roles := []string{}
				for _, r := range records(u["roles"]) {
					roles = append(roles, text(r, "slug")+"="+text(r, "role"))
				}
				devices := records(u["devices"])
				s.members = append(s.members, accountRow{kind: "user", id: email, detail: fmt.Sprintf("%s · %d devices · invited by %s", firstNonempty(strings.Join(roles, ", "), "no roles"), len(devices), firstNonempty(text(u, "invited_by"), "-"))})
				for _, d := range devices {
					s.members = append(s.members, deviceRow(d))
				}
			}
		}
		return accountLoadedMsg{request: requestID, state: s}
	}
}

func deviceRow(d map[string]any) accountRow {
	kind := text(d, "kind")
	detail := "added " + firstNonempty(text(d, "created_at"), "-") + " · last used " + firstNonempty(text(d, "last_used_at"), "never")
	if kind == "ci" {
		detail = "CI · " + detail + " · expires " + firstNonempty(text(d, "expires_at"), "-")
	}
	if store := text(d, "key_store"); store != "" {
		detail += " · key in " + store
	}
	return accountRow{kind: "device", id: text(d, "device_id"), owner: text(d, "principal"), label: text(d, "label"), status: text(d, "status"), detail: detail}
}

func inviteRow(i map[string]any) accountRow {
	grants := []string{}
	for project, role := range mapData(i["grants"]) {
		grants = append(grants, fmt.Sprintf("%s=%v", project, role))
	}
	detail := text(i, "purpose") + " · expires " + firstNonempty(text(i, "expires_at"), "-")
	if len(grants) > 0 {
		detail += " · " + strings.Join(sortedStrings(grants), ", ")
	}
	if text(i, "status") == "burnt" {
		detail += " · burnt after failed attempts at " + firstNonempty(text(i, "revoked_at"), "-")
	}
	return accountRow{kind: "invite", id: text(i, "invite_id"), owner: text(i, "email"), status: text(i, "status"), detail: detail}
}

func sortedStrings(values []string) []string {
	out := append([]string(nil), values...)
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j] < out[j-1]; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}

func (m Model) applyAccount(x accountLoadedMsg) Model {
	if x.request != m.request || m.screen != ScreenAccount {
		return m
	}
	m.loading = false
	if x.err != nil {
		m.errText = displayError(x.err)
		return m
	}
	m.errText = ""
	m.source = "server"
	// A code on show, an action awaiting its word and a label draft's
	// device survive the reload (R108-L2).
	code, note, link, pending, labelling := m.account.code, m.account.codeNote, m.account.link, m.account.pending, m.account.labelling
	m.account = x.state
	m.account.code, m.account.codeNote, m.account.link, m.account.pending, m.account.labelling = code, note, link, pending, labelling
	m.selected = min(m.selected, max(0, len(m.accountRows())-1))
	return m
}

// accountDo sends one write. A write never runs against an offline
// mirror, and its answer reloads the screen.
func (m Model) accountDo(action accountAction) (Model, tea.Cmd) {
	if m.loading {
		// One write at a time, on a screen that shows its answer (R108-L1);
		// a confirmation stays, for Enter again once the load lands.
		m.actionErr = "wait for the screen to finish loading, then press Enter again"
		return m, nil
	}
	m.account.pending = nil
	m.confirming = false
	m.confirm = ""
	if m.writesBlocked() {
		m.actionErr = "the authority is offline; nothing was changed"
		return m, nil
	}
	m.request++
	m.loading = true
	m.actionErr = ""
	requestID, executor, ctx, req := m.request, m.cfg.Executor, m.cfg.Context, m.accountRequest(action.op, action.args)
	return m, func() tea.Msg {
		result, err := executor.Execute(ctx, req)
		if err == nil && result.Source == "offline-mirror" {
			err = errors.New("authority returned an offline-mirror result for a write; nothing was applied or queued")
		}
		return accountActionMsg{request: requestID, op: action.op, result: result, err: err}
	}
}

func (m Model) applyAccountAction(x accountActionMsg) (Model, tea.Cmd) {
	if x.request != m.request {
		return m, nil
	}
	m.loading = false
	if x.err != nil {
		m.actionErr = displayError(x.err)
		return m, nil
	}
	data := mapData(x.result.Data)
	switch x.op {
	case "invite_create":
		m.account.code = text(data, "code")
		email, server := text(data, "email"), firstNonempty(m.cfg.Server, "URL")
		m.account.link, _ = devicekey.FormatLink(m.cfg.Server, m.cfg.Workspace, email, m.account.code)
		switch {
		case m.account.link != "":
			m.account.codeNote = "Give the link below privately; it works once, for " + email + " only, until " + text(data, "expires_at") + ". On the new machine: clawbridge join, then paste it."
		case text(data, "purpose") == "device":
			m.account.codeNote = "On the new machine: clawbridge --server " + server + " join --email " + email + ". The code works once, until " + text(data, "expires_at") + "."
		default:
			m.account.codeNote = "Give " + email + " the code and this server's URL (" + server + ") over a channel you'd trust with a password. It works once, for " + email + " only, until " + text(data, "expires_at") + "."
		}
		m.actionErr = "Invite " + text(data, "invite_id") + " issued."
	case "device_revoke":
		m.actionErr = "Device " + text(data, "device_id") + " revoked."
	case "device_label":
		m.actionErr = "Device " + text(data, "device_id") + " relabelled."
	case "device_ack":
		m.actionErr = "Device events marked seen on this device."
	case "invite_revoke":
		m.actionErr = "Invite " + text(data, "invite_id") + " revoked."
	case "user_revoke":
		m.actionErr = fmt.Sprintf("%s removed: %d devices revoked.", text(data, "principal"), len(stringsOf(data["revoked_devices"])))
	case "owner_transfer_offer":
		m.actionErr = "Ownership offered to " + text(data, "to") + "; they accept from their own device."
	case "owner_transfer_respond":
		m.actionErr = "Ownership offer " + text(data, "status") + "."
	}
	m.request++
	m.loading = true
	return m.accountRefresh()
}

func stringsOf(v any) []string {
	out := []string{}
	if list, ok := v.([]any); ok {
		for _, item := range list {
			if s, ok := item.(string); ok {
				out = append(out, s)
			}
		}
	}
	if list, ok := v.([]string); ok {
		out = append(out, list...)
	}
	return out
}

// confirmAccount asks for the typed phrase before a destructive write.
func (m Model) confirmAccount(action accountAction) Model {
	m.account.pending = &action
	m.confirming = true
	m.confirm = ""
	m.actionErr = ""
	return m
}

func (m Model) accountKey(key string) (Model, tea.Cmd) {
	if m.account.code != "" {
		// The code shows until Enter or Esc, and is then gone for good.
		if key == "enter" || key == "esc" {
			m.account.code, m.account.codeNote, m.account.link = "", "", ""
		}
		return m, nil
	}
	if m.cfg.Mode != ModeCloud || m.account.principal == "" {
		return m, nil
	}
	if m.loading && strings.Contains("ailxtynce", key) && len(key) == 1 {
		m.actionErr = "wait for the screen to finish loading"
		return m, nil
	}
	row, selected := m.accountSelected()
	switch key {
	case "a":
		// A live code is minted only on a typed word (R108-L4).
		return m.confirmAccount(accountAction{op: "invite_create", args: map[string]any{"email": m.account.principal, "purpose": "device"}, prompt: "Make a one-time code to add another device of yours? Type add", phrase: "add"}), nil
	case "i":
		m.inputMode, m.draft, m.actionErr = inputAccountInvite, "", ""
	case "l":
		if !selected || row.kind != "device" || row.owner != m.account.principal || row.status != "active" {
			m.actionErr = "select one of your active devices to label it"
			return m, nil
		}
		m.inputMode, m.draft, m.actionErr, m.account.labelling = inputAccountLabel, row.label, "", row.id
	case "x":
		if !selected {
			return m, nil
		}
		switch {
		case row.kind == "device" && row.status == "active":
			prompt := "Revoke device " + row.id + " (" + row.label + ")"
			if row.id == m.cfg.DeviceID {
				prompt += ", THIS device: this profile stops working"
			}
			return m.confirmAccount(accountAction{op: "device_revoke", args: map[string]any{"device_id": row.id, "reason": "revoked in the TUI"}, prompt: prompt + "? Type revoke", phrase: "revoke"}), nil
		case row.kind == "invite" && row.status == "pending":
			return m.confirmAccount(accountAction{op: "invite_revoke", args: map[string]any{"invite_id": row.id}, prompt: "Revoke invite " + row.id + " for " + row.owner + "? Type revoke", phrase: "revoke"}), nil
		case row.kind == "user" && m.account.mainOwner:
			return m.confirmAccount(accountAction{op: "user_revoke", args: map[string]any{"principal": row.id}, prompt: "Remove " + row.id + " from the workspace: every role, device and pending invite here. Type their email", phrase: row.id}), nil
		}
		m.actionErr = "nothing to revoke on this row"
	case "t":
		if !m.account.mainOwner {
			m.actionErr = "only the workspace owner may offer ownership"
			return m, nil
		}
		if m.account.transfer != nil {
			m.actionErr = "an ownership offer is already pending; c cancels it"
			return m, nil
		}
		m.inputMode, m.draft, m.actionErr = inputAccountTransfer, "", ""
	case "y", "n":
		if text(m.account.transfer, "to") != m.account.principal {
			return m, nil
		}
		verb := map[string]string{"y": "accept", "n": "decline"}[key]
		prompt := "Decline the ownership offer from " + text(m.account.transfer, "from") + "? Type decline"
		if verb == "accept" {
			prompt = "Become this workspace's owner, offered by " + text(m.account.transfer, "from") + "? Type accept"
		}
		return m.confirmAccount(accountAction{op: "owner_transfer_respond", args: map[string]any{"action": verb}, prompt: prompt, phrase: verb}), nil
	case "e":
		if len(m.account.events) == 0 {
			return m, nil
		}
		// Acknowledging changes nothing but this device's alert, so it
		// takes no typed word. It acknowledges every pending event, however
		// many: the banner shows only the newest.
		return m.accountDo(accountAction{op: "device_ack", args: map[string]any{"all": true}})
	case "c":
		if text(m.account.transfer, "from") != m.account.principal {
			return m, nil
		}
		return m.confirmAccount(accountAction{op: "owner_transfer_respond", args: map[string]any{"action": "cancel"}, prompt: "Cancel the ownership offer to " + text(m.account.transfer, "to") + "? Type cancel", phrase: "cancel"}), nil
	}
	return m, nil
}

// accountSubmit finishes an Account input or confirmation on Enter.
func (m Model) accountSubmit() (Model, tea.Cmd) {
	if m.confirming {
		action := m.account.pending
		if action == nil {
			return m.CancelInput(), nil
		}
		if strings.TrimSpace(m.confirm) != action.phrase {
			m.actionErr = "the confirmation doesn't match; nothing was changed"
			return m, nil
		}
		return m.accountDo(*action)
	}
	draft := strings.TrimSpace(m.draft)
	mode := m.inputMode
	m.inputMode, m.draft = inputNone, ""
	switch mode {
	case inputAccountLabel:
		id := m.account.labelling
		m.account.labelling = ""
		if id == "" {
			m.actionErr = "select one of your devices to label it"
			return m, nil
		}
		if draft == "" || len(draft) > 64 {
			m.actionErr = "a device label is 1-64 characters"
			return m, nil
		}
		return m.accountDo(accountAction{op: "device_label", args: map[string]any{"device_id": id, "label": draft}})
	case inputAccountInvite:
		email, grants, err := parseInviteDraft(draft)
		if err != nil {
			m.actionErr = err.Error()
			return m, nil
		}
		return m.accountDo(accountAction{op: "invite_create", args: map[string]any{"email": email, "purpose": "user", "grants": grants}})
	case inputAccountTransfer:
		if !strings.Contains(draft, "@") {
			m.actionErr = "offer ownership to a member's email"
			return m, nil
		}
		to := strings.ToLower(draft)
		return m.confirmAccount(accountAction{op: "owner_transfer_offer", args: map[string]any{"to": to}, prompt: "Offer ownership of this workspace to " + to + "? You keep an owner role on every project. Type transfer", phrase: "transfer"}), nil
	}
	return m, nil
}

// parseInviteDraft reads "EMAIL PROJECT=ROLE [PROJECT=ROLE]...".
func parseInviteDraft(draft string) (string, map[string]any, error) {
	fields := strings.Fields(draft)
	if len(fields) < 2 || !strings.Contains(fields[0], "@") {
		return "", nil, errors.New("an invite is EMAIL PROJECT=ROLE, such as ada@example.com api=member")
	}
	grants := map[string]any{}
	for _, g := range fields[1:] {
		project, role, ok := strings.Cut(g, "=")
		if !ok || project == "" || role == "" {
			return "", nil, errors.New("each grant is PROJECT=ROLE, such as api=member")
		}
		if _, dup := grants[project]; dup {
			return "", nil, errors.New("the invite names " + project + " twice")
		}
		grants[project] = role
	}
	return strings.ToLower(fields[0]), grants, nil
}

func renderAccount(m Model) string {
	var b strings.Builder
	b.WriteString(title(ScreenAccount))
	b.WriteString("  ")
	b.WriteString(mutedStyle.Render("your devices · members and invites for owners"))
	b.WriteString("\n\n")
	if m.account.code != "" {
		modal := warnStyle.Render("Invite code (shown once, never again)") + "\n\n  " + accentStyle.Render(terminalText(m.account.code)) + "\n\n" +
			terminalText(m.account.codeNote) + "\nCopy it now. Enter or Esc closes this, and the code is gone."
		b.WriteString(boxStyle.Width(max(20, min(m.width-8, 80))).Render(modal))
		if m.account.link != "" {
			// Outside the box and unwrapped, so it copies as one line.
			b.WriteString("\n\nInvite link:\n" + terminalText(m.account.link))
		}
		return b.String()
	}
	if m.cfg.Mode != ModeCloud {
		b.WriteString("Account works on a hosted workspace; local mode has no invites or devices.")
		return b.String()
	}
	if m.account.principal == "" {
		b.WriteString("Not loaded. r reloads.")
		return b.String()
	}
	role := "member"
	if m.account.mainOwner {
		role = "workspace owner"
	}
	b.WriteString(kv("Signed in as", terminalText(m.account.principal)+" ("+role+")"))
	if p := m.account.pending; p != nil {
		b.WriteString(warnStyle.Render(terminalText(p.prompt)) + "\n")
	}
	for i, e := range m.account.events {
		if i == shownDeviceEvents {
			b.WriteString(warnStyle.Render(fmt.Sprintf("…and %d more (clawbridge device events)", len(m.account.events)-i)) + "\n")
			break
		}
		at := text(e, "created_at")
		if t, err := time.Parse(time.RFC3339Nano, at); err == nil {
			at = t.Local().Format("2006-01-02 15:04")
		}
		var what string
		switch text(e, "kind") {
		case "added":
			what = "New device " + strconv.Quote(text(e, "label")) + " (" + text(e, "device_id") + ") joined " + text(e, "principal") + " at " + at + " via " + strings.ReplaceAll(text(e, "route"), "_", " ")
		case "revoked":
			what = "Device " + strconv.Quote(text(e, "label")) + " (" + text(e, "device_id") + ") of " + text(e, "principal") + " was revoked at " + at + " by " + text(e, "actor")
		default:
			// A kind this client doesn't know is shown as it is, never as
			// a new device.
			what = "Device " + strconv.Quote(text(e, "label")) + " (" + text(e, "device_id") + ") of " + text(e, "principal") + ": " + strings.ReplaceAll(text(e, "kind"), "_", " ") + " at " + at
		}
		b.WriteString(warnStyle.Render(terminalText(what)) + "\n")
	}
	if len(m.account.events) > 0 {
		b.WriteString(mutedStyle.Render("Not you? Select the device and x revokes it · e marks these seen") + "\n")
	}
	if t := m.account.transfer; t != nil {
		switch m.account.principal {
		case text(t, "to"):
			b.WriteString(warnStyle.Render("Ownership offered to you by "+terminalText(text(t, "from"))+" until "+terminalText(text(t, "expires_at"))) + " · y accept · n decline\n")
		case text(t, "from"):
			b.WriteString(warnStyle.Render("Your ownership offer to "+terminalText(text(t, "to"))+" is pending until "+terminalText(text(t, "expires_at"))) + " · c cancel\n")
		}
	}
	index := 0
	line := func(r accountRow, text string) {
		prefix := "  "
		if index == m.selected {
			prefix = "› "
		}
		out := prefix + terminalText(text)
		if index == m.selected {
			out = accentStyle.Render(out)
		} else if r.status != "" && r.status != "active" && r.status != "pending" {
			out = mutedStyle.Render(out)
		}
		b.WriteString(out + "\n")
		index++
	}
	device := func(r accountRow, indent string) {
		this := ""
		if r.id == m.cfg.DeviceID && r.id != "" {
			this = " (this device)"
		}
		line(r, fmt.Sprintf("%s%s · %s%s · %s · %s", indent, r.label, r.id, this, r.status, r.detail))
	}
	b.WriteString("\nYour devices\n")
	if len(m.account.devices) == 0 {
		b.WriteString(mutedStyle.Render("  none") + "\n")
	}
	for _, r := range m.account.devices {
		device(r, "")
	}
	if m.account.mainOwner {
		b.WriteString("\nMembers\n")
		if len(m.account.members) == 0 {
			b.WriteString(mutedStyle.Render("  no other members") + "\n")
		}
		for _, r := range m.account.members {
			if r.kind == "user" {
				line(r, r.id+" · "+r.detail)
			} else {
				device(r, "    ")
			}
		}
	}
	b.WriteString("\nInvites\n")
	if len(m.account.invites) == 0 {
		b.WriteString(mutedStyle.Render("  none") + "\n")
	}
	for _, r := range m.account.invites {
		line(r, fmt.Sprintf("%s · %s · %s · %s", r.owner, r.id, r.status, r.detail))
	}
	keys := "\na add a device (one-time code) · l label · x revoke · i invite"
	if m.account.mainOwner {
		keys += " · t transfer ownership"
	}
	b.WriteString(mutedStyle.Render(keys + " · r reload"))
	return b.String()
}
