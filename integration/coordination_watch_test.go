package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"clawbridge/internal/bus"
	"clawbridge/internal/mcpserver"
	"clawbridge/internal/notify"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func coordinationPolicyResult(policy string, unread, latest, fresh int64) bus.Result {
	r := notificationResult(unread, latest, fresh)
	r.Data = notify.NotificationStatus{Status: r.Data.(notify.Status), Policy: policy}
	return r
}

func TestCoordinationRealStoreWatcherPolicyNoStamp(t *testing.T) {
	a := newNotificationAuthority(t, false)
	request := func(operation, session string, args map[string]any) bus.Request {
		return bus.Request{Operation: operation, Workspace: "w", Project: "p", Session: session, Args: actor(session, args)}
	}
	sent, err := a.execute(request("send", "lead", map[string]any{"to": "worker", "body": "durable accepted coordination context", "operation_id": "policy-real-store-mail", "coordination": coordinationObject("private-context-not-hint", "ready")}))
	if err != nil {
		t.Fatal(err)
	}
	id := stringField(t, jsonData(t, sent.Data), "id")
	c := notificationConfig(t)
	c.Binding.Workspace = "w"
	c.Binding.Project = "p"
	c.Binding.Session = "worker"
	c.Binding.Instance = "worker-instance"
	var out bytes.Buffer
	c.Output = &out
	c.Executor = notificationExecutorFunc(func(_ context.Context, r bus.Request) (bus.Result, error) {
		if r.Operation != "notification_status" {
			t.Fatalf("watcher used heartbeat/stamping/fallback operation %s", r.Operation)
		}
		return a.execute(r)
	})
	for step, policy := range []string{"hold", "refuse", "accept", "accept"} {
		if _, err = a.execute(request("set_notification_policy", "worker", map[string]any{"policy": policy})); err != nil {
			t.Fatal(err)
		}
		a.exec("UPDATE sessions SET last_seen=? WHERE project_id IN (SELECT id FROM projects WHERE workspace_id=?)", "2001-01-01T00:00:00.000000000Z", a.workspaceID)
		before := a.snapshot()
		out.Reset()
		if err = notify.Run(context.Background(), c); err != nil {
			t.Fatal(err)
		}
		if before != a.snapshot() {
			t.Fatal("full watcher altered domain state under policy")
		}
		if (out.Len() != 0) != (step == 2) {
			t.Fatal("real policy failed suppression/one-time backlog acceptance")
		}
		if strings.Contains(out.String(), "private-context-not-hint") || strings.Contains(out.String(), coordinationIssue) || strings.Contains(out.String(), "durable accepted coordination context") {
			t.Fatal("hint disclosed participant-only metadata/body")
		}
	}
	// Full watcher and policy changes retain the delivery; accepted reminder
	// cursor is not a delivered/read stamp and never erases accepted mail.
	var delivered, read any
	if err = a.db.QueryRow("SELECT delivered_at,read_at FROM deliveries WHERE message_id=?", id).Scan(&delivered, &read); err != nil {
		t.Fatal(err)
	}
	if delivered != nil || read != nil {
		t.Fatal("notification suppressed/accepted mail was stamped")
	}
	status, err := a.execute(request("notification_status", "worker", nil))
	if err != nil {
		t.Fatal(err)
	}
	if object(t, jsonData(t, status.Data))["unread_count"] != float64(1) {
		t.Fatal("wake policy erased accepted unread mail")
	}
}

func TestCoordinationWatcherSuppressionRestartAndBacklog(t *testing.T) {
	for _, adapter := range []string{"json", "codex"} {
		t.Run(adapter, func(t *testing.T) {
			c := notificationConfig(t)
			if adapter == "codex" {
				c = notificationCodexConfig(t)
			}
			var out, diag bytes.Buffer
			c.Output = &out
			c.Diagnostics = &diag
			queued := 0
			runner := notify.RunnerFunc(func(_ context.Context, path string, args []string) ([]byte, error) {
				if reflect.DeepEqual(args, []string{"queue", "--help"}) {
					return []byte("queue --thread --message"), nil
				}
				if len(args) != 5 || args[0] != "queue" || args[1] != "--thread" || args[2] != c.Binding.Thread || args[3] != "--message" {
					t.Fatalf("unsafe queue argv: %q", args)
				}
				for _, bad := range []string{"assignment_revision", "issue_id", "peer context", "private-instance"} {
					if strings.Contains(args[4], bad) {
						t.Fatal("structured context/secret leaked into hint")
					}
				}
				queued++
				return []byte("not proof of processing"), nil
			})
			if adapter == "codex" {
				c.Runner = runner
			}
			policy := "hold"
			unread, latest := int64(2), int64(9)
			wantAfter := int64(0)
			calls := 0
			c.Executor = notificationExecutorFunc(func(_ context.Context, r bus.Request) (bus.Result, error) {
				calls++
				after := notificationCursor(t, r)
				if after != wantAfter {
					t.Fatalf("suppressed/read status advanced/regressed cursor: %d want %d", after, wantAfter)
				}
				fresh := unread
				if latest <= after {
					fresh = 0
				}
				return coordinationPolicyResult(policy, unread, latest, fresh), nil
			})
			cursor := func() int64 {
				raw, err := os.ReadFile(c.StatePath)
				if err != nil {
					t.Fatal(err)
				}
				var v struct {
					After int64 `json:"after"`
				}
				if json.Unmarshal(raw, &v) != nil {
					t.Fatal("invalid checkpoint")
				}
				return v.After
			}
			// Separate invocations exercise suppressed checkpoint persistence/restart,
			// then acceptance of exactly the previously suppressed backlog.
			for _, step := range []struct {
				policy                string
				unread, latest, after int64
				emit                  bool
			}{
				{"hold", 2, 9, 0, false}, {"hold", 2, 9, 0, false}, {"refuse", 2, 9, 0, false}, {"accept", 2, 9, 0, true},
				{"hold", 3, 12, 9, false}, {"refuse", 3, 12, 9, false}, {"accept", 3, 12, 9, true}, {"accept", 3, 12, 12, false}, {"accept", 0, 0, 12, false},
			} {
				policy, unread, latest, wantAfter = step.policy, step.unread, step.latest, step.after
				out.Reset()
				beforeQueue := queued
				if err := notify.Run(context.Background(), c); err != nil {
					t.Fatal(err)
				}
				if adapter == "json" && (out.Len() != 0) != step.emit {
					t.Fatalf("policy %s output=%q emit=%v", policy, out.String(), step.emit)
				}
				if adapter == "codex" && (queued > beforeQueue) != step.emit {
					t.Fatalf("policy %s incorrectly queued/suppressed", policy)
				}
				want := wantAfter
				if step.emit {
					want = latest
				}
				if got := cursor(); got != want {
					t.Fatalf("policy %s checkpoint=%d want=%d", policy, got, want)
				}
				notificationNoTemps(t, c.StatePath)
			}
			if calls != 9 {
				t.Fatalf("unexpected fallback/bootstrap polling: %d", calls)
			}
		})
	}
}

func TestCoordinationOldServiceAndMalformedPolicyFailClosed(t *testing.T) {
	for _, kind := range []string{"legacy-six", "null-policy", "invalid-policy", "extra-field", "mirror"} {
		t.Run(kind, func(t *testing.T) {
			c := notificationConfig(t)
			var out bytes.Buffer
			c.Output = &out
			c.Once = false
			calls := 0
			c.Wait = func(context.Context, time.Duration) error {
				t.Fatal("malformed policy retried")
				return context.Canceled
			}
			c.Executor = notificationExecutorFunc(func(_ context.Context, r bus.Request) (bus.Result, error) {
				calls++
				if r.Operation != "notification_status" {
					t.Fatal("old-status fallback")
				}
				if kind == "legacy-six" {
					return notificationResult(1, 1, 1), nil
				}
				raw := object(t, jsonData(t, coordinationPolicyResult("accept", 1, 1, 1).Data))
				switch kind {
				case "null-policy":
					raw["policy"] = nil
				case "invalid-policy":
					raw["policy"] = "pause"
				case "extra-field":
					raw["secret"] = "PRIVATE"
				}
				source := "server"
				if kind == "mirror" {
					source = "offline-mirror"
				}
				return bus.Result{Source: source, Data: raw}, nil
			})
			if err := notify.Run(context.Background(), c); err == nil || !notify.IsPermanent(err) {
				t.Fatalf("invalid policy accepted/nonterminal: %v", err)
			}
			if out.Len() != 0 || calls != 1 {
				t.Fatal("invalid service produced wake/fallback")
			}
			notificationNoTemps(t, c.StatePath)
		})
	}
}

func TestCoordinationMCPPolicySuppressionPreservesListenerCursor(t *testing.T) {
	for _, version := range []string{"2025-11-25", "2026-07-28"} {
		t.Run(version, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			policy := "hold"
			unread, latest := int64(1), int64(3)
			// Changes occur only while the loop is parked at its injected Wait.
			waits := make(chan notificationMCPWait, 10)
			notices := make(chan string, 10)
			executor := notificationExecutorFunc(func(_ context.Context, r bus.Request) (bus.Result, error) {
				if r.Operation == "inbox_status" {
					return notificationResult(unread, latest, unread), nil
				}
				if r.Operation != "notification_status" {
					t.Fatalf("unexpected automatic operation %s", r.Operation)
				}
				return coordinationPolicyResult(policy, unread, latest, unread), nil
			})
			server := mcpserver.NewServerWithOptions(executor, "w", "p", "recipient", "private-instance", mcpserver.Options{Context: ctx, Wait: func(ctx context.Context, _ time.Duration) error {
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
			client := notificationMCPConnect(t, ctx, server, notices, version)
			if err := client.Subscribe(ctx, &mcp.SubscribeParams{URI: mcpserver.InboxStatusURI}); err != nil {
				t.Fatal(err)
			}
			next := notificationAwaitWait(t, ctx, waits)
			noNotice := func() {
				t.Helper()
				select {
				case <-notices:
					t.Fatal("suppressed/unchanged policy emitted notice")
				default:
				}
			}
			noNotice()
			policy = "refuse"
			close(next.release)
			next = notificationAwaitWait(t, ctx, waits)
			noNotice()
			// A direct read is allowed and remains six-field, without changing policy
			// or establishing that any message was delivered/read.
			resource, err := client.ReadResource(ctx, &mcp.ReadResourceParams{URI: mcpserver.InboxStatusURI})
			if err != nil {
				t.Fatal(err)
			}
			var envelope bus.Result
			if len(resource.Contents) != 1 || json.Unmarshal([]byte(resource.Contents[0].Text), &envelope) != nil || len(object(t, envelope.Data)) != 6 {
				t.Fatal("resource changed to policy envelope")
			}
			policy = "accept"
			close(next.release)
			notificationAwaitNotice(t, ctx, notices)
			next = notificationAwaitWait(t, ctx, waits)
			// An accepted tuple is remembered; suppressed later unread changes are
			// NOT acknowledged and must invalidate after accept resumes.
			policy = "hold"
			unread, latest = 2, 7
			close(next.release)
			next = notificationAwaitWait(t, ctx, waits)
			noNotice()
			policy = "accept"
			close(next.release)
			notificationAwaitNotice(t, ctx, notices)
			next = notificationAwaitWait(t, ctx, waits)
			close(next.release)
			next = notificationAwaitWait(t, ctx, waits)
			noNotice()
			if err = client.Unsubscribe(ctx, &mcp.UnsubscribeParams{URI: mcpserver.InboxStatusURI}); err != nil {
				t.Fatal(err)
			}
			select {
			case <-next.ctx.Done():
			case <-ctx.Done():
				t.Fatal("unsubscribe left poll loop")
			}
		})
	}
}
