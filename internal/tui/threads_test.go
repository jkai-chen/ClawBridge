package tui

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"clawbridge/internal/bus"

	"github.com/charmbracelet/lipgloss"
)

const (
	threadOne = "msg_00000000000000000000000000000001"
	threadTwo = "msg_00000000000000000000000000000002"
	threadOld = "msg_00000000000000000000000000000009"
)

// threadsAuthority is the projects authority with core's threads: two on
// the first page and one older, and one thread's messages in two pages.
type threadsAuthority struct {
	*fakeAuthority
	threadErr error
}

func (a *threadsAuthority) Execute(ctx context.Context, r bus.Request) (bus.Result, error) {
	switch r.Operation {
	case "project_threads":
		a.fakeAuthority.Execute(ctx, r)
		if r.Args["before"] == "cur-1" {
			return bus.Result{Source: "server", Data: map[string]any{"threads": []any{
				map[string]any{"thread_id": threadOld, "kind": "handoff", "last_at": "2026-10-20T10:00:00Z", "messages": float64(1), "participants": []any{"old"}},
			}, "next": nil, "readable_from": "2026-10-01T00:00:00Z"}}, nil
		}
		return bus.Result{Source: "server", Data: map[string]any{"threads": []any{
			map[string]any{"thread_id": threadOne, "kind": "question", "last_at": "2026-10-27T09:58:00Z", "messages": float64(3),
				"participants": []any{"coordinator", "engineer \x1b[2J"}, "participants_more": float64(2)},
			map[string]any{"thread_id": threadTwo, "kind": "message", "last_at": "2026-10-27T08:00:00Z", "messages": float64(1), "participants": []any{"tester"}},
		}, "next": "cur-1", "readable_from": "2026-10-01T00:00:00Z"}}, nil
	case "project_thread":
		a.fakeAuthority.Execute(ctx, r)
		if a.threadErr != nil {
			return bus.Result{}, a.threadErr
		}
		if r.Args["after"] == int64(2) {
			return bus.Result{Source: "server", Data: map[string]any{"thread_id": threadOne, "messages": []any{
				map[string]any{"sequence": float64(3), "id": "msg_c", "sender": "coordinator", "address": "engineer", "reply_to": "msg_b", "kind": "reply", "body": "Merged.", "created_at": "2026-10-27T09:58:00Z"},
			}, "next": nil}}, nil
		}
		return bus.Result{Source: "server", Data: map[string]any{"thread_id": threadOne, "messages": []any{
			map[string]any{"sequence": float64(1), "id": threadOne, "sender": "coordinator", "address": "engineer", "kind": "question",
				"body": "Is \x1b[31mSYN-5\x1b[0m ready?", "coordination": "handoff-1", "created_at": "2026-10-27T09:00:00Z",
				"recipients": []any{map[string]any{"name": "engineer", "delivered_at": "2026-10-27T09:00:01Z", "read_at": "2026-10-27T09:01:00Z"}}},
			map[string]any{"sequence": float64(2), "id": "msg_b", "sender": "engineer", "address": "coordinator", "reply_to": threadOne, "kind": "reply", "body": "Yes.", "created_at": "2026-10-27T09:30:00Z",
				"recipients": []any{map[string]any{"name": "coordinator", "delivered_at": "2026-10-27T09:30:01Z"}, map[string]any{"name": "tester"}}},
		}, "next": float64(2), "readable_from": "2026-10-01T00:00:00Z"}}, nil
	}
	return a.fakeAuthority.Execute(ctx, r)
}

// threadsView is Projects for a person: the test profile's session off.
func threadsView(t *testing.T, a *threadsAuthority) Model {
	t.Helper()
	m := projectsView(t, a.fakeAuthority)
	m.cfg.Executor, m.cfg.Session = a, ""
	return m
}

func (a *threadsAuthority) threadCalls() []bus.Request {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := []bus.Request{}
	for _, c := range a.calls {
		if strings.HasPrefix(c.request.Operation, "project_thread") {
			out = append(out, c.request)
		}
	}
	return out
}

// U62: the owner opens core, reads its threads and one thread, as the
// person; older pages append; Esc walks back to the project.
func TestThreadsOwnerReadsTheProjectsThreads(t *testing.T) {
	a := &threadsAuthority{fakeAuthority: overviewAuthority()}
	// core, the one owned, second on the list.
	a.results["list_projects"] = bus.Result{Source: "server", Data: map[string]any{"projects": []any{
		map[string]any{"id": "prj_2", "slug": "web", "name": "Web", "role": "reader"},
		map[string]any{"id": "prj_1", "slug": "core", "name": "Core", "role": "owner"},
	}}}
	m := threadsView(t, a)
	m = do(m, "down", "enter")
	if !strings.Contains(footerText(m), "t its threads") {
		t.Fatal("no t on an owned project:", footerText(m))
	}
	m = do(m, "t")
	view := Render(m)
	for _, want := range []string{"core · threads", "Read-only", "Readable from", "LAST", "question", "3", `coordinator, engineer \x1b[2J +2`, "tester", "Older threads: ]"} {
		if !strings.Contains(view, want) {
			t.Fatalf("no %q in\n%s", want, view)
		}
	}
	if !strings.Contains(footerText(m), "back to the project") || strings.Contains(footerText(m), "its sessions") || strings.Contains(footerText(m), "its threads") {
		t.Fatal(footerText(m))
	}
	calls := a.threadCalls()
	if len(calls) != 1 || calls[0].Operation != "project_threads" || calls[0].Project != "core" || calls[0].Session != "" || len(calls[0].Args) != 0 {
		t.Fatal("not read as the person:", calls)
	}
	// ] appends the older page.
	m = do(m, "]")
	if calls = a.threadCalls(); calls[1].Args["before"] != "cur-1" || len(m.threads.rows) != 3 || !strings.Contains(Render(m), "handoff") || strings.Contains(Render(m), "Older threads") {
		t.Fatal("no older page:", calls, Render(m))
	}
	if strings.Contains(footerText(m), "older threads") {
		t.Fatal("] offered with no next page")
	}
	// Enter opens the selected thread.
	m = do(m, "enter")
	view = Render(m)
	for _, want := range []string{"Thread " + threadOne, "coordinator → engineer · question", `Is \x1b[31mSYN-5\x1b[0m ready?`, "coordination: handoff-1", "engineer read",
		"reply to coordinator", "coordinator delivered · tester not delivered", "More messages: ]"} {
		if !strings.Contains(view, want) {
			t.Fatalf("no %q in\n%s", want, view)
		}
	}
	if c := a.threadCalls()[2]; c.Operation != "project_thread" || c.Args["thread_id"] != threadOne || c.Session != "" || c.Args["instance"] != nil {
		t.Fatal(c)
	}
	m = do(m, "]")
	if c := a.threadCalls()[3]; c.Args["after"] != int64(2) || !strings.Contains(Render(m), "Merged.") || !strings.Contains(Render(m), "Yes.") {
		t.Fatal("more messages:", c, Render(m))
	}
	// r reads the open thread again, from the start.
	m = do(m, "r")
	if c := a.threadCalls()[4]; c.Operation != "project_thread" || c.Args["after"] != nil || strings.Contains(Render(m), "Merged.") {
		t.Fatal("r:", c)
	}
	// Esc: thread → list (selection kept) → project → projects.
	m.threads.thread.back = 1
	m = do(m, "esc")
	if m.threads.thread != nil || !m.threads.open || m.selected != 1 || !strings.Contains(footerText(m), "back to the project") {
		t.Fatal("esc from the thread:", m.selected)
	}
	m = do(m, "r")
	if c := a.threadCalls(); c[len(c)-1].Operation != "project_threads" || len(m.threads.rows) != 2 {
		t.Fatal("r on the list:", c)
	}
	m = do(m, "esc")
	if m.threads.open || !m.projectOpen || m.selected != 1 || !strings.Contains(Render(m), "Your profile works in this project.") {
		t.Fatal("esc from the list:", Render(m))
	}
	if m = do(m, "esc"); m.projectOpen {
		t.Fatal("esc from the project")
	}
}

// Only an owner gets t; another role's project, or the list, has none.
func TestThreadsOnlyForAnOwner(t *testing.T) {
	a := &threadsAuthority{fakeAuthority: overviewAuthority()}
	m := threadsView(t, a)
	if strings.Contains(footerText(m), "its threads") {
		t.Fatal("t on the list")
	}
	m = do(m, "down", "enter")
	if strings.Contains(footerText(m), "its threads") {
		t.Fatal("t for a reader:", footerText(m))
	}
	if m = do(m, "t"); m.threads.open || len(a.threadCalls()) != 0 {
		t.Fatal("a reader read the threads")
	}
}

// A refused read says so in the view; leaving another screen closes it.
func TestThreadsRefusedRead(t *testing.T) {
	a := &threadsAuthority{fakeAuthority: overviewAuthority(), threadErr: errors.New("forbidden: the project's threads are read only from a person's own signed-in device")}
	threadErr := a.threadErr
	a.threadErr = nil
	m := threadsView(t, a)
	m = do(m, "enter", "t", "enter")
	if !strings.Contains(footerText(m), "more messages") {
		t.Fatal("no ] on a thread with more")
	}
	// A reread that fails offers no next page.
	a.threadErr = threadErr
	m = do(m, "r")
	if view := Render(m); !strings.Contains(view, "Couldn't load the thread") || !strings.Contains(view, "signed-in device") {
		t.Fatal(view)
	}
	if strings.Contains(footerText(m), "more messages") {
		t.Fatal("] after a failed read")
	}
	m, _ = m.Show(ScreenSessions)
	if m.threads.open {
		t.Fatal("threads stayed open on another screen")
	}
}

// Like the CLI, a TUI opened with a session, or one an agent runs, reads
// no threads and says why.
func TestThreadsRefusedWithASessionOrAnAgent(t *testing.T) {
	a := &threadsAuthority{fakeAuthority: overviewAuthority()}
	m := threadsView(t, a)
	m = do(m, "enter")
	session := m
	session.cfg.Session = "sess-1"
	if session = do(session, "t"); session.threads.open || !strings.Contains(session.status.text, "open the TUI without --session") {
		t.Fatal("a session read the threads:", session.status.text)
	}
	agent := m
	agent.cfg.AgentMarker = "CLAUDECODE"
	if agent = do(agent, "t"); agent.threads.open || !strings.Contains(agent.status.text, "CLAUDECODE says an agent runs this TUI") {
		t.Fatal("an agent read the threads:", agent.status.text)
	}
	if len(a.threadCalls()) != 0 {
		t.Fatal(a.threadCalls())
	}
}

// A thread opens at its top, even after PgDn on the list, and Esc returns
// to the list's top.
func TestThreadsOpenAtTheTop(t *testing.T) {
	a := &threadsAuthority{fakeAuthority: overviewAuthority()}
	m := threadsView(t, a)
	m = do(m, "enter", "t")
	m.scroll, m.scrollManual = 5, true
	if m = do(m, "enter"); m.scroll != 0 || m.scrollManual {
		t.Fatal("the thread opened part-way down")
	}
	m.scroll, m.scrollManual = 5, true
	if m = do(m, "esc"); m.scroll != 0 || m.scrollManual {
		t.Fatal("the list came back part-way down")
	}
}

// Many participants, long names, at 80×24: the list stays in the frame.
func TestThreadsManyParticipantsFit(t *testing.T) {
	a := &threadsAuthority{fakeAuthority: overviewAuthority()}
	m := threadsView(t, a)
	m = do(m, "enter", "t").SetSize(80, 24)
	names := []string{}
	for i := 0; i < 20; i++ {
		names = append(names, fmt.Sprintf("a-rather-long-session-name-%02d", i))
	}
	m.threads.rows = []threadRow{{ID: threadOne, Kind: "question", LastAt: "2026-10-27T09:58:00Z", Messages: 120, Participants: names, More: 7}}
	view := Render(m)
	for _, line := range strings.Split(view, "\n") {
		if lipgloss.Width(line) > 80 {
			t.Fatalf("a line past 80 columns: %q", line)
		}
	}
	if !strings.Contains(view, "a-rather-long-session-name-00") {
		t.Fatal(view)
	}
}
