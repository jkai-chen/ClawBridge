package sessions

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func sampleLink(p ProcID, conv string) Link {
	return Link{Agent: p, Kind: KindClaudeCode, ConversationID: conv, Source: "startup", CWD: "/work/a", ProjectDir: "/work/a", TTY: "/dev/ttys001"}
}

func TestLinkWriteRead(t *testing.T) {
	testConfig(t)
	f := newFakeProcs(t)
	p := f.add(fakeBase+1, 500)
	if err := WriteLink(sampleLink(p, convID(1))); err != nil {
		t.Fatal(err)
	}
	got, err := ReadLink(p)
	if err != nil || got.ConversationID != convID(1) || got.Agent != p || got.TTY != "/dev/ttys001" || got.At.IsZero() {
		t.Fatalf("ReadLink = %+v, %v", got, err)
	}
	dir, _ := LinksDir()
	fi, err := os.Stat(filepath.Join(dir, linkFileName(p)))
	if err != nil || runtimeMode(fi) != 0o600 {
		t.Errorf("link file mode %v, %v", fi, err)
	}
	if di, _ := os.Stat(dir); runtimeMode(di) != 0o700 {
		t.Errorf("links dir mode %v", runtimeMode(di))
	}
	// A /clear overwrites it with the new conversation.
	l := sampleLink(p, convID(2))
	l.Source = "clear"
	if err := WriteLink(l); err != nil {
		t.Fatal(err)
	}
	if got, _ := ReadLink(p); got.ConversationID != convID(2) || got.Source != "clear" {
		t.Errorf("after overwrite: %+v", got)
	}
}

func TestLinkRefusals(t *testing.T) {
	testConfig(t)
	f := newFakeProcs(t)
	p := f.add(fakeBase+1, 500)
	if _, err := ReadLink(p); !errors.Is(err, ErrNoLink) {
		t.Errorf("missing link: %v", err)
	}
	if err := WriteLink(sampleLink(p, convID(1))); err != nil {
		t.Fatal(err)
	}
	// The same PID with another start time finds no file by name...
	if _, err := ReadLink(ProcID{PID: p.PID, Start: 501}); !errors.Is(err, ErrNoLink) {
		t.Errorf("other start: %v", err)
	}
	// ...and a file moved under the wrong name is refused by its content.
	dir, _ := LinksDir()
	other := ProcID{PID: p.PID, Start: 777}
	if err := os.Rename(filepath.Join(dir, linkFileName(p)), filepath.Join(dir, linkFileName(other))); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadLink(other); !errors.Is(err, ErrLinkMismatch) {
		t.Errorf("start mismatch inside the file: %v", err)
	}
	for name, l := range map[string]Link{
		"no pid":  {Agent: ProcID{Start: 1}, Kind: KindCodex, ConversationID: convID(1)},
		"no kind": {Agent: p, Kind: "vim", ConversationID: convID(1)},
		"bad id":  {Agent: p, Kind: KindCodex, ConversationID: "../x"},
	} {
		if WriteLink(l) == nil {
			t.Errorf("%s: WriteLink should fail", name)
		}
	}
}

func TestWaitLink(t *testing.T) {
	testConfig(t)
	f := newFakeProcs(t)
	p := f.add(fakeBase+1, 500)
	old := linkPollEvery
	linkPollEvery = 5 * time.Millisecond
	defer func() { linkPollEvery = old }()

	start := time.Now()
	if _, err := WaitLink(context.Background(), p, 60*time.Millisecond); !errors.Is(err, ErrNoLink) {
		t.Fatalf("timeout: %v", err)
	}
	if time.Since(start) < 50*time.Millisecond {
		t.Error("returned before the wait ended")
	}

	go func() {
		time.Sleep(30 * time.Millisecond)
		WriteLink(sampleLink(p, convID(3)))
	}()
	l, err := WaitLink(context.Background(), p, 2*time.Second)
	if err != nil || l.ConversationID != convID(3) {
		t.Fatalf("WaitLink = %+v, %v", l, err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	q := ProcID{PID: fakeBase + 2, Start: 9}
	if _, err := WaitLink(ctx, q, time.Second); !errors.Is(err, context.Canceled) {
		t.Errorf("cancelled: %v", err)
	}
}

func TestSweepLinks(t *testing.T) {
	testConfig(t)
	f := newFakeProcs(t)
	alive := f.add(fakeBase+1, 100)
	reused := f.add(fakeBase+2, 200)
	dead := f.add(fakeBase+3, 300)
	unknown := f.add(fakeBase+4, 400)
	for i, p := range []ProcID{alive, reused, dead, unknown} {
		if err := WriteLink(sampleLink(p, convID(i+1))); err != nil {
			t.Fatal(err)
		}
	}
	f.restart(reused.PID, 201) // the PID now belongs to another process
	f.kill(dead.PID)
	f.mu.Lock()
	f.unknown[unknown.PID] = true // can't be checked
	f.mu.Unlock()

	n, err := SweepLinks()
	if err != nil || n != 2 {
		t.Fatalf("SweepLinks = %d, %v", n, err)
	}
	dir, _ := LinksDir()
	for p, want := range map[ProcID]bool{alive: true, reused: false, dead: false, unknown: true} {
		_, err := os.Stat(filepath.Join(dir, linkFileName(p)))
		if (err == nil) != want {
			t.Errorf("link %v present=%v, want %v", p, err == nil, want)
		}
	}
	// Nothing to sweep when the directory does not exist.
	testConfig(t)
	if n, err := SweepLinks(); n != 0 || err != nil {
		t.Errorf("empty sweep = %d, %v", n, err)
	}
}

func TestLinkStrictDecode(t *testing.T) {
	testConfig(t)
	f := newFakeProcs(t)
	p := f.add(fakeBase+1, 500)
	if err := WriteLink(sampleLink(p, convID(1))); err != nil {
		t.Fatal(err)
	}
	dir, _ := LinksDir()
	path := filepath.Join(dir, linkFileName(p))
	data, _ := os.ReadFile(path)
	os.WriteFile(path, append(data[:len(data)-1], []byte(`,"socket":"/x"}`)...), 0o600)
	if _, err := ReadLink(p); err == nil {
		t.Error("an unknown field (a socket) must be refused")
	}
}

func TestLinkAgentStartNameIsOptional(t *testing.T) {
	testConfig(t)
	p := ProcID{PID: fakeBase + 5, Start: 5}
	// A link written without the field parses, and reads back empty.
	if err := WriteLink(Link{Agent: p, Kind: KindClaudeCode, ConversationID: convID(1)}); err != nil {
		t.Fatal(err)
	}
	l, err := ReadLink(p)
	if err != nil || l.AgentStartName != "" {
		t.Fatalf("%+v %v", l, err)
	}
	if err := WriteLink(Link{Agent: p, Kind: KindClaudeCode, ConversationID: convID(1), AgentStartName: "web"}); err != nil {
		t.Fatal(err)
	}
	if l, err = ReadLink(p); err != nil || l.AgentStartName != "web" {
		t.Fatalf("%+v %v", l, err)
	}
}
