package tui

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"clawbridge/internal/bus"
	tea "github.com/charmbracelet/bubbletea"
)

// A hosted project's Linear budget and queued writes on the Linear screen
// (LINEAR-QUEUE-DESIGN §8): a header line, the queue (o) and, for the
// owner, the budget (b). Counts, times and IDs only, never a write's text.

// Queue-age thresholds, as doctor's linear.outbox check.
const (
	queueWarnAge = 15 * time.Minute
	queueFailAge = 60 * time.Minute
)

// linearQueue is the project's queue and hourly budget from
// linear_app_status.
type linearQueue struct {
	used, limit int
	// people and agents are the hour's calls for writes through a
	// personal profile (FR8); the rest are the app's.
	people, agents                 int
	source                         string
	resetsAt                       time.Time
	queued, sending, dead, expired int
	uncertain                      int
	oldest                         time.Duration
	waiting                        string
}

// readLinearQueue reads the queue and budget as the person; nil when the
// server has no queue or it can't be read.
func readLinearQueue(ctx context.Context, executor Executor, workspace, project string) *linearQueue {
	if executor == nil || project == "" {
		return nil
	}
	r, err := executor.Execute(ctx, bus.Request{Operation: "linear_app_status", Workspace: workspace, Project: project, Args: map[string]any{}})
	if err != nil {
		return nil
	}
	return parseLinearQueue(mapData(r.Data))
}

func parseLinearQueue(data map[string]any) *linearQueue {
	outbox, budget := mapData(data["outbox"]), mapData(data["budget"])
	if outbox == nil || budget == nil {
		return nil
	}
	q := &linearQueue{
		used: asCount(budget["used"]), limit: asCount(budget["limit"]), source: text(budget, "source"),
		queued: asCount(outbox["queued"]), sending: asCount(outbox["sending"]), dead: asCount(outbox["dead_24h"]),
		expired: asCount(outbox["expired_24h"]), uncertain: asCount(outbox["uncertain_24h"]),
		oldest: time.Duration(asCount(outbox["oldest_queued_age_s"])) * time.Second, waiting: text(outbox, "waiting"),
	}
	q.resetsAt, _ = time.Parse(time.RFC3339, text(budget, "window_resets_at"))
	if by := mapData(budget["by"]); by != nil {
		q.people, q.agents = asCount(by["people"]), asCount(by["agents"])
	}
	return q
}

// linearQueueLine is the header: "Linear: 143/600 this hour · 3 queued
// (oldest 6 min)", coloured by doctor's thresholds.
func linearQueueLine(q *linearQueue) string {
	line := fmt.Sprintf("Linear: %d/%d this hour", q.used, q.limit)
	if q.limit <= 0 {
		line = fmt.Sprintf("Linear: %d this hour (no cap)", q.used)
	}
	if q.people+q.agents > 0 {
		line += fmt.Sprintf(" (people %d, agents %d)", q.people, q.agents)
	}
	if n := q.queued + q.sending; n > 0 {
		line += fmt.Sprintf(" · %d queued", n)
		if q.queued > 0 {
			line += fmt.Sprintf(" (oldest %d min)", int(q.oldest/time.Minute))
		}
	}
	if lost := q.dead + q.expired; lost > 0 {
		line += fmt.Sprintf(" · %d given up today", lost)
	}
	if q.uncertain > 0 {
		line += fmt.Sprintf(" · %d uncertain", q.uncertain)
	}
	line += " (o: queue)"
	switch {
	case q.oldest >= queueFailAge || q.dead+q.expired > 0:
		return errorStyle.Render(line)
	case q.oldest >= queueWarnAge || q.uncertain > 0 || q.limit > 0 && q.used >= q.limit:
		return warnStyle.Render(line)
	}
	return mutedStyle.Render(line)
}

type linearWritesMsg struct {
	request uint64
	writes  []map[string]any
	queue   *linearQueue
	err     error
}

type linearBudgetMsg struct {
	request uint64
	err     error
}

// openLinearWrites shows the project's queued and recent writes.
func (m Model) openLinearWrites() (Model, tea.Cmd) {
	m.linear.step, m.selected = "writes", 0
	requestID := m.request
	if m.cfg.Executor == nil || m.cfg.Project == "" {
		return m, func() tea.Msg {
			return linearWritesMsg{request: requestID, err: errors.New("the write queue is per project; open the TUI with a project")}
		}
	}
	m.loading = true
	executor, ctx, workspace, project := m.cfg.Executor, m.cfg.Context, m.cfg.Workspace, m.cfg.Project
	return m, func() tea.Msg {
		r, err := executor.Execute(ctx, bus.Request{Operation: "linear_write_status", Workspace: workspace, Project: project, Args: map[string]any{"scope": "project"}})
		if err != nil {
			return linearWritesMsg{request: requestID, err: err}
		}
		return linearWritesMsg{request: requestID, writes: records(mapData(r.Data)["writes"]), queue: readLinearQueue(ctx, executor, workspace, project)}
	}
}

func (m Model) applyLinearWrites(x linearWritesMsg) Model {
	if x.request != m.request || m.screen != ScreenLinear || m.linear.step != "writes" {
		return m
	}
	m.loading = false
	if x.err != nil {
		m.status = errStatus(x.err)
		return m
	}
	m.linear.writes = x.writes
	if x.queue != nil {
		m.linear.queue = x.queue
	}
	if m.selected >= len(x.writes) {
		m.selected = max(0, len(x.writes)-1)
	}
	return m
}

// renderLinearWrites lists the project's queue: operation, issue, state
// and why, age, and when it is next tried. Typed fields only.
func renderLinearWrites(m Model) string {
	var b strings.Builder
	b.WriteString("Queued Linear writes (this project; queued first, then the last 24 hours)\n\n")
	if len(m.linear.writes) == 0 {
		b.WriteString("No writes are queued, and none ended in the last 24 hours.\n")
		return b.String()
	}
	now := m.now()
	for i, w := range m.linear.writes {
		prefix := "  "
		if i == m.selected {
			prefix = "› "
		}
		line := text(w, "operation") + " · " + firstNonempty(text(w, "issue"), "new issue") + " · " + text(w, "state")
		if reason := text(w, "reason"); reason != "" {
			line += " (" + reason + ")"
		}
		if at, err := time.Parse(time.RFC3339, text(w, "queued_at")); err == nil {
			line += fmt.Sprintf(" · queued %d min ago", int(now.Sub(at)/time.Minute))
		}
		if next, err := time.Parse(time.RFC3339, text(w, "next_attempt_at")); err == nil {
			line += " · next try " + next.Local().Format("15:04")
		}
		if result := text(w, "result"); result != "" {
			line += " · " + result
		}
		b.WriteString(prefix + terminalText(line) + "\n")
	}
	return b.String()
}

// canSetLinearBudget: the project owner sets its budget, on a server that
// shows it.
func (m Model) canSetLinearBudget() bool {
	return m.linear.queue != nil && m.linear.role == "owner"
}

// beginLinearBudget asks for the project's new hourly budget (b).
func (m Model) beginLinearBudget() Model {
	m.inputMode, m.draft, m.status = inputLinearBudget, "", status{}
	return m
}

// linearBudgetTyped takes the typed budget and asks y before setting it.
// Empty clears the project's own budget, back to the server's.
func (m Model) linearBudgetTyped() (Model, tea.Cmd) {
	draft := strings.TrimSpace(m.draft)
	m.inputMode, m.draft = inputNone, ""
	if draft == "" {
		m.linear.budgetAsk = -1
		m.status = noteStatus("Clear this project's own Linear budget, back to the server's? y clears; any other key keeps it.")
		return m, nil
	}
	n, err := strconv.Atoi(draft)
	if err != nil || n < 1 {
		m.status = noteStatus("the budget is a whole number of Linear calls an hour, 1 or more")
		return m, nil
	}
	m.linear.budgetAsk = n
	m.status = noteStatus(fmt.Sprintf("Set this project's Linear budget to %d calls an hour? Agent writes, the comment watch, health and status reads all count. y sets it; any other key keeps it.", n))
	return m, nil
}

// linearBudgetAnswer is the key pressed after a typed budget.
func (m Model) linearBudgetAnswer(key string) (Model, tea.Cmd) {
	n := m.linear.budgetAsk
	m.linear.budgetAsk, m.status = 0, status{}
	if key != "y" {
		return m, nil
	}
	var budget any = n
	if n < 0 {
		budget = nil
	}
	m.request++
	requestID := m.request
	m.loading = true
	executor, ctx, workspace, project := m.cfg.Executor, m.cfg.Context, m.cfg.Workspace, m.cfg.Project
	return m, func() tea.Msg {
		_, err := executor.Execute(ctx, bus.Request{Operation: "comment_watch_budget_set", Workspace: workspace, Project: project, Args: map[string]any{"watch_budget": budget}})
		return linearBudgetMsg{request: requestID, err: err}
	}
}

func (m Model) applyLinearBudget(x linearBudgetMsg) (Model, tea.Cmd) {
	if x.request != m.request {
		return m, nil
	}
	m.loading = false
	if x.err != nil {
		m.status = errStatus(x.err)
		return m, nil
	}
	m.status = okStatus("The project's Linear budget is set.")
	return m.linearRefresh()
}
