package tui

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"clawbridge/internal/bus"
	"clawbridge/internal/clirender"
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
	// used is a device's last use, a column of its own.
	used string
}

// shownDeviceEvents is how many device events the banner lists: the
// newest, then a count of the rest.
const shownDeviceEvents = 5

// name is how a row is named in a success line: a device by its label,
// anything else by its ID (an email for a member).
func (r accountRow) name() string {
	if r.kind == "device" && r.label != "" {
		return r.label
	}
	return firstNonempty(r.id, "it")
}

// find is the row of that kind and ID as last loaded; a bare row with the
// ID when it is gone.
func (s accountState) find(kind, id string) accountRow {
	for _, list := range [][]accountRow{s.devices, s.members, s.invites} {
		for _, r := range list {
			if r.kind == kind && r.id == id {
				return r
			}
		}
	}
	return accountRow{kind: kind, id: id}
}

type accountState struct {
	// projects are the workspace's projects, for the invite checklist
	// (U54); invite is that checklist while it's open.
	projects  []ProjectRow
	invite    *inviteDraft
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
	executor, ctx, now := m.cfg.Executor, m.cfg.Context, m.now()
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
			s.devices = append(s.devices, deviceRow(now, d))
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
		// U43: grants by project slug; best effort, an ID stays an ID.
		slugs := map[string]string{}
		if projects, e := read("list_projects", map[string]any{}); e == nil {
			s.projects = projectRows(records(projects["projects"]))
			for _, p := range s.projects {
				slugs[p.ID] = p.Slug
			}
		}
		for _, i := range records(invites["invites"]) {
			s.invites = append(s.invites, inviteRow(now, i, slugs))
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
				s.members = append(s.members, accountRow{kind: "user", id: email, detail: firstNonempty(strings.Join(roles, ", "), "no roles") + " · " + plural(len(devices), "device", "devices") + " · invited by " + firstNonempty(text(u, "invited_by"), "-")})
				for _, d := range devices {
					s.members = append(s.members, deviceRow(now, d))
				}
			}
		}
		return accountLoadedMsg{request: requestID, state: s}
	}
}

func deviceRow(now time.Time, d map[string]any) accountRow {
	kind := text(d, "kind")
	used := "last used " + firstNonempty(clirender.When(now, text(d, "last_used_at")), "never")
	detail := "added " + firstNonempty(clirender.When(now, text(d, "created_at")), "-")
	if kind == "ci" {
		detail = "CI · " + detail + " · expires " + firstNonempty(clirender.When(now, text(d, "expires_at")), "-")
	}
	if store := text(d, "key_store"); store != "" {
		detail += " · key in " + store
	}
	if due := text(d, "dormant_at"); due != "" && text(d, "status") == "dormant" {
		detail += " · dormant since " + due + ": another of its user's devices, or the owner, confirms it (w)"
	} else if d["dormant_soon"] == true {
		detail += " · goes dormant " + due + " unless used or confirmed (w)"
	}
	return accountRow{kind: "device", id: text(d, "device_id"), owner: text(d, "principal"), label: text(d, "label"), status: text(d, "status"), detail: detail, used: used}
}

func inviteRow(now time.Time, i map[string]any, slugs map[string]string) accountRow {
	grants := []string{}
	for project, role := range mapData(i["grants"]) {
		grants = append(grants, fmt.Sprintf("%s=%v", firstNonempty(slugs[project], project), role))
	}
	detail := "expires " + firstNonempty(clirender.When(now, text(i, "expires_at")), "-")
	if p := text(i, "purpose"); p != "" && p != "user" {
		detail = p + " · " + detail
	}
	if len(grants) > 0 {
		detail += " · " + strings.Join(sortedStrings(grants), ", ")
	}
	if text(i, "status") == "burnt" {
		detail += " · burnt after failed attempts " + firstNonempty(clirender.When(now, text(i, "revoked_at")), "-")
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
		m.loadErr = displayError(x.err)
		return m
	}
	m.loadErr = ""
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
		m.status = noteStatus("wait for the screen to finish loading, then press Enter again")
		return m, nil
	}
	m.account.pending = nil
	m.confirming = false
	m.confirm = ""
	if m.writesBlocked() {
		m.status = noteStatus("the authority is offline; nothing was changed")
		return m, nil
	}
	m.request++
	m.loading = true
	m.status = status{}
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
		m.status = errStatus(x.err)
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
		m.status = okStatus("Invite for " + email + " created.")
	case "device_revoke":
		m.status = okStatus(m.account.find("device", text(data, "device_id")).name() + " revoked.")
	case "device_confirm":
		m.status = okStatus(m.account.find("device", text(data, "device_id")).name() + " confirmed.")
	case "device_label":
		row := m.account.find("device", text(data, "device_id"))
		m.status = okStatus(row.name() + " renamed to " + firstNonempty(text(data, "label"), "(no label)") + ".")
	case "device_ack":
		m.status = okStatus("Device events marked seen on this device.")
	case "invite_revoke":
		row := m.account.find("invite", text(data, "invite_id"))
		m.status = okStatus("Invite for " + firstNonempty(row.owner, row.id) + " revoked.")
	case "user_revoke":
		m.status = okStatus(fmt.Sprintf("%s removed: %d devices revoked.", text(data, "principal"), len(stringsOf(data["revoked_devices"]))))
	case "owner_transfer_offer":
		m.status = okStatus("Ownership offered to " + text(data, "to") + "; they accept from their own device.")
	case "owner_transfer_respond":
		m.status = okStatus("Ownership offer " + text(data, "status") + ".")
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
	m.status = status{}
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
	if m.loading && strings.Contains("ailxtydce", key) && len(key) == 1 {
		m.status = noteStatus("wait for the screen to finish loading")
		return m, nil
	}
	row, selected := m.accountSelected()
	switch key {
	case "a":
		// A live code is minted only on a typed word (R108-L4).
		return m.confirmAccount(accountAction{op: "invite_create", args: map[string]any{"email": m.account.principal, "purpose": "device"}, prompt: "Make a one-time code to add another device of yours? Type add", phrase: "add"}), nil
	case "i":
		m.inputMode, m.draft, m.status = inputAccountInvite, "", status{}
	case "l":
		if !selected || row.kind != "device" || row.owner != m.account.principal || row.status != "active" {
			m.status = noteStatus("select one of your active devices to label it")
			return m, nil
		}
		m.inputMode, m.draft, m.status, m.account.labelling = inputAccountLabel, row.label, status{}, row.id
	case "x":
		if !selected {
			return m, nil
		}
		switch {
		case row.kind == "device" && (row.status == "active" || row.status == "dormant"):
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
		m.status = noteStatus("nothing to revoke on this row")
	case "w":
		// §17.4: confirm wakes a dormant device, or restarts an idle one's
		// clock; this device's own clock runs on its use.
		if !selected || row.kind != "device" || (row.status != "active" && row.status != "dormant") || row.id == m.cfg.DeviceID {
			m.status = noteStatus("select another active or dormant device to confirm it")
			return m, nil
		}
		args := map[string]any{"device_id": row.id}
		if row.owner != m.account.principal {
			args["principal"] = row.owner
		}
		return m.confirmAccount(accountAction{op: "device_confirm", args: args, prompt: "Confirm device " + row.id + " (" + row.label + ") of " + row.owner + ", so it stays usable? Type confirm", phrase: "confirm"}), nil
	case "t":
		if !m.account.mainOwner {
			m.status = noteStatus("only the workspace owner may offer ownership")
			return m, nil
		}
		if m.account.transfer != nil {
			m.status = noteStatus("an ownership offer is already pending; c cancels it")
			return m, nil
		}
		m.inputMode, m.draft, m.status = inputAccountTransfer, "", status{}
	case "y", "d":
		if text(m.account.transfer, "to") != m.account.principal {
			return m, nil
		}
		verb := map[string]string{"y": "accept", "d": "decline"}[key]
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
			m.status = noteStatus("the confirmation doesn't match; nothing was changed")
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
			m.status = noteStatus("select one of your devices to label it")
			return m, nil
		}
		if draft == "" || len(draft) > 64 {
			m.status = noteStatus("a device label is 1-64 characters")
			return m, nil
		}
		return m.accountDo(accountAction{op: "device_label", args: map[string]any{"device_id": id, "label": draft}})
	case inputAccountInvite:
		if !strings.Contains(draft, "=") && !strings.Contains(strings.TrimSpace(draft), " ") {
			// U54: the email, then a checklist of projects and roles.
			if !strings.Contains(draft, "@") {
				m.status = noteStatus("type the invitee's email")
				return m, nil
			}
			m.inputMode, m.draft = inputNone, ""
			m.account.invite = &inviteDraft{email: strings.ToLower(strings.TrimSpace(draft)), roles: map[string]string{}}
			return m, nil
		}
		email, grants, err := parseInviteDraft(draft)
		if err != nil {
			m.status = noteStatus(err.Error())
			return m, nil
		}
		return m.accountDo(accountAction{op: "invite_create", args: map[string]any{"email": email, "purpose": "user", "grants": grants}})
	case inputAccountTransfer:
		if !strings.Contains(draft, "@") {
			m.status = noteStatus("offer ownership to a member's email")
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
	b.WriteString("\n\n")
	if m.account.invite != nil {
		return b.String() + renderInvite(m)
	}
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
	if body, ok := bodyState(m, "your account", m.account.principal != ""); ok {
		return b.String() + body
	}
	if m.account.principal == "" {
		b.WriteString("Not loaded. " + firstNonempty(keyHint(m, ScreenAccount, "reload"), "r reload") + ".")
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
		due := text(e, "due_at")
		if t, err := time.Parse(time.RFC3339Nano, due); err == nil {
			due = t.Local().Format("2006-01-02 15:04")
		}
		var what string
		switch text(e, "kind") {
		case "added":
			what = "New device " + strconv.Quote(text(e, "label")) + " (" + text(e, "device_id") + ") joined " + text(e, "principal") + " at " + at + " via " + strings.ReplaceAll(text(e, "route"), "_", " ")
		case "revoked":
			what = "Device " + strconv.Quote(text(e, "label")) + " (" + text(e, "device_id") + ") of " + text(e, "principal") + " was revoked at " + at + " by " + text(e, "actor")
		case "reconfirmed":
			what = "Device " + strconv.Quote(text(e, "label")) + " (" + text(e, "device_id") + ") of " + text(e, "principal") + " was re-confirmed at " + at + " by " + text(e, "actor")
		case "dormant_soon":
			what = "Device " + strconv.Quote(text(e, "label")) + " (" + text(e, "device_id") + ") of " + text(e, "principal") + " goes dormant at " + due + " unless used or confirmed (w)"
		case "dormant":
			what = "Device " + strconv.Quote(text(e, "label")) + " (" + text(e, "device_id") + ") of " + text(e, "principal") + " went dormant at " + due + ", unused: w confirms it, x revokes it"
		default:
			// A kind this client doesn't know is shown as it is, never as
			// a new device.
			what = "Device " + strconv.Quote(text(e, "label")) + " (" + text(e, "device_id") + ") of " + text(e, "principal") + ": " + strings.ReplaceAll(text(e, "kind"), "_", " ") + " at " + at
		}
		b.WriteString(warnStyle.Render(terminalText(what)) + "\n")
	}
	if len(m.account.events) > 0 {
		b.WriteString(mutedStyle.Render("Not you? Select the device: "+keyHint(m, ScreenAccount, "revoke")+" · "+keyHint(m, ScreenAccount, "ack")) + "\n")
	}
	if t := m.account.transfer; t != nil {
		switch m.account.principal {
		case text(t, "to"):
			b.WriteString(warnStyle.Render("Ownership offered to you by "+terminalText(text(t, "from"))+" until "+terminalText(text(t, "expires_at"))) + " · " + keyHint(m, ScreenAccount, "accept") + " · " + keyHint(m, ScreenAccount, "decline") + "\n")
		case text(t, "from"):
			b.WriteString(warnStyle.Render("Your ownership offer to "+terminalText(text(t, "to"))+" is pending until "+terminalText(text(t, "expires_at"))) + " · " + keyHint(m, ScreenAccount, "cancel-offer") + "\n")
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
	// U43: aligned columns; the ID and the rest only on the selected row.
	selectedDetail := func(text string) {
		if index-1 == m.selected {
			b.WriteString(mutedStyle.Render("      "+terminalText(text)) + "\n")
		}
	}
	device := func(r accountRow, indent string) {
		name := r.label
		if r.id == m.cfg.DeviceID && r.id != "" {
			name += " (this device)"
		}
		line(r, indent+padRight(clip(name, 30), 31)+padRight(r.status, 10)+r.used)
		selectedDetail(r.id + " · " + r.detail)
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
				line(r, padRight(clip(r.id, 30), 31)+r.detail)
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
		line(r, padRight(clip(r.owner, 30), 31)+padRight(r.status, 10)+r.detail)
		selectedDetail(r.id)
	}
	return strings.TrimRight(b.String(), "\n")
}

// inviteDraft is the invite checklist: the email typed, then a role per
// project (U54).
type inviteDraft struct {
	email  string
	roles  map[string]string // project slug -> role
	cursor int
}

// inviteRoles are the roles Space cycles through; "" grants nothing.
var inviteRoles = []string{"", "reader", "member", "lead", "owner"}

// inviteKey runs the checklist: ↑↓ a project, Space its role, Enter the
// invite, Esc back to the email.
func (m Model) inviteKey(key string) (Model, tea.Cmd) {
	d := m.account.invite
	switch key {
	case "up", "k":
		d.cursor = max(0, d.cursor-1)
	case "down", "j":
		d.cursor = min(max(0, len(m.account.projects)-1), d.cursor+1)
	case " ":
		if d.cursor < len(m.account.projects) {
			slug := m.account.projects[d.cursor].Slug
			for i, r := range inviteRoles {
				if r == d.roles[slug] {
					d.roles[slug] = inviteRoles[(i+1)%len(inviteRoles)]
					break
				}
			}
			if d.roles[slug] == "" {
				delete(d.roles, slug)
			}
		}
	case "esc":
		m.account.invite = nil
		m.inputMode, m.draft = inputAccountInvite, d.email
	case "enter":
		grants := map[string]any{}
		for slug, role := range d.roles {
			grants[slug] = role
		}
		m.account.invite = nil
		return m.accountDo(accountAction{op: "invite_create", args: map[string]any{"email": d.email, "purpose": "user", "grants": grants}})
	}
	return m, nil
}

// renderInvite is the checklist.
func renderInvite(m Model) string {
	d := m.account.invite
	var b strings.Builder
	b.WriteString("Invite " + terminalText(d.email) + "\n")
	if len(m.account.projects) == 0 {
		b.WriteString(mutedStyle.Render("No projects to grant yet; the invite lets them in, and they wait for a project.") + "\n")
		return b.String()
	}
	b.WriteString(mutedStyle.Render("Space sets a project's role (reader, member, lead, owner, none); Enter sends the invite.") + "\n")
	for i, p := range m.account.projects {
		prefix := "  "
		if i == d.cursor {
			prefix = accentStyle.Render("› ")
		}
		role := firstNonempty(d.roles[p.Slug], "—")
		b.WriteString(prefix + padRight("["+role+"]", 10) + terminalText(p.Slug))
		if name := terminalText(p.Name); name != "" && name != p.Slug {
			b.WriteString(mutedStyle.Render("  " + name))
		}
		b.WriteString("\n")
	}
	return b.String()
}
