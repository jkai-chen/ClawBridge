package tui

import (
	"context"
	"sync"

	"clawbridge/internal/remote"
	tea "github.com/charmbracelet/bubbletea"
)

// The Inbox stays live from the server's observe stream (MESSAGE-QUEUE-DESIGN
// §10, slice 3): a status event, which says only that the session's counts
// changed, reloads the Inbox when it is on screen and nothing is being
// typed. A server without the stream, or local mode, keeps r refresh.

// streamer is a hosted executor that can follow a session's stream.
type streamer interface {
	Stream(context.Context, remote.StreamScope, remote.StreamOptions, func(remote.StreamEvent) error) error
}

// InboxChangedMsg says the session's queue changed on the server.
type InboxChangedMsg struct{}

// inboxLive is the observe stream's wakes, made with the Model when its
// executor can stream; start begins following the stream, once.
type inboxLive struct {
	wake  chan struct{}
	start sync.Once
}

func newInboxLive(cfg Config) *inboxLive {
	if _, ok := cfg.Executor.(streamer); !ok || cfg.Session == "" || cfg.Instance == "" {
		return nil
	}
	return &inboxLive{wake: make(chan struct{}, 1)}
}

// inboxLiveCmd starts following the stream, the first time, and waits for
// its first change; nil without a stream.
func (m Model) inboxLiveCmd() tea.Cmd {
	live := m.inboxLive
	if live == nil {
		return nil
	}
	s := m.cfg.Executor.(streamer)
	ctx := m.cfg.Context
	if ctx == nil {
		ctx = context.Background()
	}
	scope := remote.StreamScope{Workspace: m.cfg.Workspace, Project: m.cfg.Project, Session: m.cfg.Session, Instance: m.cfg.Instance, Mode: "observe"}
	live.start.Do(func() {
		go func() {
			// The stream ends with the program's context, or for good (an
			// older server, a revoke); r refresh still works either way.
			_ = s.Stream(ctx, scope, remote.StreamOptions{}, func(e remote.StreamEvent) error {
				if e.Name == "status" {
					select {
					case live.wake <- struct{}{}:
					default:
					}
				}
				return nil
			})
		}()
	})
	return waitInboxChange(live.wake)
}

func waitInboxChange(wake <-chan struct{}) tea.Cmd {
	return func() tea.Msg {
		<-wake
		return InboxChangedMsg{}
	}
}

// inboxChanged reloads a shown, idle Inbox and waits for the next change.
func (m Model) inboxChanged() (Model, tea.Cmd) {
	next := waitInboxChange(m.inboxLive.wake)
	if m.screen != ScreenInbox || m.loading || m.inputMode != inputNone || m.confirming {
		return m, next
	}
	m, cmd := m.refresh()
	return m, tea.Batch(cmd, next)
}
