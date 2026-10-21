package integration

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"clawbridge/internal/bus"
	"clawbridge/internal/mcpserver"
	"clawbridge/internal/notify"
	"clawbridge/internal/remote"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type notificationErrorFixture struct {
	executor                              notify.Executor
	revoke                                func()
	workspace, project, session, instance string
}

func notificationRealErrorFixture(t *testing.T, kind string) notificationErrorFixture {
	t.Helper()
	if kind == "fenced" || kind == "retired" || kind == "moved" {
		f := newFixture(t)
		f.setup("w", "p")
		return notificationErrorFixture{executor: f.store, workspace: "w", project: "p", session: "worker", instance: "worker-instance", revoke: func() {
			switch kind {
			case "fenced":
				f.call("register_session", "w", "p", "worker", map[string]any{"instance": "worker-instance", "resume": true})
			case "retired":
				f.call("retire_session", "w", "p", "worker", actor("worker", nil))
			case "moved":
				f.call("retire_session", "w", "p", "worker", actor("worker", map[string]any{"successor": "outsider"}))
			}
		}}
	}
	var rejected atomic.Bool
	endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if rejected.Load() && kind == "unauthorized" {
			w.WriteHeader(401)
			w.Write([]byte(`{"error":{"code":"unauthenticated"}}`))
			return
		}
		if rejected.Load() && kind == "malformed-response" {
			w.Write([]byte(`invalid cloud JSON`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"source":"server","data":{"workspace_id":"ws_canonical","project_id":"prj_canonical","session_id":"ses_canonical","unread_count":0,"latest_sequence":0,"new_count":0,"policy":"accept"}}`))
	}))
	t.Cleanup(endpoint.Close)
	client, err := remote.NewWithTokenSource(remote.Config{URL: endpoint.URL, Audience: "synthetic-test-audience", Principal: "recipient@example.test"}, remote.TokenSourceFunc(func(context.Context) (string, error) {
		if rejected.Load() {
			switch kind {
			case "credential-error":
				return "", errors.New("synthetic source cannot load credential")
			case "invalid-token":
				return "invalid token with whitespace", nil
			}
		}
		return "synthetic-fixture-token", nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	return notificationErrorFixture{executor: client, revoke: func() { rejected.Store(true) }, workspace: "workspace-alias", project: "project-alias", session: "recipient-alias", instance: "private-instance"}
}
func TestNotificationRealDefinitiveErrorsStopWatcher(t *testing.T) {
	for _, kind := range []string{"fenced", "retired", "moved", "credential-error", "invalid-token", "unauthorized", "malformed-response"} {
		t.Run(kind, func(t *testing.T) {
			fixture := notificationRealErrorFixture(t, kind)
			c := notificationConfig(t)
			c.Once = false
			c.Binding.Workspace = fixture.workspace
			c.Binding.Project = fixture.project
			c.Binding.Session = fixture.session
			c.Binding.Instance = fixture.instance
			calls, waits := 0, 0
			c.Executor = notificationExecutorFunc(func(ctx context.Context, r bus.Request) (bus.Result, error) {
				calls++
				return fixture.executor.Execute(ctx, r)
			})
			c.Wait = func(context.Context, time.Duration) error {
				waits++
				if waits == 1 {
					fixture.revoke()
					return nil
				}
				t.Error("definitive Store/remote error entered retry backoff")
				return context.Canceled
			}
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			err := notify.Run(ctx, c)
			if err == nil || !notify.IsPermanent(err) {
				t.Fatalf("real definitive error not terminal/permanent: %v", err)
			}
			if calls != 2 || waits != 1 {
				t.Fatalf("definitive error looped: polls=%d waits=%d", calls, waits)
			}
			notificationNoTemps(t, c.StatePath)
		})
	}
}
func TestNotificationRealDefinitiveErrorsStopMCP(t *testing.T) {
	for _, kind := range []string{"fenced", "retired", "moved", "credential-error", "invalid-token", "unauthorized", "malformed-response"} {
		t.Run(kind, func(t *testing.T) {
			fixture := notificationRealErrorFixture(t, kind)
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			var calls atomic.Int64
			failure := make(chan error, 1)
			waits := make(chan notificationMCPWait, 4)
			notices := make(chan string, 4)
			executor := notificationExecutorFunc(func(ctx context.Context, r bus.Request) (bus.Result, error) {
				calls.Add(1)
				result, err := fixture.executor.Execute(ctx, r)
				if err != nil {
					failure <- err
				}
				return result, err
			})
			server := mcpserver.NewServerWithOptions(executor, fixture.workspace, fixture.project, fixture.session, fixture.instance, mcpserver.Options{Context: ctx, Wait: func(ctx context.Context, _ time.Duration) error {
				pending := notificationMCPWait{ctx: ctx, release: make(chan struct{})}
				select {
				case waits <- pending:
				case <-ctx.Done():
					return ctx.Err()
				}
				select {
				case <-pending.release:
					return nil
				case <-ctx.Done():
					return ctx.Err()
				}
			}})
			client := notificationMCPConnect(t, ctx, server, notices)
			if err := client.Subscribe(ctx, &mcp.SubscribeParams{URI: mcpserver.InboxStatusURI}); err != nil {
				t.Fatal(err)
			}
			pending := notificationAwaitWait(t, ctx, waits)
			fixture.revoke()
			close(pending.release)
			select {
			case err := <-failure:
				if !notify.IsPermanent(err) {
					t.Fatalf("actual Store/remote error lacks definitive classification: %v", err)
				}
			case <-ctx.Done():
				t.Fatal("post-revocation poll never returned its real error")
			}
			// Terminating a poller need not cancel the caller-owned Options
			// context. Observe retry behavior directly rather than equating it
			// with cancellation of the injected Wait context.
			select {
			case <-waits:
				t.Fatal("real definitive error retried MCP polling instead of stopping")
			case <-time.After(40 * time.Millisecond):
			case <-ctx.Done():
				t.Fatal("protocol fixture timed out")
			}
			if calls.Load() != 3 {
				t.Fatalf("unexpected validation/initial/failure calls=%d", calls.Load())
			}
			select {
			case <-notices:
				t.Fatal("definitive rejection emitted inbox hint")
			default:
			}
		})
	}
}
