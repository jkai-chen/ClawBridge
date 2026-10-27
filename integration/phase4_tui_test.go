package integration

import (
	"context"
	"strings"
	"testing"
	"time"

	"clawbridge/internal/bus"
	"clawbridge/internal/tui"
	tea "github.com/charmbracelet/bubbletea"
)

func phase4TUI(t *testing.T, executor tui.Executor, actor bool, ctx context.Context) tui.Model {
	t.Helper()
	cfg := tui.Config{Executor: executor, Mode: tui.ModeCloud, Workspace: "workspace", Project: "project", Context: ctx}
	if actor {
		cfg.Session, cfg.Instance = "session", "instance-1"
	}
	m, err := tui.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func TestPhase4TUIDraftsRequireExplicitSubmitAndDoNotRetryFailures(t *testing.T) {
	e := &recordingTUIExecutor{source: "server", failOp: "send"}
	m := phase4TUI(t, e, true, context.Background())
	m, cmd := m.Show(tui.Inbox)
	m = applyTUIMsg(t, m, runTUICmd(t, cmd))
	before := len(e.recorded())
	for _, key := range []tea.KeyMsg{{Type: tea.KeyRunes, Runes: []rune("n")}, {Type: tea.KeyRunes, Runes: []rune("peer")}, {Type: tea.KeyEnter}, {Type: tea.KeyRunes, Runes: []rune("cancel this draft")}, {Type: tea.KeyEsc}} {
		m, cmd = m.UpdateModel(key)
		if cmd != nil {
			t.Fatal("draft edit/cancel issued authority command")
		}
	}
	if len(e.recorded()) != before {
		t.Fatal("canceled draft reached authority")
	}
	for _, key := range []tea.KeyMsg{{Type: tea.KeyRunes, Runes: []rune("n")}, {Type: tea.KeyRunes, Runes: []rune("peer")}, {Type: tea.KeyEnter}, {Type: tea.KeyRunes, Runes: []rune("explicit message")}, {Type: tea.KeyEnter}} {
		m, cmd = m.UpdateModel(key)
	}
	if cmd == nil {
		t.Fatal("explicit submit did not produce command")
	}
	m, next := m.UpdateModel(runTUICmd(t, cmd))
	if next != nil || !strings.Contains(m.State().Error, "forbidden") {
		t.Fatal("failed write retried/refreshed or hid failure")
	}
	requests := e.recorded()
	if len(requests) != before+1 {
		t.Fatal("write failure issued additional commands")
	}
	last := requests[len(requests)-1]
	if last.Operation != "send" || last.Args["body"] != "explicit message" || last.Args["to"] != "peer" || last.Args["operation_id"] == "" {
		t.Fatalf("explicit action changed: %+v", last)
	}
	assertTUIReadsPinned(t, requests, true)
}

func TestPhase4TUIActorlessActionsDoNotReachAuthority(t *testing.T) {
	for _, name := range []string{"send", "reply", "mark-read", "inbox"} {
		t.Run(name, func(t *testing.T) {
			e := &recordingTUIExecutor{source: "server"}
			m := phase4TUI(t, e, false, context.Background())
			var next tui.Model
			var cmd tea.Cmd
			switch name {
			case "send":
				next, cmd = m.Send("peer", "body", "op")
			case "reply":
				next, cmd = m.Reply("msg", "body", "op")
			case "mark-read":
				next, cmd = m.MarkRead([]string{"msg"})
			case "inbox":
				next, cmd = m.Show(tui.Inbox)
			}
			if cmd != nil {
				next = applyTUIMsg(t, next, runTUICmd(t, cmd))
			}
			if len(e.recorded()) != 0 {
				t.Error("actorless action reached executor")
			}
			if name == "inbox" {
				// Without a session the Inbox explains whose inboxes these
				// are; it is not an error (owner, TUI-UX-AUDIT Q1).
				if next.State().Error != "" || !strings.Contains(tui.Render(next), "Inboxes belong to agent sessions") {
					t.Errorf("sessionless inbox not explained: %+v", next.State())
				}
			} else if next.State().Error == "" {
				t.Error("actorless action lacked actionable error")
			}
		})
	}
}

func TestPhase4TUIStaleScreenResultCannotReplaceCurrentView(t *testing.T) {
	e := &recordingTUIExecutor{source: "server"}
	m := phase4TUI(t, e, true, context.Background())
	m, oldCmd := m.Show(tui.Sessions)
	m, newCmd := m.Show(tui.Inbox)
	m = applyTUIMsg(t, m, runTUICmd(t, newCmd))
	before := m.State()
	m = applyTUIMsg(t, m, runTUICmd(t, oldCmd))
	after := m.State()
	if after.Screen != tui.Inbox || len(after.Messages) != len(before.Messages) || len(after.Sessions) != len(before.Sessions) {
		t.Fatal("late result overwrote selected screen")
	}
}

type phase4BlockingTUI struct{ started chan struct{} }

func (e phase4BlockingTUI) Execute(ctx context.Context, _ bus.Request) (bus.Result, error) {
	close(e.started)
	<-ctx.Done()
	return bus.Result{}, ctx.Err()
}

func TestPhase4TUIInFlightReadUsesCanceledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	e := phase4BlockingTUI{started: make(chan struct{})}
	m := phase4TUI(t, e, true, ctx)
	m, cmd := m.Show(tui.Inbox)
	done := make(chan tea.Msg, 1)
	go func() { done <- cmd() }()
	select {
	case <-e.started:
	case <-time.After(time.Second):
		t.Fatal("read did not start")
	}
	cancel()
	select {
	case msg := <-done:
		next, retry := m.UpdateModel(msg)
		if retry != nil || !strings.Contains(next.State().Error, "canceled") {
			t.Fatal("cancellation was hidden or retried")
		}
	case <-time.After(time.Second):
		t.Fatal("in-flight command ignored cancellation")
	}
}
