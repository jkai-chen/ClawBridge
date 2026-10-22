package integration

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"clawbridge/internal/bus"
	"clawbridge/internal/linearboard"
	"clawbridge/internal/linearprovider"
)

type l1LedgerProvider struct {
	*l1SyntheticProvider
	afterWrite func()
	failWrite  bool
}

func (p *l1LedgerProvider) Create(ctx context.Context, in linearprovider.CreateInput) (linearprovider.Issue, error) {
	issue, err := p.l1SyntheticProvider.Create(ctx, in)
	if p.afterWrite != nil {
		p.afterWrite()
	}
	if p.failWrite {
		return linearprovider.Issue{}, errors.New("synthetic response lost after dispatch")
	}
	return issue, err
}

func l1LedgerBytes(t *testing.T, root string) map[string]string {
	t.Helper()
	files := map[string]string{}
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		if !strings.HasSuffix(path, ".json") {
			t.Errorf("unexpected retained ledger temporary/lock file %s", filepath.Base(path))
			return nil
		}
		b, err := os.ReadFile(path)
		if err == nil {
			rel, _ := filepath.Rel(root, path)
			files[rel] = string(b)
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return files
}

func TestL1LedgerCanonicalActorRetryAndContextConflict(t *testing.T) {
	f := newFixture(t)
	f.setup("w", "p")
	f.call("taskboard_set", "w", "p", "", l1MappingArgs(0))
	p := new(l1SyntheticProvider)
	cfg := l1ServiceConfig(t, f.store, p)
	scope := f.request("", "w", "p", "worker", actor("worker", nil))
	in := l1Create("canonical-once")
	first, err := linearboard.New(cfg).Write(context.Background(), scope, in)
	if err != nil {
		t.Fatal(err)
	}
	before := l1LedgerBytes(t, cfg.LedgerDirectory)
	r, err := f.store.Execute(context.Background(), f.request("taskboard_access", "w", "p", "worker", actor("worker", nil)))
	if err != nil {
		t.Fatal(err)
	}
	access, err := bus.ParseTaskboardAccess(r.Data)
	if err != nil {
		t.Fatal(err)
	}
	canonical := scope
	canonical.Workspace, canonical.Project, canonical.Session = access.WorkspaceID, access.ProjectID, access.SessionID
	second, err := linearboard.New(cfg).Write(context.Background(), canonical, in)
	if err != nil || !reflect.DeepEqual(first, second) || p.writes.Load() != 1 {
		t.Fatalf("canonical aliases replayed or lost receipt: first=%v second=%v calls=%d err=%v", first, second, p.writes.Load(), err)
	}
	if !reflect.DeepEqual(before, l1LedgerBytes(t, cfg.LedgerDirectory)) {
		t.Fatal("accepted identical retry rewrote ledger")
	}
	for _, variant := range []string{"profile", "operation", "payload", "revision"} {
		t.Run(variant, func(t *testing.T) {
			changed := cfg
			input := in
			switch variant {
			case "profile":
				changed.ProfileID = "lin_bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
			case "operation":
				input = linearboard.WriteInput{Operation: "comment", OperationID: in.OperationID, IssueID: l1IssueID, Body: "changed operation"}
			case "payload":
				title := "changed payload"
				input.Title = &title
			case "revision":
				f.call("taskboard_set", "w", "p", "", l1MappingArgs(1))
			}
			_, err := linearboard.New(changed).Write(context.Background(), scope, input)
			if err == nil || !strings.Contains(err.Error(), "operation_id") || !strings.Contains(err.Error(), "different") || p.writes.Load() != 1 {
				t.Fatalf("same key context change dispatched instead of conflict: %s calls=%d err=%v", variant, p.writes.Load(), err)
			}
			if !reflect.DeepEqual(before, l1LedgerBytes(t, cfg.LedgerDirectory)) {
				t.Fatal("conflict changed accepted record")
			}
		})
	}
}

func TestL1LedgerPostDispatchBindingChangePreservesKnownReceipt(t *testing.T) {
	f := newFixture(t)
	f.setup("w", "p")
	f.call("taskboard_set", "w", "p", "", l1MappingArgs(0))
	p := &l1LedgerProvider{l1SyntheticProvider: new(l1SyntheticProvider)}
	p.afterWrite = func() { f.call("taskboard_set", "w", "p", "", l1MappingArgs(1)) }
	cfg := l1ServiceConfig(t, f.store, p)
	scope := f.request("", "w", "p", "worker", actor("worker", nil))
	in := l1Create("changed-after-dispatch")
	receipt, err := linearboard.New(cfg).Write(context.Background(), scope, in)
	var coded interface{ Code() string }
	if receipt == nil || !errors.As(err, &coded) || coded.Code() != "binding_changed" || p.writes.Load() != 1 {
		t.Fatalf("lost truthful accepted old-revision effect: receipt=%v calls=%d err=%v", receipt, p.writes.Load(), err)
	}
	files := l1LedgerBytes(t, cfg.LedgerDirectory)
	if len(files) != 1 {
		t.Fatalf("ledger files=%d", len(files))
	}
	for _, raw := range files {
		var record struct {
			State, Outcome string
			Receipt        map[string]any
		}
		if json.Unmarshal([]byte(raw), &record) != nil || record.State != "accepted" || record.Outcome != "binding_changed" || !reflect.DeepEqual(record.Receipt, receipt) {
			t.Fatal("known old-revision receipt/outcome not durably recorded")
		}
	}
	if _, err := linearboard.New(cfg).Write(context.Background(), scope, in); err == nil || p.writes.Load() != 1 {
		t.Fatalf("changed mapping retry dispatched: calls=%d err=%v", p.writes.Load(), err)
	}
	if !reflect.DeepEqual(files, l1LedgerBytes(t, cfg.LedgerDirectory)) {
		t.Fatal("changed context retry overwrote old receipt")
	}
}

func TestL1LedgerPendingNeverAutomaticallyDispatchesAgain(t *testing.T) {
	for _, mode := range []string{"response-lost", "in-flight"} {
		t.Run(mode, func(t *testing.T) {
			f := newFixture(t)
			f.setup("w", "p")
			f.call("taskboard_set", "w", "p", "", l1MappingArgs(0))
			p := &l1LedgerProvider{l1SyntheticProvider: new(l1SyntheticProvider), failWrite: mode == "response-lost"}
			cfg := l1ServiceConfig(t, f.store, p)
			scope := f.request("", "w", "p", "worker", actor("worker", nil))
			in := l1Create("uncertain-once")
			if mode == "in-flight" {
				entered, release, done := make(chan struct{}), make(chan struct{}), make(chan error, 1)
				p.afterWrite = func() { close(entered); <-release }
				t.Cleanup(func() {
					select {
					case <-release:
					default:
						close(release)
					}
				})
				go func() { _, err := linearboard.New(cfg).Write(context.Background(), scope, in); done <- err }()
				select {
				case <-entered:
				case <-time.After(3 * time.Second):
					t.Fatal("provider did not enter dispatch")
				}
				if _, err := linearboard.New(cfg).Write(context.Background(), scope, in); err == nil || p.writes.Load() != 1 {
					t.Fatalf("locked pending operation dispatched: calls=%d err=%v", p.writes.Load(), err)
				}
				close(release)
				select {
				case err := <-done:
					if err != nil {
						t.Fatal(err)
					}
				case <-time.After(3 * time.Second):
					t.Fatal("dispatch did not settle")
				}
				if _, err := linearboard.New(cfg).Write(context.Background(), scope, in); err != nil || p.writes.Load() != 1 {
					t.Fatalf("accepted retry failed: calls=%d err=%v", p.writes.Load(), err)
				}
				return
			}
			if _, err := linearboard.New(cfg).Write(context.Background(), scope, in); err == nil {
				t.Fatal("lost response reported accepted")
			}
			before := l1LedgerBytes(t, cfg.LedgerDirectory)
			if _, err := linearboard.New(cfg).Write(context.Background(), scope, in); err == nil || p.writes.Load() != 1 {
				t.Fatalf("pending restart retried mutation: calls=%d err=%v", p.writes.Load(), err)
			}
			if !reflect.DeepEqual(before, l1LedgerBytes(t, cfg.LedgerDirectory)) {
				t.Fatal("uncertain retry changed pending record")
			}
		})
	}
}
