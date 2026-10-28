package bus

import (
	"context"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// Every code path that opens a dispatcher case notifies cb_case in the same
// function, so cases/next is woken (MESSAGE-QUEUE-DESIGN §4): a new insert
// fails here until it does.
func TestEveryCaseInsertNotifies(t *testing.T) {
	insert := regexp.MustCompile(`(?i)\binsert\s+into\s+dispatcher_cases\b`)
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	found := 0
	for _, path := range files {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		src, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		for _, at := range insert.FindAllIndex(src, -1) {
			found++
			start := strings.LastIndex(string(src[:at[0]]), "\nfunc ")
			end := strings.Index(string(src[at[0]:]), "\nfunc ")
			body := string(src[start+1:])
			if end >= 0 {
				body = string(src[start+1 : at[0]+end])
			}
			if !strings.Contains(body, "caseNotify(ctx, tx, pid)") {
				t.Errorf("%s:%d opens a case without caseNotify", path, 1+strings.Count(string(src[:at[0]]), "\n"))
			}
		}
	}
	if found < 2 {
		t.Fatalf("found %d case inserts; the guard is looking in the wrong place", found)
	}
}

// On Postgres a committed case notifies cb_case with its project's ID; a
// rolled-back one notifies no one.
func TestPostgresCaseNotify(t *testing.T) {
	dsn := os.Getenv("CLAWBRIDGE_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("reviewer supplies disposable Postgres DSN")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	s, err := OpenPostgres(ctx, dsn, "owner@case-notify.example")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	listener, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close(context.Background())
	if _, err = listener.Exec(ctx, "LISTEN "+CaseChannel); err != nil {
		t.Fatal(err)
	}
	pid := newID("prj_")
	notify := func(commit bool) {
		raw, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		tx := &transaction{Tx: raw, postgres: true}
		if err = caseNotify(ctx, tx, pid); err != nil {
			t.Fatal(err)
		}
		if commit {
			err = tx.Commit()
		} else {
			err = tx.Rollback()
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	notify(false)
	notify(true)
	// Other tests share the database: skip anything not this project's.
	for {
		n, err := listener.WaitForNotification(ctx)
		if err != nil {
			t.Fatal("no notification", err)
		}
		if n.Payload == pid {
			break
		}
	}
	quiet, stop := context.WithTimeout(ctx, 300*time.Millisecond)
	defer stop()
	for {
		n, err := listener.WaitForNotification(quiet)
		if err != nil {
			break
		}
		if n.Payload == pid {
			t.Fatal("the rolled-back case notified too")
		}
	}
}
