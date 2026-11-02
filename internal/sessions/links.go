package sessions

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"time"
)

// Agent kinds a link can carry.
const (
	KindClaudeCode = "claude-code"
	KindCodex      = "codex"
)

const (
	linkVersion  = 1
	maxLinkBytes = 64 << 10
)

// Link is what an agent's SessionStart hook records about its process. It
// holds no messaging socket or token. Any same-user process can write one,
// so it guards against mistakes (wrong PID, stale file), not hostile code
// (design §2.4, C2).
type Link struct {
	Agent          ProcID `json:"agent"`
	Kind           string `json:"kind"`
	ConversationID string `json:"conversation_id"`
	Source         string `json:"source,omitempty"`
	CWD            string `json:"cwd,omitempty"`
	ProjectDir     string `json:"project_dir,omitempty"`
	// Home is the CLAWBRIDGE_HOME the hook saw, or "".
	Home           string    `json:"home,omitempty"`
	TTY            string    `json:"tty,omitempty"`
	PaneHandle     string    `json:"pane_handle,omitempty"`
	EnvSessionHint string    `json:"env_session_hint,omitempty"`
	At             time.Time `json:"at"`
}

type linkFile struct {
	V int `json:"v"`
	Link
}

var (
	// ErrNoLink is no link for that process (yet).
	ErrNoLink = errors.New("no agent link")
	// ErrLinkMismatch is a link file whose content names another process.
	ErrLinkMismatch = errors.New("agent link is for a different process instance")
)

// linkPollEvery is how often WaitLink looks again; tests shorten it.
var linkPollEvery = 25 * time.Millisecond

// now is the clock; tests replace it.
var now = time.Now

func linkFileName(p ProcID) string {
	return strconv.Itoa(p.PID) + "-" + strconv.FormatInt(p.Start, 10) + ".json"
}

// WriteLink writes the link for l.Agent atomically, 0600, replacing an
// earlier one (a /clear overwrites it with the new conversation). At is set
// when zero.
func WriteLink(l Link) error {
	if l.Agent.PID <= 0 || l.Agent.Start <= 0 {
		return errors.New("a link needs the agent's PID and start time")
	}
	if l.Kind != KindClaudeCode && l.Kind != KindCodex {
		return fmt.Errorf("unknown agent kind %q", l.Kind)
	}
	conv, err := normConv(l.ConversationID)
	if err != nil {
		return err
	}
	l.ConversationID = conv
	if l.At.IsZero() {
		l.At = now().UTC()
	}
	data, err := json.Marshal(linkFile{V: linkVersion, Link: l})
	if err != nil {
		return err
	}
	if len(data) > maxLinkBytes {
		return ErrTooLarge
	}
	dir, err := LinksDir()
	if err != nil {
		return err
	}
	if err := fsys.MkdirPrivate(dir); err != nil {
		return err
	}
	return fsys.WriteFile(filepath.Join(dir, linkFileName(l.Agent)), data)
}

// ReadLink is the link for process p. A file whose recorded agent is not
// exactly p (a different start time) is refused with ErrLinkMismatch. A
// missing one is ErrNoLink.
func ReadLink(p ProcID) (Link, error) {
	dir, err := LinksDir()
	if err != nil {
		return Link{}, err
	}
	data, err := fsys.ReadFile(filepath.Join(dir, linkFileName(p)), maxLinkBytes)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return Link{}, ErrNoLink
		}
		return Link{}, err
	}
	var f linkFile
	if err := decodeStrict(data, &f); err != nil {
		return Link{}, fmt.Errorf("agent link: %w", err)
	}
	if f.V != linkVersion {
		return Link{}, fmt.Errorf("agent link: unsupported version %d", f.V)
	}
	if f.Agent != p {
		return Link{}, ErrLinkMismatch
	}
	return f.Link, nil
}

// WaitLink waits up to max for p's link to appear (the SessionStart hook
// runs after the MCP server starts; the design waits 2 s). It returns
// ErrNoLink on timeout and the context's error when ctx ends first.
func WaitLink(ctx context.Context, p ProcID, max time.Duration) (Link, error) {
	deadline := time.NewTimer(max)
	defer deadline.Stop()
	tick := time.NewTicker(linkPollEvery)
	defer tick.Stop()
	for {
		l, err := ReadLink(p)
		if err == nil || !errors.Is(err, ErrNoLink) {
			return l, err
		}
		select {
		case <-ctx.Done():
			return Link{}, ctx.Err()
		case <-deadline.C:
			return Link{}, ErrNoLink
		case <-tick.C:
		}
	}
}

var linkName = regexp.MustCompile(`^([0-9]+)-([0-9]+)\.json$`)

// SweepLinks deletes the links of processes that are gone, and returns how
// many. The process is taken from the file name, and a link goes only when
// the PID is unused or now has another start time (C3). A link whose process
// can't be checked is kept.
func SweepLinks() (int, error) {
	dir, err := LinksDir()
	if err != nil {
		return 0, err
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return 0, nil
		}
		return 0, err
	}
	removed := 0
	var errs []error
	for _, e := range entries {
		m := linkName.FindStringSubmatch(e.Name())
		if m == nil || e.IsDir() {
			continue
		}
		pid, err1 := strconv.Atoi(m[1])
		start, err2 := strconv.ParseInt(m[2], 10, 64)
		if err1 != nil || err2 != nil {
			continue
		}
		if !gone(ProcID{PID: pid, Start: start}) {
			continue
		}
		if err := os.Remove(filepath.Join(dir, e.Name())); err != nil && !errors.Is(err, os.ErrNotExist) {
			errs = append(errs, err)
			continue
		}
		removed++
	}
	return removed, errors.Join(errs...)
}

// decodeStrict decodes one JSON value, refusing unknown fields and trailing
// data.
func decodeStrict(data []byte, v any) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return err
	}
	if _, err := dec.Token(); err != io.EOF {
		return errors.New("trailing data after the JSON value")
	}
	return nil
}
