package linearboard

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"clawbridge/internal/bus"
	"clawbridge/internal/linearconn"
	"clawbridge/internal/linearprovider"
)

const teamID = "11111111-2222-3333-4444-555555555555"
const otherTeamID = "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"
const issueID = "22222222-2222-3333-4444-555555555555"

type fakeAuthority struct {
	binding    bus.TaskboardBinding
	calls      int
	deny       bool
	accesses   int
	onAccess   func(int)
	failAccess int
	comments   fakeCommentLedger
}

func (a *fakeAuthority) Execute(_ context.Context, r bus.Request) (bus.Result, error) {
	a.calls++
	if r.Operation == "taskboard_access" {
		a.accesses++
		if a.onAccess != nil {
			a.onAccess(a.accesses)
		}
		if a.accesses == a.failAccess {
			return bus.Result{}, errors.New("authority unavailable")
		}
		return bus.Result{Source: "local-authority", Data: bus.TaskboardAccess{WorkspaceID: a.binding.WorkspaceID, ProjectID: a.binding.ProjectID, SessionID: "ses", SessionName: "worker-1", CanWrite: !a.deny, ProjectRole: "member"}}, nil
	}
	if strings.HasPrefix(r.Operation, "linear_comment") || strings.HasPrefix(r.Operation, "comment_watch") || strings.HasPrefix(r.Operation, "marker_") {
		return a.comments.execute(r)
	}
	if r.Operation != "taskboard_get" {
		return bus.Result{}, errors.New("unexpected authority write")
	}
	return bus.Result{Source: "local-authority", Data: a.binding}, nil
}

type fakeConnections struct{}

func (fakeConnections) Status(context.Context, string) (linearconn.Profile, error) {
	return linearconn.Profile{ID: "profile-1", Name: "test", Access: linearconn.Write}, nil
}
func (fakeConnections) HTTPClient(context.Context, string) (*http.Client, error) {
	return &http.Client{}, nil
}

type fakeProvider struct {
	writes, reads int
	// bodies are the comment bodies posted, in order.
	bodies []string
	// commentErr fails every comment; nothing is recorded as posted.
	commentErr         error
	foreign            bool
	incomplete         bool
	nextCursor         string
	failure            bool
	afterWrite         func()
	revisionDuringRead func()
}

func (p *fakeProvider) Close() error { return nil }
func (p *fakeProvider) GetTeam(_ context.Context, id string) (linearprovider.Team, error) {
	return linearprovider.Team{ID: id, Name: "Team"}, nil
}
func (p *fakeProvider) GetProject(_ context.Context, id string) (linearprovider.Project, error) {
	return linearprovider.Project{ID: id, TeamIDs: []string{teamID}}, nil
}
func (p *fakeProvider) Issues(context.Context, linearprovider.IssueQuery) (linearprovider.Page[linearprovider.Issue], error) {
	p.reads++
	if p.revisionDuringRead != nil {
		p.revisionDuringRead()
	}
	i := linearprovider.Issue{ID: issueID, TeamID: teamID, Title: "task"}
	if p.foreign {
		i.TeamID = otherTeamID
	}
	return linearprovider.Page[linearprovider.Issue]{Items: []linearprovider.Issue{i}, Complete: !p.incomplete, NextCursor: p.nextCursor}, nil
}
func (p *fakeProvider) GetIssue(context.Context, string) (linearprovider.Issue, error) {
	p.reads++
	i := linearprovider.Issue{ID: issueID, TeamID: teamID, Title: "task"}
	if p.foreign {
		i.TeamID = otherTeamID
	}
	return i, nil
}
func (p *fakeProvider) Statuses(context.Context, string) ([]linearprovider.Status, error) {
	return []linearprovider.Status{{ID: issueID, TeamID: teamID}}, nil
}
func (p *fakeProvider) Create(context.Context, linearprovider.CreateInput) (linearprovider.Issue, error) {
	p.writes++
	if p.afterWrite != nil {
		p.afterWrite()
	}
	if p.failure {
		return linearprovider.Issue{}, errors.New("private raw transport token")
	}
	return linearprovider.Issue{ID: issueID, TeamID: teamID, Identifier: "TEST-1"}, nil
}
func (p *fakeProvider) Update(context.Context, linearprovider.UpdateInput) (linearprovider.Issue, error) {
	return p.Create(context.Background(), linearprovider.CreateInput{})
}
func (p *fakeProvider) Comment(_ context.Context, _ string, body string) (linearprovider.Receipt, error) {
	p.writes++
	if p.commentErr != nil {
		return linearprovider.Receipt{}, p.commentErr
	}
	p.bodies = append(p.bodies, body)
	return linearprovider.Receipt{ID: issueID, IssueID: issueID}, nil
}
func testService(t *testing.T) (*Service, *fakeAuthority, *fakeProvider, bus.Request) {
	t.Helper()
	authority := &fakeAuthority{binding: bus.TaskboardBinding{WorkspaceID: "ws", ProjectID: "prj", Configured: true, Revision: 1, UpdatedAt: "2026-10-22T00:00:00Z", Provider: "linear", LinearTeamIDs: []string{teamID}, LinearDefaultTeamID: teamID}}
	provider := &fakeProvider{}
	service := New(Config{Authority: "local-test-authority", Executor: authority, Connections: fakeConnections{}, ProfileID: "profile-1", LedgerDirectory: filepath.Join(t.TempDir(), "ledger"), Open: func(context.Context, *http.Client, bool) (Provider, error) { return provider, nil }})
	return service, authority, provider, bus.Request{Workspace: "ws", Project: "prj", Session: "ses", Args: map[string]any{"instance": "private-instance"}}
}
func TestLinearLedgerAcceptedRetryNeverDispatchesAndConflictFails(t *testing.T) {
	service, _, provider, scope := testService(t)
	title := "task"
	in := WriteInput{Operation: "create", OperationID: "create-1", Title: &title}
	first, err := service.Write(context.Background(), scope, in)
	if err != nil {
		t.Fatal(err)
	}
	service.cfg.Open = func(context.Context, *http.Client, bool) (Provider, error) {
		t.Fatal("accepted retry opened provider")
		return nil, nil
	}
	service.cfg.Connections = nil // Recorded retries need no keyring/provider connection.
	again, err := service.Write(context.Background(), scope, in)
	if err != nil || !reflect.DeepEqual(first, again) || provider.writes != 1 {
		t.Fatal(first, again, err, provider.writes)
	}
	title = "changed"
	if _, err = service.Write(context.Background(), scope, in); err == nil {
		t.Fatal("changed retry accepted")
	}
	entries, err := os.ReadDir(service.cfg.LedgerDirectory)
	if err != nil || len(entries) != 1 {
		t.Fatal(entries, err)
	}
	files, err := os.ReadDir(filepath.Join(service.cfg.LedgerDirectory, entries[0].Name()))
	if err != nil || len(files) != 1 {
		t.Fatal("temporary/lock file retained", files, err)
	}
	raw, _ := os.ReadFile(filepath.Join(service.cfg.LedgerDirectory, entries[0].Name(), files[0].Name()))
	if bytes.Contains(raw, []byte("private-instance")) || bytes.Contains(raw, []byte("task")) {
		t.Fatal("ledger leaked actor token or body")
	}
}
func TestLinearUncertainWriteAndRevisionChangeNeverReplay(t *testing.T) {
	for _, changed := range []bool{false, true} {
		service, a, p, scope := testService(t)
		p.failure = !changed
		if changed {
			p.afterWrite = func() { a.binding.Revision++ }
		}
		title := "task"
		in := WriteInput{Operation: "create", OperationID: "uncertain", Title: &title}
		if _, err := service.Write(context.Background(), scope, in); err == nil {
			t.Fatal("uncertain write accepted")
		}
		before := p.writes
		if _, err := service.Write(context.Background(), scope, in); err == nil || p.writes != before {
			t.Fatal("uncertain write replayed", err, p.writes)
		}
	}
}
func TestLinearProviderErrorStaysUncertainWhenBindingChanges(t *testing.T) {
	service, a, p, scope := testService(t)
	p.failure = true
	p.afterWrite = func() { a.binding.Revision++ }
	title := "task"
	in := WriteInput{Operation: "create", OperationID: "lost-response", Title: &title}
	receipt, err := service.Write(context.Background(), scope, in)
	var coded *ReceiptError
	if receipt != nil || err == nil || errors.As(err, &coded) || !strings.Contains(err.Error(), "outcome uncertain") || !strings.Contains(err.Error(), "binding also changed") {
		t.Fatal("binding change hid an uncertain provider outcome", receipt, err)
	}
	p.afterWrite = nil
	a.binding.Revision--
	if _, err = service.Write(context.Background(), scope, in); err == nil || !strings.Contains(err.Error(), "uncertain") || p.writes != 1 {
		t.Fatal("uncertain operation was not left pending", err, p.writes)
	}
}
func TestLinearLedgerReleasesOnlyNeverDispatchedFailures(t *testing.T) {
	l := ledger{dir: filepath.Join(t.TempDir(), "ledger")}
	path := filepath.Join(l.dir, digest("ns"), digest("op")+".json")
	refused := errors.New("actor changed before dispatch")
	pendingSeen := false
	_, err := l.write("ns", "op", "payload", func() (any, error) {
		_, e := os.Stat(path)
		pendingSeen = e == nil
		return nil, notDispatched(refused)
	})
	if err != refused || !pendingSeen {
		t.Fatal("pre-dispatch refusal lost or not durably pending during dispatch", err, pendingSeen)
	}
	if _, e := os.Stat(path); !os.IsNotExist(e) {
		t.Fatal("never-dispatched record retained", e)
	}
	if _, err = l.write("ns", "drift", "payload", func() (any, error) { return nil, refusedBeforeDispatch(refused) }); err == nil || !strings.Contains(err.Error(), "refused before dispatch; nothing was sent: actor changed before dispatch; re-read and submit a new operation_id") {
		t.Fatal("drift was not a terminal refusal", err)
	}
	again, err := l.write("ns", "drift", "payload", func() (any, error) { t.Fatal("terminal refusal redispatched"); return nil, nil })
	if again != nil || err == nil || !strings.Contains(err.Error(), "refused before dispatch; nothing was sent: actor changed") {
		t.Fatal("recorded refusal not returned", again, err)
	}
	// Only the closure's own top-level marker is honoured; a marker nested
	// inside another error, or any plain error, stays uncertain.
	for _, failure := range []error{errors.Join(errors.New("provider"), notDispatched(refused)), errors.Join(errors.New("provider"), refusedBeforeDispatch(refused)), fmt.Errorf("wrapped: %w", refusedBeforeDispatch(refused)), errors.New("provider")} {
		if _, err = l.write("ns", "op", "payload", func() (any, error) { return nil, failure }); err == nil || !strings.Contains(err.Error(), "outcome uncertain") {
			t.Fatal("dispatched failure released", err)
		}
		if _, err = l.write("ns", "op", "payload", func() (any, error) { t.Fatal("pending replayed"); return nil, nil }); err == nil || !strings.Contains(err.Error(), "uncertain") {
			t.Fatal("pending operation not refused", err)
		}
		_ = os.Remove(path)
	}
}
func TestLinearPreDispatchFailureClassification(t *testing.T) {
	title := "task"
	// A failed authority read learned nothing: the same ID may be retried.
	service, a, p, scope := testService(t)
	a.failAccess = 2
	in := WriteInput{Operation: "create", OperationID: "authority-read-failure", Title: &title}
	if _, err := service.Write(context.Background(), scope, in); err == nil || strings.Contains(err.Error(), "uncertain") || strings.Contains(err.Error(), "refused") || p.writes != 0 {
		t.Fatal("transient authority failure misclassified", err, p.writes)
	}
	if receipt, err := service.Write(context.Background(), scope, in); err != nil || receipt == nil || p.writes != 1 {
		t.Fatal("transiently refused operation ID was burned", receipt, err, p.writes)
	}
	// Access revoked inside the lock is drift: the same ID stays refused
	// without opening Linear; a new ID is evaluated normally.
	service, a, p, scope = testService(t)
	a.onAccess = func(n int) { a.deny = n == 2 }
	in.OperationID = "access-revoked"
	if _, err := service.Write(context.Background(), scope, in); err == nil || !strings.Contains(err.Error(), "refused before dispatch; nothing was sent") || p.writes != 0 {
		t.Fatal("access revocation was not a terminal refusal", err, p.writes)
	}
	open := service.cfg.Open
	service.cfg.Open = func(context.Context, *http.Client, bool) (Provider, error) {
		t.Fatal("terminal refusal retry opened Linear")
		return nil, nil
	}
	if _, err := service.Write(context.Background(), scope, in); err == nil || !strings.Contains(err.Error(), "refused before dispatch") || p.writes != 0 {
		t.Fatal("same-ID retry after drift was not the recorded refusal", err)
	}
	service.cfg.Open = open
	in.OperationID = "access-restored-new-id"
	if receipt, err := service.Write(context.Background(), scope, in); err != nil || receipt == nil || p.writes != 1 {
		t.Fatal("new operation ID was not evaluated normally", receipt, err, p.writes)
	}
}
func TestLinearScopeAndCursorRevisionFence(t *testing.T) {
	service, a, p, scope := testService(t)
	p.foreign = true
	if _, err := service.Issue(context.Background(), scope, issueID); err == nil {
		t.Fatal("foreign issue exposed")
	}
	p.foreign = false
	p.revisionDuringRead = func() { a.binding.Revision++ }
	if _, err := service.Issues(context.Background(), scope, 10, ""); err == nil {
		t.Fatal("changed binding read exposed")
	}
	p.revisionDuringRead = nil
	before := p.reads
	if _, err := service.Issues(context.Background(), scope, 10, "invalid"); err == nil || p.reads != before {
		t.Fatal("invalid scope cursor dispatched")
	}
}

func TestLinearWriteRoleFenceAndCanonicalRetryNamespace(t *testing.T) {
	s, a, p, scope := testService(t)
	title := "task"
	in := WriteInput{Operation: "create", OperationID: "stable-operation", Title: &title}
	a.deny = true
	if _, err := s.Write(context.Background(), scope, in); err == nil || p.writes != 0 {
		t.Fatal("reader dispatched", err)
	}
	a.deny = false
	if _, err := s.Write(context.Background(), scope, in); err != nil {
		t.Fatal(err)
	}
	scope.Session = "display-alias" // Authority still returns the same immutable actor.
	if _, err := s.Write(context.Background(), scope, in); err != nil || p.writes != 1 {
		t.Fatal("actor alias changed retry namespace", err)
	}
	a.binding.Revision++
	if _, err := s.Write(context.Background(), scope, in); err == nil || p.writes != 1 {
		t.Fatal("new revision reused operation", err)
	}
}

func TestLinearBindingChangedPreservesKnownReceipt(t *testing.T) {
	s, a, p, scope := testService(t)
	title := "task"
	in := WriteInput{Operation: "create", OperationID: "known-receipt", Title: &title}
	p.afterWrite = func() { a.binding.Revision++ }
	receipt, err := s.Write(context.Background(), scope, in)
	var result *ReceiptError
	if receipt == nil || !errors.As(err, &result) || result.Outcome != "binding_changed" {
		t.Fatal("known accepted effect was lost", receipt, err)
	}
	// Restore the old revision only to inspect its recorded result, never redispatch.
	a.binding.Revision--
	again, err := s.Write(context.Background(), scope, in)
	if !reflect.DeepEqual(receipt, again) || !errors.As(err, &result) || p.writes != 1 {
		t.Fatal("recorded receipt/outcome lost", again, err, p.writes)
	}
}

func TestLinearAcceptedReceiptSurvivesLedgerSaveFailure(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "ledger")
	l := ledger{dir: dir}
	namespace := "canonical-scope"
	operation := "accepted-save-failure"
	path := filepath.Join(dir, digest(namespace), digest(operation)+".json")
	known := map[string]any{"id": issueID}
	receipt, err := l.write(namespace, operation, "payload", func() (any, error) {
		if err := os.Rename(path, path+".pending"); err != nil {
			t.Fatal(err)
		}
		if err := os.Mkdir(path, 0700); err != nil {
			t.Fatal(err)
		}
		return known, nil
	})
	var outcome *ReceiptError
	if !reflect.DeepEqual(receipt, known) || !errors.As(err, &outcome) || outcome.Outcome != "accepted_ledger_unrecorded" {
		t.Fatal(receipt, err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(path+".pending", path); err != nil {
		t.Fatal(err)
	}
	_, err = l.write(namespace, operation, "payload", func() (any, error) { t.Fatal("pending result replayed"); return nil, nil })
	if err == nil {
		t.Fatal("pending accepted after failed save")
	}
}

func TestLinearLinkPreviewShowsDefaultAndNeverMutates(t *testing.T) {
	s, _, p, scope := testService(t)
	data, err := s.Link(context.Background(), scope, LinkInput{TeamIDs: []string{teamID}, DefaultTeamID: teamID, TeamBoard: true, ExpectedRevision: 1})
	if err != nil {
		t.Fatal(err)
	}
	preview, ok := data.(LinkPreview)
	if !ok || preview.DefaultTeamID != teamID || !preview.TeamBoard || p.writes != 0 {
		t.Fatal(data, p.writes)
	}
}

func TestLinearBoardNeverInventsProviderExhaustion(t *testing.T) {
	for _, teams := range [][]string{{teamID}, {teamID, otherTeamID}} {
		s, a, p, scope := testService(t)
		a.binding.LinearTeamIDs = teams
		p.incomplete = true
		page, err := s.Issues(context.Background(), scope, 10, "")
		if err == nil || page.Complete || len(page.Items) != 0 || page.NextCursor != "" {
			t.Fatal("unknown provider page became exhaustive/advanced", page, err)
		}
		// A partial page with a usable cursor remains partial on this same team.
		p.nextCursor = "provider-next"
		page, err = s.Issues(context.Background(), scope, 10, "")
		if err != nil || page.Complete || page.NextCursor == "" || len(page.Items) != 1 {
			t.Fatal("valid partial page lost continuation", page, err)
		}
		p.incomplete = false
		if _, err = s.Issues(context.Background(), scope, 10, ""); err == nil {
			t.Fatal("contradictory continuation accepted")
		}
	}
}

type countingProvider struct{ sent uint64 }

func (c *countingProvider) Sent() uint64 { return c.sent }

func TestLinearWriteThatSentNothingIsReleasedAndAnyOtherStaysUncertain(t *testing.T) {
	l := ledger{dir: filepath.Join(t.TempDir(), "ledger")}
	post := func(e error) error { return e }
	rateLimited := &linearprovider.Error{Kind: "rate_limited", Status: 429, RetryAfter: "30", NotSent: true}
	p := &countingProvider{sent: 4}
	_, err := l.write("ns", "op", "payload", func() (any, error) {
		before, known := sentCount(p)
		return nil, writeFailure(p, before, known, rateLimited, post)
	})
	if err == nil || err.Error() != "nothing was sent to Linear (rate_limited, HTTP 429, retry after 30); retry with the same operation_id" {
		t.Fatal(err)
	}
	// Released: the same operation_id dispatches again.
	rejected := &linearprovider.Error{Kind: "provider_rejected", LinearCode: "RATELIMITED"}
	_, err = l.write("ns", "op", "payload", func() (any, error) {
		before, known := sentCount(p)
		p.sent++
		return nil, writeFailure(p, before, known, rejected, post)
	})
	if err == nil || !strings.Contains(err.Error(), "outcome uncertain (provider_rejected, Linear code RATELIMITED)") {
		t.Fatal("a sent write must stay uncertain", err)
	}
	_, err = l.write("ns", "op", "payload", func() (any, error) { t.Fatal("pending operation redispatched"); return nil, nil })
	if err == nil || !strings.Contains(err.Error(), "operation outcome uncertain (provider_rejected, Linear code RATELIMITED)") {
		t.Fatal("pending record lost its safe reason", err)
	}
	// A provider that cannot count, or a failure that is not the provider's,
	// stays uncertain without a code.
	_, err = l.write("ns", "op2", "payload", func() (any, error) {
		return nil, writeFailure(struct{}{}, 0, false, rateLimited, post)
	})
	if err == nil || !strings.Contains(err.Error(), "outcome uncertain (rate_limited, HTTP 429, retry after 30)") {
		t.Fatal("an uncounted provider must stay uncertain", err)
	}
	if _, err = l.write("ns", "op3", "payload", func() (any, error) { return nil, errors.New("receipt mismatch") }); err == nil || !strings.Contains(err.Error(), "outcome uncertain, inspect") {
		t.Fatal(err)
	}
}
