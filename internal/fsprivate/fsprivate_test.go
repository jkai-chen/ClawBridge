package fsprivate

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func TestRenameRetryTriesAboutTenTimesThenFails(t *testing.T) {
	blocked := errors.New("blocked")
	calls, slept := 0, time.Duration(0)
	err := renameRetry(func() error { calls++; return blocked }, func(error) bool { return true }, func(d time.Duration) { slept += d })
	if !errors.Is(err, blocked) || calls != renameTries {
		t.Fatal(err, calls)
	}
	if slept < 800*time.Millisecond || slept > 1500*time.Millisecond {
		t.Fatalf("waited %v, want about one second", slept)
	}
}

func TestRenameRetrySucceedsOnceUnblockedAndStopsOnOtherErrors(t *testing.T) {
	n := 0
	err := renameRetry(func() error {
		if n++; n < 4 {
			return errors.New("blocked")
		}
		return nil
	}, func(error) bool { return true }, func(time.Duration) {})
	if err != nil || n != 4 {
		t.Fatal(err, n)
	}
	other, calls := errors.New("other"), 0
	err = renameRetry(func() error { calls++; return other }, func(error) bool { return false }, func(time.Duration) { t.Fatal("slept") })
	if !errors.Is(err, other) || calls != 1 {
		t.Fatal(err, calls)
	}
}

func TestWriteAtomicReplacesWholeAndLeavesNoTemporaryFile(t *testing.T) {
	d := t.TempDir()
	if err := MakePrivate(d); err != nil {
		t.Fatal(err)
	}
	for _, v := range []string{"one", "two"} {
		if err := WriteAtomic(d, ".x-", "x.sealed", []byte(v)); err != nil {
			t.Fatal(err)
		}
	}
	if b, err := os.ReadFile(filepath.Join(d, "x.sealed")); err != nil || string(b) != "two" {
		t.Fatal(string(b), err)
	}
	if err := CheckPrivate(filepath.Join(d, "x.sealed"), false); err != nil {
		t.Fatal(err)
	}
	if ents, _ := os.ReadDir(d); len(ents) != 1 {
		t.Fatal("left files behind:", len(ents))
	}
	var we *WriteError
	if err := WriteAtomic(filepath.Join(d, "absent"), ".x-", "x", nil); !errors.As(err, &we) || we.Stage != StageCreate {
		t.Fatal(err)
	}
}

func TestCreateExclusiveKeepsTheFirstWriter(t *testing.T) {
	d := t.TempDir()
	if err := MakePrivate(d); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(d, "k")
	if err := CreateExclusive(p, []byte("first")); err != nil {
		t.Fatal(err)
	}
	if err := CreateExclusive(p, []byte("second")); !errors.Is(err, os.ErrExist) {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(p); string(b) != "first" {
		t.Fatal(string(b))
	}
	if ents, _ := os.ReadDir(d); len(ents) != 1 {
		t.Fatal("left files behind:", len(ents))
	}
	if err := CheckPrivate(p, false); err != nil {
		t.Fatal(err)
	}
}

func TestOnWindowsDrive(t *testing.T) {
	d := t.TempDir()
	resolved, err := filepath.EvalSymlinks(d)
	if err != nil {
		t.Fatal(err)
	}
	drives := resolved + string(filepath.Separator)
	if !OnWindowsDrive(filepath.Join(d, "a", "b"), drives, false) {
		t.Fatal("a path under the drives was allowed")
	}
	if OnWindowsDrive(filepath.Join(d, "a"), drives, true) {
		t.Fatal("refused on Windows itself")
	}
	if OnWindowsDrive("/home/example/.clawbridge", drives, false) {
		t.Fatal("refused a path elsewhere")
	}
}

// Many writers race to make one file: exactly one wins, the others are told
// it exists, and the file holds the winner's whole content.
func TestCreateExclusiveHasOneWinnerUnderRace(t *testing.T) {
	d := t.TempDir()
	if err := MakePrivate(d); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(d, "k")
	const n = 16
	errs := make([]error, n)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			errs[i] = CreateExclusive(p, []byte(fmt.Sprintf("writer-%02d", i)))
		}()
	}
	close(start)
	wg.Wait()
	winner := -1
	for i, err := range errs {
		switch {
		case err == nil:
			if winner >= 0 {
				t.Fatal("two writers won")
			}
			winner = i
		case !errors.Is(err, os.ErrExist):
			t.Fatal(i, err)
		}
	}
	if winner < 0 {
		t.Fatal("no writer won")
	}
	if b, _ := os.ReadFile(p); string(b) != fmt.Sprintf("writer-%02d", winner) {
		t.Fatalf("the file holds %q, the winner was %d", b, winner)
	}
	if ents, _ := os.ReadDir(d); len(ents) != 1 {
		t.Fatal("left temporary files:", len(ents))
	}
}
