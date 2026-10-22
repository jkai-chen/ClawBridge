package integration

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"

	"clawbridge/internal/bus"
	"clawbridge/internal/linearboard"
	"clawbridge/internal/linearconn"
	"clawbridge/internal/linearprovider"
)

const l1IssueID = "55555555-5555-4555-8555-555555555555"
const l1StateID = "66666666-6666-4666-8666-666666666666"
const l1ProfileID = "lin_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

type l1SyntheticConnections struct{}

func (l1SyntheticConnections) Status(_ context.Context, id string) (linearconn.Profile, error) {
	return linearconn.Profile{ID: id, Name: "synthetic write connection", Access: linearconn.Write}, nil
}
func (l1SyntheticConnections) HTTPClient(context.Context, string) (*http.Client, error) {
	return &http.Client{Transport: transportFunc(func(*http.Request) (*http.Response, error) {
		return nil, errors.New("unexpected network in provider fixture")
	})}, nil
}

// No external transport or native credential implementation is constructed.
type l1SyntheticProvider struct {
	writes    atomic.Int64
	afterTeam func()
	teamHook  sync.Once
}

func (*l1SyntheticProvider) Close() error { return nil }
func (p *l1SyntheticProvider) GetTeam(_ context.Context, id string) (linearprovider.Team, error) {
	if p.afterTeam != nil {
		p.teamHook.Do(p.afterTeam)
	}
	return linearprovider.Team{ID: id, Name: "synthetic team"}, nil
}
func (*l1SyntheticProvider) GetProject(_ context.Context, id string) (linearprovider.Project, error) {
	return linearprovider.Project{ID: id, TeamIDs: []string{l1TeamA, l1TeamB}}, nil
}
func (*l1SyntheticProvider) Issues(context.Context, linearprovider.IssueQuery) (linearprovider.Page[linearprovider.Issue], error) {
	return linearprovider.Page[linearprovider.Issue]{Items: []linearprovider.Issue{}, Complete: true}, nil
}
func (*l1SyntheticProvider) GetIssue(_ context.Context, id string) (linearprovider.Issue, error) {
	return linearprovider.Issue{ID: id, TeamID: l1TeamA, Title: "synthetic bound issue"}, nil
}
func (*l1SyntheticProvider) Statuses(_ context.Context, team string) ([]linearprovider.Status, error) {
	return []linearprovider.Status{{ID: l1StateID, TeamID: team, Name: "synthetic state"}}, nil
}
func (p *l1SyntheticProvider) Create(_ context.Context, in linearprovider.CreateInput) (linearprovider.Issue, error) {
	n := p.writes.Add(1)
	return linearprovider.Issue{ID: fmt.Sprintf("77777777-7777-4777-8777-%012d", n), Identifier: fmt.Sprintf("SYN-%d", n), TeamID: in.TeamID, ProjectID: in.ProjectID, Title: in.Title}, nil
}
func (p *l1SyntheticProvider) Update(_ context.Context, in linearprovider.UpdateInput) (linearprovider.Issue, error) {
	p.writes.Add(1)
	return linearprovider.Issue{ID: in.ID, TeamID: l1TeamA, Title: "updated synthetic issue"}, nil
}
func (p *l1SyntheticProvider) Comment(_ context.Context, id, body string) (linearprovider.Receipt, error) {
	p.writes.Add(1)
	return linearprovider.Receipt{ID: l1IssueID, IssueID: id}, nil
}

func l1ServiceConfig(t *testing.T, executor linearboard.Executor, provider linearboard.Provider) linearboard.Config {
	t.Helper()
	return linearboard.Config{Authority: "synthetic-clawbridge-authority", Executor: executor, Connections: l1SyntheticConnections{}, ProfileID: l1ProfileID, LedgerDirectory: filepath.Join(t.TempDir(), "ledger"), Open: func(context.Context, *http.Client, bool) (linearboard.Provider, error) { return provider, nil }}
}
func l1ServiceFor(t *testing.T, executor linearboard.Executor, provider linearboard.Provider) *linearboard.Service {
	t.Helper()
	return linearboard.New(l1ServiceConfig(t, executor, provider))
}
func l1Create(operationID string) linearboard.WriteInput {
	title := "synthetic isolated task"
	return linearboard.WriteInput{Operation: "create", OperationID: operationID, Title: &title}
}

func TestL1NativeWriteRealStoreRetiredAndFencedActors(t *testing.T) {
	for _, mode := range []string{"retired", "moved", "fenced", "retired-during-provider-read"} {
		t.Run(mode, func(t *testing.T) {
			f := newFixture(t)
			f.setup("w", "p")
			f.call("taskboard_set", "w", "p", "", l1MappingArgs(0))
			provider := new(l1SyntheticProvider)
			service := l1ServiceFor(t, f.store, provider)
			scope := f.request("", "w", "p", "worker", actor("worker", nil))
			if _, err := service.Write(context.Background(), scope, l1Create("healthy-control")); err != nil {
				t.Fatalf("healthy native write control failed; permission probe not reached: %v", err)
			}
			if provider.writes.Load() != 1 {
				t.Fatal("healthy control did not reach injected provider")
			}
			switch mode {
			case "retired":
				f.call("retire_session", "w", "p", "worker", actor("worker", nil))
			case "moved":
				f.call("retire_session", "w", "p", "worker", actor("worker", map[string]any{"successor": "outsider"}))
			case "fenced":
				f.call("register_session", "w", "p", "worker", map[string]any{"instance": "worker-instance", "resume": true})
			case "retired-during-provider-read":
				provider.afterTeam = func() { f.call("retire_session", "w", "p", "worker", actor("worker", nil)) }
			}
			for _, in := range []linearboard.WriteInput{l1Create("denied-create"), {Operation: "update", OperationID: "denied-update", IssueID: l1IssueID, Description: func() *string { s := "denied description"; return &s }()}, {Operation: "comment", OperationID: "denied-comment", IssueID: l1IssueID, Body: "denied comment"}} {
				if _, err := service.Write(context.Background(), scope, in); err == nil {
					t.Errorf("%s actor dispatched/accepted %s", mode, in.Operation)
				}
				if provider.writes.Load() != 1 {
					t.Fatalf("%s actor reached provider mutation; calls=%d", mode, provider.writes.Load())
				}
			}
		})
	}
}

func TestL1NativeWritePostgresReaderAndAccessRevocation(t *testing.T) {
	f := newPGFixture(t)
	f.setup("p")
	f.call(pgOwner, "taskboard_set", "p", "", l1MappingArgs(0))
	for _, mode := range []string{"reader-actor", "reader-actorless", "member-actorless", "member-revoked-predispatch"} {
		t.Run(mode, func(t *testing.T) {
			name := "reader"
			actorName := name
			if mode == "reader-actorless" {
				actorName = ""
			}
			if mode == "member-actorless" || mode == "member-revoked-predispatch" {
				name = "alice"
				actorName = ""
			}
			provider := new(l1SyntheticProvider)
			executor := notificationExecutorFunc(func(ctx context.Context, r bus.Request) (bus.Result, error) {
				return f.store.ExecuteAs(ctx, pgPrincipal(name), r)
			})
			service := l1ServiceFor(t, executor, provider)
			if mode == "member-revoked-predispatch" {
				provider.afterTeam = func() { f.role("p", "alice", "reader") }
				t.Cleanup(func() { f.role("p", "alice", "member") })
			}
			args := map[string]any{}
			if actorName != "" {
				args = actor(actorName, nil)
			}
			scope := f.request("", "p", actorName, args)
			_, err := service.Write(context.Background(), scope, l1Create("real-permission-"+mode))
			if mode == "member-actorless" {
				if err != nil || provider.writes.Load() != 1 {
					t.Fatal("authorized actorless member not allowed native write", err)
				}
				return
			}
			if err == nil || provider.writes.Load() != 0 {
				t.Fatalf("CB permission/late revocation did not prevent external mutation: mode=%s calls=%d err=%v", mode, provider.writes.Load(), err)
			}
		})
	}
}
