package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"clawbridge/internal/faketool"
	"clawbridge/internal/remote"
)

// A send the server can't take waits in the outbox and says so; outbox
// list shows it without its body, flush sends it with its operation ID,
// and drop asks for the word.
func TestOutboxCommands(t *testing.T) {
	dir := isolated(t)
	faketool.Install(t, dir, "gcloud", "print:synthetic-cli-token")
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	principal := "service@example.test"
	var down atomic.Bool
	var mu sync.Mutex
	var sent []map[string]any
	host := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Operation string
			Args      map[string]any
		}
		json.NewDecoder(r.Body).Decode(&req)
		data := map[string]any{}
		switch req.Operation {
		case "resolve_project":
			data = map[string]any{"workspace_id": "ws-cloud", "project_id": "prj-cloud"}
		case "send":
			if down.Load() {
				// A Retry-After past the client's in-process window makes
				// it queue after its first try, however slow the run is
				// (-race), instead of on the call's deadline.
				w.Header().Set("Retry-After", "30")
				w.WriteHeader(503)
				return
			}
			mu.Lock()
			sent = append(sent, req.Args)
			mu.Unlock()
			data = map[string]any{"id": "msg-1"}
		default:
			t.Errorf("unexpected cloud operation %s", req.Operation)
		}
		json.NewEncoder(w).Encode(map[string]any{"source": "server", "data": data})
	}))
	defer host.Close()
	for k, v := range map[string]string{"MODE": "cloud", "SERVER": host.URL, "AUDIENCE": "test-audience", "ACCOUNT": "human@example.test", "IMPERSONATE_SERVICE_ACCOUNT": principal, "PRINCIPAL": principal, "CACHE": filepath.Join(dir, "profile", "mirror.db"), "WORKSPACE": "w", "PROJECT": "p", "SESSION": "a", "INSTANCE": "cloud-inst"} {
		t.Setenv("CLAWBRIDGE_"+k, v)
	}
	run := func(timeout time.Duration, args ...string) (map[string]any, string, error) {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		defer cancel()
		var out, diag bytes.Buffer
		err := Run(ctx, append([]string{"--json"}, args...), &out, &diag)
		var value map[string]any
		if err == nil {
			if e := json.Unmarshal(out.Bytes(), &value); e != nil {
				t.Fatal(e, out.String())
			}
		}
		return value, diag.String(), err
	}
	down.Store(true)
	out, diag, err := run(10*time.Second, "send", "peer", "the plan", "--operation-id", "op-1")
	if err != nil || out["source"] != "client_outbox" {
		t.Fatal(out, err)
	}
	if data := out["data"].(map[string]any); data["outcome"] != "queued_local" || data["operation_id"] != "op-1" {
		t.Fatal(data)
	}
	if !strings.Contains(diag, "send op-1 is queued on this machine") {
		t.Fatal(diag)
	}
	if info, e := os.Stat(filepath.Join(dir, "profile", "outbox.db")); e != nil || (runtime.GOOS != "windows" && info.Mode().Perm() != 0600) {
		t.Fatal(info, e)
	}
	out, _, err = run(5*time.Second, "outbox", "list")
	items, _ := out["data"].(map[string]any)["items"].([]any)
	if err != nil || len(items) != 1 {
		t.Fatal(out, err)
	}
	raw, _ := json.Marshal(items)
	if strings.Contains(string(raw), "the plan") || !strings.Contains(string(raw), `"operation_id":"op-1"`) {
		t.Fatal(string(raw))
	}
	if _, _, err = run(5*time.Second, "outbox", "drop", "1"); err == nil || !strings.Contains(err.Error(), "--confirm drop") {
		t.Fatal(err)
	}
	if _, _, err = run(5*time.Second, "outbox", "retry", "1"); err == nil {
		t.Fatal("retried a waiting write")
	}
	down.Store(false)
	time.Sleep(1100 * time.Millisecond) // the queue's backoff after its first failure
	out, diag, err = run(5*time.Second, "outbox", "flush")
	if err != nil || out["data"].(map[string]any)["sent"] != float64(1) || !strings.Contains(diag, "sent 1 queued write") {
		t.Fatal(out, diag, err)
	}
	mu.Lock()
	if len(sent) != 1 || sent[0]["operation_id"] != "op-1" || sent[0]["body"] != "the plan" {
		t.Fatal(sent)
	}
	mu.Unlock()
	if out, _, err = run(5*time.Second, "outbox", "list"); err != nil || len(out["data"].(map[string]any)["items"].([]any)) != 0 {
		t.Fatal(out, err)
	}
	for _, bad := range [][]string{{"outbox"}, {"outbox", "nope"}, {"outbox", "retry"}, {"outbox", "drop", "x"}, {"outbox", "flush", "--dead"}, {"outbox", "list", "extra"}} {
		if _, _, err = run(5*time.Second, bad...); err == nil {
			t.Fatal(bad)
		}
	}
}

func TestOutboxCommandKillSwitch(t *testing.T) {
	dir := isolated(t)
	t.Setenv("CLAWBRIDGE_CLIENT_OUTBOX", "off")
	for k, v := range map[string]string{"MODE": "cloud", "SERVER": "https://bus.example.test", "AUDIENCE": "test-audience", "PRINCIPAL": "service@example.test", "CACHE": filepath.Join(dir, "mirror.db"), "ACCOUNT": "human@example.test", "IMPERSONATE_SERVICE_ACCOUNT": "service@example.test"} {
		t.Setenv("CLAWBRIDGE_"+k, v)
	}
	var out, diag bytes.Buffer
	if err := Run(context.Background(), []string{"outbox", "list"}, &out, &diag); err == nil || !strings.Contains(err.Error(), "the client outbox is off") {
		t.Fatal(err)
	}
}

// A command closes the outboxes it opened when it ends, but a command run
// inside another (the wizard's dispatcher) leaves them to the outer one.
func TestRunClosesTheOutboxesAtTheOutermostEnd(t *testing.T) {
	isolated(t)
	client, err := remote.NewWithTokenSource(remote.Config{URL: "https://bus.example.test", Audience: "audience", Principal: "service@example.test", CachePath: filepath.Join(t.TempDir(), "cache.db")}, remote.TokenSourceFunc(func(context.Context) (string, error) { return "synthetic-token", nil }))
	if err != nil {
		t.Fatal(err)
	}
	ob := client.Outbox()
	if ob == nil {
		t.Fatal("no outbox")
	}
	var out bytes.Buffer
	runDepth.Add(1)
	err = Run(context.Background(), []string{"version"}, &out, &out)
	runDepth.Add(-1)
	if _, e := ob.Status(context.Background()); err != nil || e != nil {
		t.Fatal("an inner command closed the outer one's outbox:", err, e)
	}
	if err = Run(context.Background(), []string{"version"}, &out, &out); err != nil {
		t.Fatal(err)
	}
	if _, e := ob.Status(context.Background()); e == nil {
		t.Fatal("the outbox stayed open after the command")
	}
}
