package tui

import (
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"

	"clawbridge/internal/bus"
	"clawbridge/internal/clirender"
)

// threadsState is an opened project's threads (TUI-UX-AUDIT U62): the
// owner's read-only view of who said what to whom, as project_threads and
// project_thread give it. back is the project's row on Projects.
type threadsState struct {
	open         bool
	project      ProjectRow
	back         int
	loaded       bool
	err          string
	rows         []threadRow
	next         string
	readableFrom string
	thread       *threadView
}

// threadView is one opened thread: its messages in order; back is its
// row in the list.
type threadView struct {
	row      threadRow
	back     int
	loaded   bool
	err      string
	messages []threadMessage
	next     int64
}

type threadRow struct {
	ID, Kind, StartedAt, LastAt string
	Messages                    int64
	Participants                []string
	More                        int64
}

type threadMessage struct {
	Sequence                                                     int64
	ID, Sender, Address, ReplyTo, Kind, Body, Coordination, Sent string
	Recipients                                                   []threadRecipient
}

type threadRecipient struct{ Name, DeliveredAt, ReadAt string }

// threadsMsg is a page of the list; more appends it.
type threadsMsg struct {
	request uint64
	more    bool
	data    map[string]any
	err     error
}

// threadMsg is a page of one thread; more appends it.
type threadMsg struct {
	request uint64
	id      string
	more    bool
	data    map[string]any
	err     error
}

// threadsRequest reads as the person: no session and no instance, which
// the server requires of these reads.
func (m Model) threadsRequest(op string, args map[string]any) bus.Request {
	p := m.threads.project
	return bus.Request{Operation: op, Workspace: m.cfg.Workspace, Project: firstNonempty(p.Slug, p.ID), Args: args}
}

// canReadThreads is an opened project the viewer owns.
func canReadThreads(m Model) bool {
	p, ok := m.selectedProject()
	return ok && m.projectOpen && !m.threads.open && p.Role == "owner"
}

// openThreads shows the selected project's threads, reading them. Like
// clawbridge project threads, it's the owner's own act: never in a TUI
// opened with a session, nor one an agent runs.
func (m Model) openThreads() (Model, tea.Cmd) {
	switch {
	case m.cfg.Session != "":
		m.status = noteStatus("The project's threads are read without a session: open the TUI without --session to read them.")
		return m, nil
	case m.cfg.AgentMarker != "":
		m.status = noteStatus("Reading the project's threads is the owner's, and " + m.cfg.AgentMarker + " says an agent runs this TUI; run it yourself in your own terminal.")
		return m, nil
	}
	p, _ := m.selectedProject()
	m.threads = threadsState{open: true, project: p, back: m.selected}
	m.selected = 0
	return m.loadThreads(false)
}

// loadThreads reads the list, or its next page when more.
func (m Model) loadThreads(more bool) (Model, tea.Cmd) {
	args := map[string]any{}
	if more {
		args["before"] = m.threads.next
	} else {
		m.threads.loaded, m.threads.err = false, ""
	}
	m.status = m.status.kept()
	m.request++
	request, r := m.request, m.threadsRequest("project_threads", args)
	return m, func() tea.Msg {
		result, err := m.cfg.Executor.Execute(m.cfg.Context, r)
		return threadsMsg{request: request, more: more, data: mapData(result.Data), err: err}
	}
}

// openThread shows the selected thread, reading it.
func (m Model) openThread() (Model, tea.Cmd) {
	if m.selected < 0 || m.selected >= len(m.threads.rows) {
		return m, nil
	}
	m.threads.thread = &threadView{row: m.threads.rows[m.selected], back: m.selected}
	m.selected, m.scroll, m.scrollManual = 0, 0, false
	return m.loadThread(false)
}

// loadThread reads the open thread, or its next messages when more.
func (m Model) loadThread(more bool) (Model, tea.Cmd) {
	t := *m.threads.thread
	args := map[string]any{"thread_id": t.row.ID}
	if more {
		args["after"] = t.next
	} else {
		t.loaded, t.err = false, ""
	}
	m.threads.thread = &t
	m.status = m.status.kept()
	m.request++
	request, r := m.request, m.threadsRequest("project_thread", args)
	return m, func() tea.Msg {
		result, err := m.cfg.Executor.Execute(m.cfg.Context, r)
		return threadMsg{request: request, id: t.row.ID, more: more, data: mapData(result.Data), err: err}
	}
}

// reloadThreads is r inside the view: the open thread, or the list.
func (m Model) reloadThreads() (Model, tea.Cmd) {
	if m.threads.thread != nil {
		return m.loadThread(false)
	}
	return m.loadThreads(false)
}

func (m Model) applyThreadsMsg(x threadsMsg) Model {
	if x.request != m.request || m.screen != ScreenProjects || !m.threads.open {
		return m
	}
	if x.err != nil {
		// A failed next page keeps the list; a failed read replaces it.
		if x.more {
			m.status = errStatus(x.err)
			return m
		}
		m.threads.loaded, m.threads.err, m.threads.rows = true, displayError(x.err), nil
		return m
	}
	rows := []threadRow{}
	for _, t := range records(x.data["threads"]) {
		row := threadRow{ID: text(t, "thread_id"), Kind: text(t, "kind"), StartedAt: text(t, "started_at"), LastAt: text(t, "last_at"),
			Messages: integer(t, "messages"), More: integer(t, "participants_more")}
		if names, ok := t["participants"].([]any); ok {
			for _, n := range names {
				if s, ok := n.(string); ok {
					row.Participants = append(row.Participants, s)
				}
			}
		}
		rows = append(rows, row)
	}
	if x.more {
		m.threads.rows = append(m.threads.rows, rows...)
	} else {
		m.threads.rows = rows
		m.selected = min(m.selected, max(0, len(rows)-1))
	}
	m.threads.loaded, m.threads.err = true, ""
	m.threads.next = text(x.data, "next")
	m.threads.readableFrom = firstNonempty(text(x.data, "readable_from"), m.threads.readableFrom)
	return m
}

func (m Model) applyThreadMsg(x threadMsg) Model {
	if x.request != m.request || m.screen != ScreenProjects || m.threads.thread == nil || m.threads.thread.row.ID != x.id {
		return m
	}
	t := *m.threads.thread
	if x.err != nil {
		if x.more {
			m.status = errStatus(x.err)
			return m
		}
		t.loaded, t.err, t.messages = true, displayError(x.err), nil
		m.threads.thread = &t
		return m
	}
	messages := []threadMessage{}
	for _, r := range records(x.data["messages"]) {
		message := threadMessage{Sequence: integer(r, "sequence"), ID: text(r, "id"), Sender: text(r, "sender"), Address: text(r, "address"),
			ReplyTo: text(r, "reply_to"), Kind: text(r, "kind"), Body: text(r, "body"), Coordination: text(r, "coordination"), Sent: text(r, "created_at")}
		for _, d := range records(r["recipients"]) {
			message.Recipients = append(message.Recipients, threadRecipient{Name: text(d, "name"), DeliveredAt: text(d, "delivered_at"), ReadAt: text(d, "read_at")})
		}
		messages = append(messages, message)
	}
	if x.more {
		t.messages = append(t.messages, messages...)
	} else {
		t.messages = messages
	}
	t.loaded, t.err, t.next = true, "", integer(x.data, "next")
	m.threads.thread = &t
	m.threads.readableFrom = firstNonempty(text(x.data, "readable_from"), m.threads.readableFrom)
	return m
}

// threadRowIDs are the list's rows; an open thread has none.
func (m Model) threadRowIDs() []string {
	if m.threads.thread != nil {
		return nil
	}
	out := make([]string, len(m.threads.rows))
	for i, t := range m.threads.rows {
		out[i] = t.ID
	}
	return out
}

// threadsBack is Esc inside the view: thread to list, list to project.
func (m Model) threadsBack() Model {
	m.scroll, m.scrollManual = 0, false
	if t := m.threads.thread; t != nil {
		m.threads.thread, m.selected = nil, t.back
		return m
	}
	m.selected, m.threads = m.threads.back, threadsState{}
	return m
}

func (m Model) threadsBackTarget() string {
	if m.threads.thread != nil {
		return "back to the threads"
	}
	return "back to the project"
}

// threadsMore reports whether ] has another page to read.
func threadsMore(m Model) bool {
	if !m.threads.open {
		return false
	}
	if t := m.threads.thread; t != nil {
		return t.loaded && t.err == "" && t.next > 0
	}
	return m.threads.loaded && m.threads.err == "" && m.threads.next != ""
}

// renderThreads is the list, or the open thread.
func renderThreads(m Model) string {
	var b strings.Builder
	width := tableBudget(m)
	p := m.threads.project
	b.WriteString(accentStyle.Render(terminalText(p.Slug)+" · threads") + "\n")
	b.WriteString(mutedStyle.Render(ansi.Wrap("Read-only: what the project's sessions sent each other. Only you, its owner, can read them here.", width, "")) + "\n")
	if from := m.threads.readableFrom; from != "" {
		b.WriteString(mutedStyle.Render(ansi.Wrap("Readable from "+terminalText(clirender.WhenExact(m.now(), from))+"; older messages aren't shown.", width, "")) + "\n")
	}
	b.WriteString("\n")
	if m.threads.thread != nil {
		return b.String() + renderThread(m, *m.threads.thread, width)
	}
	switch {
	case !m.threads.loaded:
		return b.String() + loadingBody("the threads")
	case m.threads.err != "":
		return b.String() + errorBody(m, "the threads", m.threads.err)
	case len(m.threads.rows) == 0:
		return b.String() + "No threads to read yet."
	}
	rows := make([][]string, len(m.threads.rows))
	for i, t := range m.threads.rows {
		who := make([]string, len(t.Participants))
		for j, n := range t.Participants {
			who[j] = terminalText(n)
		}
		people := firstNonempty(strings.Join(who, ", "), "-")
		if t.More > 0 {
			people += fmt.Sprintf(" +%d", t.More)
		}
		rows[i] = []string{terminalText(clirender.When(m.now(), t.LastAt)), firstNonempty(terminalText(t.Kind), "message"), fmt.Sprint(t.Messages), people}
	}
	b.WriteString(renderTable(m, []string{"LAST", "KIND", "MESSAGES", "BETWEEN"}, []int{14, 14, 8, 40}, rows))
	if m.threads.next != "" {
		b.WriteString("\n\n" + mutedStyle.Render("Older threads: ]"))
	}
	return strings.TrimRight(b.String(), "\n")
}

// renderThread is one thread: each message, who it went to and whether
// they read it, in the order sent.
func renderThread(m Model, t threadView, width int) string {
	var b strings.Builder
	b.WriteString(fmt.Sprintf("Thread %s · %s\n\n", terminalText(t.row.ID), firstNonempty(terminalText(t.row.Kind), "message")))
	switch {
	case !t.loaded:
		return b.String() + loadingBody("the thread")
	case t.err != "":
		return b.String() + errorBody(m, "the thread", t.err)
	case len(t.messages) == 0:
		return b.String() + "No messages in this thread can be read."
	}
	senders := map[string]string{}
	for _, message := range t.messages {
		senders[message.ID] = message.Sender
	}
	for _, message := range t.messages {
		b.WriteString(accentStyle.Render(firstNonempty(terminalText(message.Sender), "unknown sender")))
		b.WriteString(" → " + firstNonempty(terminalText(message.Address), "-"))
		b.WriteString(" · " + firstNonempty(terminalText(message.Kind), "message"))
		b.WriteString(" · " + terminalText(clirender.When(m.now(), message.Sent)) + "\n")
		if message.ReplyTo != "" {
			b.WriteString(mutedStyle.Render("  reply to "+firstNonempty(terminalText(senders[message.ReplyTo]), "an earlier message")) + "\n")
		}
		for _, line := range strings.Split(ansi.Wrap(terminalText(message.Body), width-2, ""), "\n") {
			b.WriteString("  " + line + "\n")
		}
		if message.Coordination != "" {
			b.WriteString(mutedStyle.Render("  coordination: "+clip(terminalText(message.Coordination), width-16)) + "\n")
		}
		if len(message.Recipients) > 0 {
			read := make([]string, len(message.Recipients))
			for i, r := range message.Recipients {
				read[i] = terminalText(r.Name) + " " + recipientState(r)
			}
			b.WriteString(mutedStyle.Render(ansi.Wrap("  "+strings.Join(read, " · "), width, "")) + "\n")
		}
		b.WriteString("\n")
	}
	if t.next > 0 {
		b.WriteString(mutedStyle.Render("More messages: ]"))
	}
	return strings.TrimRight(b.String(), "\n")
}

func recipientState(r threadRecipient) string {
	switch {
	case r.ReadAt != "":
		return "read"
	case r.DeliveredAt != "":
		return "delivered"
	}
	return "not delivered"
}

var threadBindings = []Binding{
	// U62: an owner reads the project's threads.
	{ID: "its-threads", Keys: []string{"t"}, Label: "its threads", When: canReadThreads, Action: func(m Model, _ string) (Model, tea.Cmd) { return m.openThreads() }},
	{ID: "threads-more", Keys: []string{"]"}, Labeler: func(m Model) string {
		if m.threads.thread != nil {
			return "more messages"
		}
		return "older threads"
	}, When: threadsMore, Action: func(m Model, _ string) (Model, tea.Cmd) {
		if m.threads.thread != nil {
			return m.loadThread(true)
		}
		return m.loadThreads(true)
	}},
}
