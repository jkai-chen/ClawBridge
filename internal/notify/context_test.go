package notify

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"clawbridge/internal/bus"
	"clawbridge/internal/ctxwatch"
)

// fakeReports answers report_context as the server would, from a script.
type fakeReports struct {
	mu      sync.Mutex
	reports []map[string]any
	answer  func(args map[string]any) (map[string]any, error)
}

func (f *fakeReports) Execute(_ context.Context, r bus.Request) (bus.Result, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r.Operation != "report_context" {
		return bus.Result{}, errors.New("unexpected " + r.Operation)
	}
	f.reports = append(f.reports, r.Args)
	if f.answer != nil {
		data, err := f.answer(r.Args)
		return bus.Result{Data: data}, err
	}
	return bus.Result{Data: map[string]any{"enabled": true, "level": r.Args["level"], "compactions": r.Args["compactions"],
		"levels": map[string]any{"warn_tokens": float64(1000), "handover_tokens": float64(2000)}, "handover_hint": "Update the notes."}}, nil
}

func (f *fakeReports) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.reports)
}

func (f *fakeReports) last() map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.reports[len(f.reports)-1]
}

type reportFixture struct {
	t          *testing.T
	dir        string
	transcript string
	now        time.Time
}

func newReportFixture(t *testing.T) *reportFixture {
	t.Helper()
	f := &reportFixture{t: t, dir: filepath.Join(t.TempDir(), "context"), transcript: filepath.Join(t.TempDir(), "s.jsonl"), now: time.Date(2026, 1, 1, 10, 0, 0, 0, time.UTC)}
	f.write(500)
	if err := ctxwatch.SaveBinding(f.dir, "worker-1", ctxwatch.Binding{Agent: ctxwatch.ClaudeCode, Transcript: f.transcript, BoundAt: "2026-01-01T09:00:00Z"}); err != nil {
		t.Fatal(err)
	}
	return f
}

func assistant(tokens int) string {
	return `{"type":"assistant","sessionId":"s","timestamp":"2026-01-01T10:00:00Z","message":{"model":"m","usage":{"input_tokens":` + strconv.Itoa(tokens) + `}}}`
}

func isWindows() bool { return runtime.GOOS == "windows" }

// write replaces the transcript with lines ending at tokens, after the
// compactions given as their times.
func (f *reportFixture) write(tokens int, compactions ...string) {
	f.t.Helper()
	lines := []string{assistant(100)}
	for _, at := range compactions {
		lines = append(lines, `{"type":"system","subtype":"compact_boundary","sessionId":"s","timestamp":"`+at+`"}`)
	}
	lines = append(lines, assistant(tokens))
	if err := os.WriteFile(f.transcript, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		f.t.Fatal(err)
	}
}

func (f *reportFixture) reporter(e Executor) *contextReporter {
	return &contextReporter{e: e, dir: f.dir, now: func() time.Time { return f.now }}
}

var reportRequest = bus.Request{Workspace: "w", Project: "p", Session: "worker-1", Args: map[string]any{"instance": "i-1"}}

func (f *reportFixture) binding() ctxwatch.Binding {
	f.t.Helper()
	b, bound, err := ctxwatch.LoadBinding(f.dir, "worker-1")
	if err != nil || !bound {
		f.t.Fatal(err, bound)
	}
	return b
}

func TestReportContextWhenDue(t *testing.T) {
	f := newReportFixture(t)
	e := &fakeReports{}
	r := f.reporter(e)
	defer r.release()
	r.tick(context.Background(), reportRequest)
	if e.count() != 1 {
		t.Fatal(e.count())
	}
	got := e.last()
	if got["tokens"] != int64(500) || got["level"] != "ok" || got["compactions"] != int64(0) || got["instance"] != "i-1" || got["window"] != nil || got["observed_at"] != "2026-01-01T10:00:00Z" {
		t.Fatalf("%+v", got)
	}
	// The answer is cached for the hooks.
	if b := f.binding(); b.Levels == nil || b.Levels.HandoverTokens != 2000 || b.HandoverHint != "Update the notes." {
		t.Fatalf("%+v", b)
	}
	// Nothing changed: no report until the refresh.
	f.now = f.now.Add(time.Minute)
	r.tick(context.Background(), reportRequest)
	if e.count() != 1 {
		t.Fatal("reported with nothing changed")
	}
	// A level change reports, with the cached levels.
	f.write(1500)
	r.tick(context.Background(), reportRequest)
	if e.count() != 2 || e.last()["level"] != "warn" {
		t.Fatalf("%+v", e.last())
	}
	f.now = f.now.Add(contextRefresh)
	r.tick(context.Background(), reportRequest)
	if e.count() != 3 {
		t.Fatal("no refresh")
	}
}

// A compaction is counted once, by its time, so a transcript tail that no
// longer shows an older one never lowers the count.
func TestReportContextCountsEachCompactionOnce(t *testing.T) {
	f := newReportFixture(t)
	e := &fakeReports{}
	r := f.reporter(e)
	defer r.release()
	r.tick(context.Background(), reportRequest)
	f.write(300, "2026-01-01T10:05:00Z")
	f.now = f.now.Add(time.Minute)
	r.tick(context.Background(), reportRequest)
	if e.last()["compactions"] != int64(1) {
		t.Fatalf("%+v", e.last())
	}
	if b := f.binding(); b.ReportedCompactions != 1 || b.SeenCompactionAt != "2026-01-01T10:05:00Z" {
		t.Fatalf("%+v", b)
	}
	f.now = f.now.Add(time.Minute)
	r.tick(context.Background(), reportRequest)
	if e.count() != 2 {
		t.Fatal("the same compaction reported again")
	}
	// The tail no longer shows the first one, and a second appears.
	f.write(300, "2026-01-01T10:20:00Z")
	f.now = f.now.Add(time.Minute)
	r.tick(context.Background(), reportRequest)
	if e.count() != 3 || e.last()["compactions"] != int64(2) {
		t.Fatalf("%+v", e.last())
	}
}

// Of two processes holding one session, only the lock holder reports; the
// other takes over once it is released.
func TestReportContextOneReporterPerSession(t *testing.T) {
	f := newReportFixture(t)
	e := &fakeReports{}
	first, second := f.reporter(e), f.reporter(e)
	defer second.release()
	first.tick(context.Background(), reportRequest)
	second.tick(context.Background(), reportRequest)
	if e.count() != 1 || first.lock == nil || second.lock != nil {
		t.Fatalf("%d reports", e.count())
	}
	if info, err := os.Stat(filepath.Join(f.dir, "worker-1.lock")); err != nil || (info.Mode().Perm()&0o077 != 0 && !isWindows()) {
		t.Fatalf("%v %v", info, err)
	}
	first.release()
	second.tick(context.Background(), reportRequest)
	if e.count() != 2 || second.lock == nil {
		t.Fatalf("no takeover: %d", e.count())
	}
}

func TestReportContextBacksOffAndResyncs(t *testing.T) {
	f := newReportFixture(t)
	off := &fakeReports{answer: func(map[string]any) (map[string]any, error) { return map[string]any{"enabled": false}, nil }}
	r := f.reporter(off)
	r.tick(context.Background(), reportRequest)
	f.now = f.now.Add(5 * time.Minute)
	f.write(1500)
	r.tick(context.Background(), reportRequest)
	if off.count() != 1 {
		t.Fatal("reported while the project has it off")
	}
	f.now = f.now.Add(contextRefresh)
	r.tick(context.Background(), reportRequest)
	if off.count() != 2 {
		t.Fatal("never asked again")
	}
	r.release()

	// The server holds a higher count: take it.
	refused := &fakeReports{answer: func(map[string]any) (map[string]any, error) {
		return nil, errors.New("invalid_compactions: the session's compaction count is 4, and never goes down")
	}}
	r = f.reporter(refused)
	defer r.release()
	r.tick(context.Background(), reportRequest)
	if b := f.binding(); b.ReportedCompactions != 4 {
		t.Fatalf("%+v", b)
	}
	f.now = f.now.Add(time.Minute)
	r.tick(context.Background(), reportRequest)
	if refused.last()["compactions"] != int64(4) {
		t.Fatalf("%+v", refused.last())
	}
}

func TestReportContextNeedsABoundReadableTranscript(t *testing.T) {
	f := newReportFixture(t)
	e := &fakeReports{}
	r := f.reporter(e)
	defer r.release()
	other := reportRequest
	other.Session = "worker-2"
	r.tick(context.Background(), other)
	if err := os.WriteFile(f.transcript, []byte("not a transcript\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	r.tick(context.Background(), reportRequest)
	if e.count() != 0 {
		t.Fatalf("%d reports", e.count())
	}
}

// A session the hook bound before its transcript was written has no level
// yet: nothing is reported, until the transcript is there.
func TestReportContextWaitsForAnUnwrittenTranscript(t *testing.T) {
	config := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", config)
	project := filepath.Join(config, "projects", "p")
	if err := os.MkdirAll(project, 0o700); err != nil {
		t.Fatal(err)
	}
	transcript, err := ctxwatch.ClaudeTranscript(filepath.Join(project, "s.jsonl"), config)
	if err != nil {
		t.Fatal(err)
	}
	f := newReportFixture(t)
	f.transcript = transcript
	if err := ctxwatch.SaveBinding(f.dir, "worker-1", ctxwatch.Binding{Agent: ctxwatch.ClaudeCode, Transcript: transcript, BoundAt: "2026-01-01T09:00:00Z", ClaudeSessionID: "s"}); err != nil {
		t.Fatal(err)
	}
	e := &fakeReports{}
	r := f.reporter(e)
	defer r.release()
	r.tick(context.Background(), reportRequest)
	if e.count() != 0 {
		t.Fatalf("reported %+v", e.last())
	}
	f.write(1500)
	r.tick(context.Background(), reportRequest)
	if e.count() != 1 || e.last()["tokens"] != int64(1500) {
		t.Fatalf("%d reports", e.count())
	}
}

// ReportContext runs the reporter on its interval until ctx ends.
func TestReportContextLoop(t *testing.T) {
	f := newReportFixture(t)
	e := &fakeReports{}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		ReportContext(ctx, e, f.dir, 10*time.Millisecond, func() (bus.Request, bool) { return reportRequest, true })
		close(done)
	}()
	deadline := time.Now().Add(5 * time.Second)
	for e.count() == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	<-done
	if e.count() == 0 {
		t.Fatal("never reported")
	}
	// The lock is released when the loop ends.
	again := f.reporter(e)
	defer again.release()
	again.tick(context.Background(), reportRequest)
	if again.lock == nil {
		t.Fatal("the lock outlived its loop")
	}
}

// Against a real local store: the report lands on the session, and the
// answer's levels and hint reach the binding record.
func TestReportContextAgainstTheStore(t *testing.T) {
	store, err := bus.Open(filepath.Join(t.TempDir(), "core.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	run := func(op, session string, a map[string]any) bus.Result {
		t.Helper()
		r, err := store.Execute(context.Background(), bus.Request{Operation: op, Workspace: "w", Project: "p", Session: session, Args: a})
		if err != nil {
			t.Fatalf("%s: %v", op, err)
		}
		return r
	}
	run("workspace_init", "", map[string]any{"mode": "local"})
	run("project_create", "", map[string]any{"slug": "p"})
	run("register_session", "worker-1", map[string]any{"instance": "i-1", "role": "worker"})
	run("context_watch_set", "", map[string]any{"expected_revision": 0, "context_watch": map[string]any{"enabled": true, "handover_hint": "Update the notes.",
		"levels": []any{map[string]any{"role": "*", "warn_tokens": 400, "handover_tokens": 2000}}}})
	f := newReportFixture(t)
	r := &contextReporter{e: store, dir: f.dir, now: time.Now}
	defer r.release()
	r.tick(context.Background(), reportRequest)
	if b := f.binding(); b.Levels == nil || b.Levels.WarnTokens != 400 || b.HandoverHint != "Update the notes." {
		t.Fatalf("%+v", b)
	}
	sessions := run("list_sessions", "", nil).Data.(map[string]any)["sessions"].([]map[string]any)
	if len(sessions) != 1 || sessions[0]["context_tokens"] != int64(500) || sessions[0]["context_level"] != "warn" {
		t.Fatalf("%+v", sessions)
	}
}

// The watcher reports its session's context when given the binding
// directory.
func TestWatcherReportsContext(t *testing.T) {
	interval := ContextInterval
	ContextInterval = 10 * time.Millisecond
	t.Cleanup(func() { ContextInterval = interval })
	f := newReportFixture(t)
	c := config(t)
	c.Once = false
	c.Binding.Session = "worker-1"
	c.ContextDir = f.dir
	reports := &fakeReports{}
	c.Executor = executorFunc(func(ctx context.Context, r bus.Request) (bus.Result, error) {
		if r.Operation == "report_context" {
			return reports.Execute(ctx, r)
		}
		return result(0, 0, 0), nil
	})
	c.Wait = func(ctx context.Context, d time.Duration) error {
		deadline := time.Now().Add(5 * time.Second)
		for reports.count() == 0 && time.Now().Before(deadline) {
			time.Sleep(5 * time.Millisecond)
		}
		return context.Canceled
	}
	if e := Run(context.Background(), c); !errors.Is(e, context.Canceled) {
		t.Fatal(e)
	}
	if reports.count() == 0 || reports.last()["instance"] != "SECRET-INSTANCE" {
		t.Fatal("the watcher never reported its context")
	}
}

// Run returns only once the context reporter has stopped: a report still
// in flight when the loop ends holds Run, so nothing writes under
// ContextDir after it returns (the TempDir cleanup flake on #202).
func TestWatcherWaitsForTheReporter(t *testing.T) {
	interval := ContextInterval
	ContextInterval = 10 * time.Millisecond
	t.Cleanup(func() { ContextInterval = interval })
	f := newReportFixture(t)
	c := config(t)
	c.Once = false
	c.Binding.Session = "worker-1"
	c.ContextDir = f.dir
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	c.Executor = executorFunc(func(ctx context.Context, r bus.Request) (bus.Result, error) {
		if r.Operation == "report_context" {
			// A report that outlives its context, as a slow write would.
			once.Do(func() { close(entered) })
			<-release
			return bus.Result{Source: "server", Data: map[string]any{}}, nil
		}
		return result(0, 0, 0), nil
	})
	c.Wait = func(ctx context.Context, d time.Duration) error {
		select {
		case <-entered:
		case <-time.After(5 * time.Second):
		}
		return context.Canceled
	}
	done := make(chan error, 1)
	go func() { done <- Run(context.Background(), c) }()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the reporter never reported")
	}
	select {
	case e := <-done:
		close(release)
		t.Fatal("Run returned while the reporter could still write:", e)
	case <-time.After(200 * time.Millisecond):
	}
	close(release)
	select {
	case e := <-done:
		if !errors.Is(e, context.Canceled) {
			t.Fatal(e)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run never returned")
	}
}

// R105-M1: the PreCompact hook's trigger goes with the compaction it noted,
// so the server can tell a manual /compact from the auto-compact backstop.
func TestReportContextSendsTheCompactionTrigger(t *testing.T) {
	f := newReportFixture(t)
	e := &fakeReports{}
	r := f.reporter(e)
	defer r.release()
	r.tick(context.Background(), reportRequest)
	note := func(at, trigger string) {
		t.Helper()
		b := f.binding()
		b.CompactingAt, b.CompactTrigger = at, trigger
		if err := ctxwatch.SaveBinding(f.dir, "worker-1", b); err != nil {
			t.Fatal(err)
		}
	}
	note("2026-01-01T10:04:30Z", "manual")
	f.write(300, "2026-01-01T10:05:00Z")
	f.now = f.now.Add(time.Minute)
	r.tick(context.Background(), reportRequest)
	if got := e.last(); got["compactions"] != int64(1) || got["trigger"] != "manual" {
		t.Fatalf("%+v", got)
	}
	// A note from long before is another compaction's: no trigger.
	f.write(300, "2026-01-01T11:00:00Z")
	f.now = f.now.Add(time.Minute)
	r.tick(context.Background(), reportRequest)
	if got := e.last(); got["compactions"] != int64(2) || got["trigger"] != nil {
		t.Fatalf("%+v", got)
	}
}

// R105-L1: compaction times compare as times, whatever their written form.
func TestReportContextComparesCompactionTimes(t *testing.T) {
	f := newReportFixture(t)
	b := f.binding()
	b.ReportedCompactions, b.SeenCompactionAt = 1, "2026-01-01T09:05:00-01:00"
	if err := ctxwatch.SaveBinding(f.dir, "worker-1", b); err != nil {
		t.Fatal(err)
	}
	f.write(300, "2026-01-01T10:05:00Z")
	e := &fakeReports{}
	r := f.reporter(e)
	defer r.release()
	r.tick(context.Background(), reportRequest)
	if got := e.last(); got["compactions"] != int64(1) {
		t.Fatalf("the same compaction counted again: %+v", got)
	}
}
