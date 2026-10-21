package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"runtime/pprof"
	"strings"
	"sync"
	"testing"
	"time"

	"clawbridge/internal/bus"
	"clawbridge/internal/mcpserver"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type notificationMCPExecutor struct {
	mu               sync.Mutex
	requests         []bus.Request
	unread, sequence int64
}

func (e *notificationMCPExecutor) Execute(_ context.Context, r bus.Request) (bus.Result, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.requests = append(e.requests, r)
	if r.Operation == "register_session" {
		return bus.Result{Source: "local-authority", Data: map[string]any{"instance": "rotated-instance"}}, nil
	}
	if r.Operation != "inbox_status" {
		return bus.Result{}, fmt.Errorf("unexpected mutation or heartbeat operation %s", r.Operation)
	}
	return notificationResult(e.unread, e.sequence, e.unread), nil
}
func (e *notificationMCPExecutor) count() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return len(e.requests)
}
func (e *notificationMCPExecutor) set(unread, sequence int64) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.unread = unread
	e.sequence = sequence
}

type notificationMCPWait struct {
	ctx     context.Context
	release chan struct{}
}

func notificationAwaitWait(t *testing.T, ctx context.Context, c <-chan notificationMCPWait) notificationMCPWait {
	t.Helper()
	select {
	case w := <-c:
		return w
	case <-ctx.Done():
		t.Fatal("poll never reached injected wait")
		return notificationMCPWait{}
	}
}
func notificationAwaitNotice(t *testing.T, ctx context.Context, c <-chan string) {
	t.Helper()
	select {
	case uri := <-c:
		if uri != mcpserver.InboxStatusURI {
			t.Fatalf("wrong notification URI %q", uri)
		}
	case <-ctx.Done():
		t.Fatal("subscribed resource update missing")
	}
}
func notificationMCPConnect(t *testing.T, ctx context.Context, server *mcp.Server, notices chan<- string, versions ...string) *mcp.ClientSession {
	t.Helper()
	st, ct := mcp.NewInMemoryTransports()
	ss, err := server.Connect(ctx, st, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ss.Close() })
	client := mcp.NewClient(&mcp.Implementation{Name: "independent-notification-review", Version: "1"}, &mcp.ClientOptions{ResourceUpdatedHandler: func(_ context.Context, r *mcp.ResourceUpdatedNotificationRequest) { notices <- r.Params.URI }})
	version := "2025-11-25"
	if len(versions) > 0 {
		version = versions[0]
	}
	cs, err := client.Connect(ctx, ct, &mcp.ClientSessionOptions{ProtocolVersion: version})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cs.Close() })
	return cs
}
func TestNotificationMCPSubscriptionLifecycle(t *testing.T) {
	for _, version := range []string{"2025-11-25", "2026-07-28"} {
		t.Run(version, func(t *testing.T) { notificationMCPSubscriptionLifecycle(t, version) })
	}
}
func notificationMCPSubscriptionLifecycle(t *testing.T, version string) {
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	executor := &notificationMCPExecutor{unread: 2, sequence: 9}
	waits := make(chan notificationMCPWait, 20)
	notices := make(chan string, 20)
	server := mcpserver.NewServerWithOptions(executor, "workspace-alias", "project-alias", "recipient-alias", "private-instance", mcpserver.Options{Context: ctx, PollInterval: time.Second, Wait: func(ctx context.Context, _ time.Duration) error {
		w := notificationMCPWait{ctx: ctx, release: make(chan struct{})}
		select {
		case waits <- w:
		case <-ctx.Done():
			return ctx.Err()
		}
		select {
		case <-w.release:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}})
	cs := notificationMCPConnect(t, ctx, server, notices, version)
	resources, err := cs.ListResources(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(resources.Resources) != 1 || resources.Resources[0].URI != mcpserver.InboxStatusURI {
		t.Fatal("resource discovery mismatch")
	}
	tools, err := cs.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, tool := range tools.Tools {
		if tool.Name == "inbox_status" {
			found = true
			if tool.Annotations == nil || !tool.Annotations.ReadOnlyHint || !tool.Annotations.IdempotentHint {
				t.Error("metadata tool lacks read-only/idempotent annotations")
			}
		}
	}
	if !found {
		t.Fatal("inbox_status tool missing")
	}
	if executor.count() != 0 {
		t.Fatal("discovery caused authority polling without subscribers")
	}
	if err := cs.Subscribe(ctx, &mcp.SubscribeParams{URI: "clawbridge://inbox/foreign"}); err == nil && version == "2025-11-25" {
		t.Fatal("foreign resource accepted")
	}
	if executor.count() != 0 {
		t.Fatal("invalid resource reached authority")
	}
	if _, err := cs.ReadResource(ctx, &mcp.ReadResourceParams{URI: "clawbridge://inbox/foreign"}); err == nil {
		t.Fatal("foreign resource read accepted")
	}
	if executor.count() != 0 {
		t.Fatal("foreign resource read reached authority")
	}
	result, err := cs.ReadResource(ctx, &mcp.ReadResourceParams{URI: mcpserver.InboxStatusURI})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Contents) != 1 {
		t.Fatal("resource contents malformed")
	}
	var status bus.Result
	if err := json.Unmarshal([]byte(result.Contents[0].Text), &status); err != nil {
		t.Fatal(err)
	}
	notificationStatus(t, status, nil, false, 2, 9, 2)
	select {
	case <-waits:
		t.Fatal("resource read started background poll")
	default:
	}
	if err := cs.Subscribe(ctx, &mcp.SubscribeParams{URI: mcpserver.InboxStatusURI}); err != nil {
		t.Fatal(err)
	}
	// Initial backlog must reach the real client after SDK registration; no
	// assumption is made about first-poll timer scheduling.
	notificationAwaitNotice(t, ctx, notices)
	next := notificationAwaitWait(t, ctx, waits)
	close(next.release)
	next = notificationAwaitWait(t, ctx, waits)
	executor.set(0, 0)
	close(next.release)
	notificationAwaitNotice(t, ctx, notices)
	next = notificationAwaitWait(t, ctx, waits)
	if err := cs.Unsubscribe(ctx, &mcp.UnsubscribeParams{URI: mcpserver.InboxStatusURI}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-next.ctx.Done():
	case <-ctx.Done():
		t.Fatal("last unsubscribe left poller running")
	}
	executor.mu.Lock()
	defer executor.mu.Unlock()
	for i, r := range executor.requests {
		if r.Operation != "inbox_status" || r.Args["instance"] != "private-instance" || r.Args["after"] != int64(0) {
			t.Fatalf("poll mutated or lost actor: %+v", r)
		}
		if i > 0 && (r.Workspace != "ws_canonical" || r.Project != "prj_canonical" || r.Session != "ses_canonical") {
			t.Fatalf("resource did not pin canonical recipient: %+v", r)
		}
	}
}
func TestNotificationMCPDisconnectAndRotationStop(t *testing.T) {
	for _, action := range []string{"disconnect", "resume"} {
		t.Run(action, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			executor := &notificationMCPExecutor{unread: 1, sequence: 1}
			waits := make(chan notificationMCPWait, 5)
			notices := make(chan string, 5)
			server := mcpserver.NewServerWithOptions(executor, "workspace-alias", "project-alias", "recipient-alias", "private-instance", mcpserver.Options{Context: ctx, Wait: func(ctx context.Context, _ time.Duration) error {
				w := notificationMCPWait{ctx: ctx, release: make(chan struct{})}
				select {
				case waits <- w:
				case <-ctx.Done():
					return ctx.Err()
				}
				<-ctx.Done()
				return ctx.Err()
			}})
			cs := notificationMCPConnect(t, ctx, server, notices)
			if err := cs.Subscribe(ctx, &mcp.SubscribeParams{URI: mcpserver.InboxStatusURI}); err != nil {
				t.Fatal(err)
			}
			notificationAwaitNotice(t, ctx, notices)
			waiting := notificationAwaitWait(t, ctx, waits)
			if action == "disconnect" {
				cs.Close()
			} else {
				result, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "register_session", Arguments: map[string]any{"resume": true}})
				if err != nil || result.IsError {
					t.Fatalf("resume failed: %v", err)
				}
			}
			select {
			case <-waiting.ctx.Done():
			case <-ctx.Done():
				t.Fatal("disconnect/rotation left old poller alive")
			}
			select {
			case <-notices:
				t.Fatal("old actor published backlog after stop")
			default:
			}
			executor.mu.Lock()
			ops := []string{}
			for _, r := range executor.requests {
				ops = append(ops, r.Operation)
			}
			executor.mu.Unlock()
			want := []string{"inbox_status", "inbox_status"}
			if action == "resume" {
				want = append(want, "register_session")
			}
			if !reflect.DeepEqual(ops, want) {
				t.Fatalf("unexpected old actor calls: %v", ops)
			}
		})
	}
}

func TestNotificationMCPSharedPollerPerListener(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	executor := &notificationMCPExecutor{unread: 1, sequence: 2}
	waits := make(chan notificationMCPWait, 5)
	server := mcpserver.NewServerWithOptions(executor, "workspace-alias", "project-alias", "recipient-alias", "private-instance", mcpserver.Options{Context: ctx, Wait: func(ctx context.Context, _ time.Duration) error {
		w := notificationMCPWait{ctx: ctx, release: make(chan struct{})}
		select {
		case waits <- w:
		case <-ctx.Done():
			return ctx.Err()
		}
		select {
		case <-w.release:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}})
	n1, n2 := make(chan string, 10), make(chan string, 10)
	cs1 := notificationMCPConnect(t, ctx, server, n1)
	cs2 := notificationMCPConnect(t, ctx, server, n2)
	if err := cs1.Subscribe(ctx, &mcp.SubscribeParams{URI: mcpserver.InboxStatusURI}); err != nil {
		t.Fatal(err)
	}
	notificationAwaitNotice(t, ctx, n1)
	pending := notificationAwaitWait(t, ctx, waits)
	count := executor.count()
	if err := cs2.Subscribe(ctx, &mcp.SubscribeParams{URI: mcpserver.InboxStatusURI}); err != nil {
		t.Fatal(err)
	}
	if executor.count() != count+1 {
		t.Fatal("second listener started independent polling instead of subscription validation only")
	}
	select {
	case <-waits:
		t.Fatal("second listener started an extra poll loop")
	default:
	}
	close(pending.release)
	notificationAwaitNotice(t, ctx, n2)
	pending = notificationAwaitWait(t, ctx, waits)
	select {
	case <-n1:
		t.Fatal("second subscription repeated unchanged backlog to existing listener")
	default:
	}
	if err := cs1.Unsubscribe(ctx, &mcp.UnsubscribeParams{URI: mcpserver.InboxStatusURI}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-pending.ctx.Done():
		t.Fatal("first unsubscribe stopped remaining listener")
	default:
	}
	executor.set(2, 3)
	close(pending.release)
	notificationAwaitNotice(t, ctx, n2)
	pending = notificationAwaitWait(t, ctx, waits)
	select {
	case <-n1:
		t.Fatal("unsubscribed listener received notice")
	default:
	}
	if err := cs2.Unsubscribe(ctx, &mcp.UnsubscribeParams{URI: mcpserver.InboxStatusURI}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-pending.ctx.Done():
	case <-ctx.Done():
		t.Fatal("last listener left loop running")
	}
}

func TestNotificationMCPResubscribeDoesNotAccumulateWaiters(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	executor := &notificationMCPExecutor{}
	server := mcpserver.NewServerWithOptions(executor, "workspace-alias", "project-alias", "recipient-alias", "private-instance", mcpserver.Options{Context: ctx, Wait: func(ctx context.Context, _ time.Duration) error { <-ctx.Done(); return ctx.Err() }})
	notices := make(chan string, 50)
	client := notificationMCPConnect(t, ctx, server, notices)
	waiters := func() int {
		var stack bytes.Buffer
		if err := pprof.Lookup("goroutine").WriteTo(&stack, 2); err != nil {
			t.Fatal(err)
		}
		return strings.Count(stack.String(), "clawbridge/internal/mcpserver.(*inboxResource).subscribe.func")
	}
	baseline := waiters()
	for i := 0; i < 20; i++ {
		if err := client.Subscribe(ctx, &mcp.SubscribeParams{URI: mcpserver.InboxStatusURI}); err != nil {
			t.Fatal(err)
		}
		if err := client.Unsubscribe(ctx, &mcp.UnsubscribeParams{URI: mcpserver.InboxStatusURI}); err != nil {
			t.Fatal(err)
		}
	}
	// A single session-lifetime disconnect observer is reasonable; retaining one
	// extra observer for every inactive subscription is unbounded accumulation.
	if got := waiters() - baseline; got > 1 {
		t.Errorf("20 subscribe/unsubscribe cycles retained %d session waiters", got)
	}
}

func TestNotificationMCPIdempotentRegistrationPreservesSubscription(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	base := &notificationMCPExecutor{unread: 1, sequence: 1}
	waits := make(chan notificationMCPWait, 5)
	notices := make(chan string, 5)
	executor := notificationExecutorFunc(func(ctx context.Context, r bus.Request) (bus.Result, error) {
		if r.Operation == "register_session" {
			return bus.Result{Source: "local-authority", Data: map[string]any{"instance": "private-instance"}}, nil
		}
		return base.Execute(ctx, r)
	})
	server := mcpserver.NewServerWithOptions(executor, "workspace-alias", "project-alias", "recipient-alias", "private-instance", mcpserver.Options{Context: ctx, Wait: func(ctx context.Context, _ time.Duration) error {
		w := notificationMCPWait{ctx: ctx, release: make(chan struct{})}
		select {
		case waits <- w:
		case <-ctx.Done():
			return ctx.Err()
		}
		select {
		case <-w.release:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}})
	client := notificationMCPConnect(t, ctx, server, notices)
	if err := client.Subscribe(ctx, &mcp.SubscribeParams{URI: mcpserver.InboxStatusURI}); err != nil {
		t.Fatal(err)
	}
	notificationAwaitNotice(t, ctx, notices)
	pending := notificationAwaitWait(t, ctx, waits)
	registered, err := client.CallTool(ctx, &mcp.CallToolParams{Name: "register_session", Arguments: map[string]any{}})
	if err != nil || registered.IsError {
		t.Fatalf("same-instance registration failed: %v", err)
	}
	select {
	case <-pending.ctx.Done():
		t.Error("idempotent registration cancelled an established actor subscription")
	default:
	}
	base.set(2, 2)
	close(pending.release)
	notificationAwaitNotice(t, ctx, notices)
}
