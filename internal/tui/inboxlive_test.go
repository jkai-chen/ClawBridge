package tui

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"clawbridge/internal/remote"
	tea "github.com/charmbracelet/bubbletea"
)

// streamingAuthority is the fake authority with an observe stream: each
// value on events is one status event.
type streamingAuthority struct {
	*fakeAuthority
	events chan struct{}
	scopes chan remote.StreamScope
}

func (s streamingAuthority) Stream(ctx context.Context, scope remote.StreamScope, _ remote.StreamOptions, handle func(remote.StreamEvent) error) error {
	s.scopes <- scope
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-s.events:
			handle(remote.StreamEvent{Name: "status", Data: json.RawMessage(`{"unacked":1}`)})
		}
	}
}

func (f *fakeAuthority) count(operation string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, c := range f.calls {
		if c.request.Operation == operation {
			n++
		}
	}
	return n
}

// A status event on the observe stream reloads the Inbox while it is
// shown and idle; not while a reply is typed, and not on another screen.
func TestInboxLiveFromTheStream(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	authority := streamingAuthority{fakeAuthority: &fakeAuthority{messages: []map[string]any{}}, events: make(chan struct{}), scopes: make(chan remote.StreamScope, 1)}
	cfg := testConfig(authority)
	cfg.Context = ctx
	model, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	model, cmd := model.Show(ScreenInbox)
	model, _ = model.UpdateModel(runCommand(t, cmd))
	live := model.inboxLiveCmd()
	if scope := <-authority.scopes; scope.Mode != "observe" || scope.Session != "engineer" || scope.Instance != "inst_one" {
		t.Fatal(scope)
	}
	before := authority.count("inbox")
	got := make(chan tea.Msg, 1)
	go func() { got <- live() }()
	authority.events <- struct{}{}
	var msg tea.Msg
	select {
	case msg = <-got:
	case <-time.After(5 * time.Second):
		t.Fatal("no change from the stream")
	}
	model, cmd = model.UpdateModel(msg)
	batch, ok := cmd().(tea.BatchMsg)
	if !ok || len(batch) != 2 {
		t.Fatal("no reload and next wait")
	}
	model, _ = model.UpdateModel(batch[0]())
	if authority.count("inbox") != before+1 {
		t.Fatal("the Inbox didn't reload")
	}
	// Typing a reply: the change waits, nothing reloads.
	model.inputMode = inputReply
	if next, cmd := model.UpdateModel(InboxChangedMsg{}); next.request != model.request || next.loading || cmd == nil {
		t.Fatal("reloaded under a reply being typed")
	}
	model.inputMode = inputNone
	model, cmd = model.Show(ScreenSessions)
	model, _ = model.UpdateModel(runCommand(t, cmd))
	if next, cmd := model.UpdateModel(InboxChangedMsg{}); next.request != model.request || next.loading || cmd == nil {
		t.Fatal("reloaded on another screen")
	}
}

// Without a stream (local mode, a fake without one) nothing follows.
func TestInboxLiveOffWithoutAStream(t *testing.T) {
	model, err := New(testConfig(&fakeAuthority{}))
	if err != nil {
		t.Fatal(err)
	}
	if model.inboxLive != nil || model.inboxLiveCmd() != nil {
		t.Fatal("followed a stream that isn't there")
	}
	if next, cmd := model.UpdateModel(InboxChangedMsg{}); cmd != nil || next.loading {
		t.Fatal("acted on a change without a stream")
	}
}

// endedAuthority's stream ends at once, as an older server's does.
type endedAuthority struct{ *fakeAuthority }

func (endedAuthority) Stream(context.Context, remote.StreamScope, remote.StreamOptions, func(remote.StreamEvent) error) error {
	return remote.ErrStreamUnsupported
}

// A stream that ends for good says so once; the Inbox reloads on r from
// then on.
func TestInboxLiveEndedSaysSo(t *testing.T) {
	model, err := New(testConfig(endedAuthority{&fakeAuthority{}}))
	if err != nil {
		t.Fatal(err)
	}
	msg := model.inboxLiveCmd()()
	if _, ok := msg.(InboxLiveEndedMsg); !ok {
		t.Fatalf("%T", msg)
	}
	next, cmd := model.UpdateModel(msg)
	if next.status != noteStatus(liveOffNote) || cmd != nil {
		t.Fatal(next.status, cmd != nil)
	}
}
