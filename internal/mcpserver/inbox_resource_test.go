package mcpserver

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"clawbridge/internal/bus"
	"clawbridge/internal/notify"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type inboxExecutor struct {
	mu             sync.Mutex
	calls          int
	unread, latest int64
	instance       string
}

func (e *inboxExecutor) Execute(ctx context.Context, r bus.Request) (bus.Result, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if ctx.Err() != nil {
		return bus.Result{}, ctx.Err()
	}
	if r.Operation == "register_session" {
		if resume, _ := r.Args["resume"].(bool); resume {
			e.instance = "rotated"
		}
		return bus.Result{Source: "local-authority", Data: map[string]any{"instance": e.instance}}, nil
	}
	if r.Operation != "inbox_status" {
		return bus.Result{}, errors.New("only status allowed in resource probe")
	}
	e.calls++
	if r.Args["instance"] != e.instance {
		return bus.Result{}, notify.Permanent(errors.New("fenced"))
	}
	return bus.Result{Source: "local-authority", Data: notify.Status{WorkspaceID: "ws-id", ProjectID: "prj-id", SessionID: "ses-id", UnreadCount: e.unread, LatestSequence: e.latest, NewCount: e.unread}}, nil
}
func (e *inboxExecutor) count() int { e.mu.Lock(); defer e.mu.Unlock(); return e.calls }
func waitNotice(t *testing.T, events <-chan string) {
	t.Helper()
	select {
	case u := <-events:
		if u != InboxStatusURI {
			t.Fatal(u)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("resource notification absent")
	}
}
func tickResource(t *testing.T, waits <-chan chan struct{}) {
	t.Helper()
	select {
	case tick := <-waits:
		close(tick)
	case <-time.After(3 * time.Second):
		t.Fatal("resource poll not waiting")
	}
}
func TestNotificationMCPExplicitSubscriptionLifecycle(t *testing.T) {
	for _, protocol := range []string{"2025-11-25", "2026-07-28"} {
		t.Run(protocol, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			e := &inboxExecutor{unread: 1, latest: 4, instance: "current"}
			waits := make(chan chan struct{}, 8)
			server := NewServerWithOptions(e, "workspace", "project", "recipient", "current", Options{Context: ctx, Wait: func(ctx context.Context, _ time.Duration) error {
				tick := make(chan struct{})
				select {
				case waits <- tick:
				case <-ctx.Done():
					return ctx.Err()
				}
				select {
				case <-tick:
					return nil
				case <-ctx.Done():
					return ctx.Err()
				}
			}})
			st, ct := mcp.NewInMemoryTransports()
			ss, err := server.Connect(ctx, st, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer ss.Close()
			events := make(chan string, 8)
			client := mcp.NewClient(&mcp.Implementation{Name: "notify-test", Version: "1"}, &mcp.ClientOptions{ResourceUpdatedHandler: func(_ context.Context, r *mcp.ResourceUpdatedNotificationRequest) { events <- r.Params.URI }})
			cs, err := client.Connect(ctx, ct, &mcp.ClientSessionOptions{ProtocolVersion: protocol})
			if err != nil {
				t.Fatal(err)
			}
			defer cs.Close()
			if _, err = cs.ListTools(ctx, nil); err != nil {
				t.Fatal(err)
			}
			if e.count() != 0 {
				t.Fatal("ordinary MCP client polled")
			}
			if _, err = cs.ReadResource(ctx, &mcp.ReadResourceParams{URI: "clawbridge://other/status"}); err == nil || e.count() != 0 {
				t.Fatal("invalid URI reached authority", err)
			}
			subscribed := make(chan error, 1)
			go func() { subscribed <- cs.Subscribe(ctx, &mcp.SubscribeParams{URI: InboxStatusURI}) }()
			waitNotice(t, events)
			resource, err := cs.ReadResource(ctx, &mcp.ReadResourceParams{URI: InboxStatusURI})
			if err != nil || len(resource.Contents) != 1 {
				t.Fatal(resource, err)
			}
			// Idempotent registration keeps the exact instance and must preserve
			// the already-established subscription and its metadata baseline.
			registered, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "register_session", Arguments: map[string]any{}})
			if err != nil || registered.IsError {
				t.Fatal("same-instance registration failed", registered, err)
			}
			e.mu.Lock()
			e.unread = 0
			e.latest = 0
			e.mu.Unlock()
			tickResource(t, waits)
			waitNotice(t, events)
			if err = cs.Unsubscribe(ctx, &mcp.UnsubscribeParams{URI: InboxStatusURI}); err != nil {
				t.Fatal(err)
			}
			select {
			case <-subscribed:
			case <-time.After(3 * time.Second):
				t.Fatal("subscription goroutine leaked")
			}
			// Closing the client also must release any pending poll wait immediately.
			if err = cs.Close(); err != nil {
				t.Fatal(err)
			}
			before := e.count()
			select {
			case <-events:
				t.Fatal("unexpected hint after unsubscribe")
			default:
			}
			if e.count() != before {
				t.Fatal("poll ran after client disconnect")
			}
		})
	}
}
