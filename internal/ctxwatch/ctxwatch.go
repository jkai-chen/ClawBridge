// Package ctxwatch reads how much context a coding-agent session is using,
// from the agent's own local transcript (CONTEXT-WATCH design r2, §4.2).
//
// It reads one file it was pointed at: a regular file owned by this user,
// never through a link, and only its last 4 MiB. It extracts counts and
// times, never content: no prompt, tool output or text of any kind is kept,
// returned or logged. A format it doesn't recognise reads as unsupported,
// never as a guess.
package ctxwatch

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"
)

// The agent kinds a transcript can come from.
const (
	ClaudeCode = "claude-code"
	Codex      = "codex"
)

// Reading statuses.
const (
	StatusOK          = "ok"
	StatusUnsupported = "unsupported"
)

const (
	// maxTail is how much of the end of a transcript is read.
	maxTail = 4 << 20
	// maxLine is the longest line considered; a longer one (a large tool
	// output, say) is skipped whole.
	maxLine = 1 << 20
)

// Reading is what a transcript says about its session's context. Times are
// RFC 3339 in UTC, empty when unknown.
type Reading struct {
	Agent  string `json:"agent"`
	Status string `json:"status"`
	// Reason says why a transcript is unsupported. It is a fixed text.
	Reason string `json:"reason,omitempty"`
	// Tokens is the context in use at the last model call. It is zero, and
	// UsageAt empty, when unknown: before the first call, and after a
	// compaction until the next one.
	Tokens int64 `json:"tokens"`
	// Window is the context window when the transcript states it (Codex);
	// zero when unknown (Claude Code).
	Window int64 `json:"window,omitempty"`
	// Compactions counts the compactions within the part read; with
	// Partial, the session had at least that many.
	Compactions      int    `json:"compactions"`
	LastCompactionAt string `json:"last_compaction_at,omitempty"`
	// UsageAt is when the last model call's usage was written, empty when
	// Tokens is unknown.
	UsageAt string `json:"usage_at,omitempty"`
	// WrittenAt is the transcript's last write.
	WrittenAt string `json:"written_at"`
	// Partial says only the transcript's last 4 MiB were read.
	Partial bool `json:"partial"`
	// NotWritten says the bound transcript doesn't exist yet: a session
	// before its first model call, with no level to report.
	NotWritten bool `json:"not_written,omitempty"`
}

// parser consumes a transcript's lines, in order, and fills a Reading.
type parser interface {
	line(raw []byte)
	finish(r *Reading)
}

// Read reads the transcript at path, written by agent.
func Read(agent, path string) (Reading, error) {
	var p parser
	switch agent {
	case ClaudeCode:
		p = &claudeParser{}
	case Codex:
		p = &codexParser{}
	default:
		return Reading{}, fmt.Errorf("unknown agent kind %q: want %s or %s", agent, ClaudeCode, Codex)
	}
	f, info, err := openOwned(path)
	if err != nil {
		return Reading{}, err
	}
	defer f.Close()
	r := Reading{Agent: agent, WrittenAt: info.ModTime().UTC().Format(time.RFC3339)}
	if size := info.Size(); size > maxTail {
		r.Partial = true
		if _, err := f.Seek(size-maxTail, io.SeekStart); err != nil {
			return Reading{}, errors.New("the transcript could not be read")
		}
	}
	if err := eachLine(f, r.Partial, p.line); err != nil {
		return Reading{}, errors.New("the transcript could not be read")
	}
	p.finish(&r)
	return r, nil
}

// ReadBound reads the transcript b is bound to. A Claude Code binding the
// hook made is checked again first: the path must still resolve to itself,
// so a link put at it, or in place of a directory on it, since it was bound
// is refused. Claude Code's configuration is not checked again: the hook
// found the path under its projects directory, and a path that still
// resolves to itself can't have left it, while the reading process (doctor,
// say) may see another CLAUDE_CONFIG_DIR. Such a transcript not written yet
// reads as NotWritten, with no usage. Any other binding is read as it was
// bound.
func ReadBound(b Binding) (Reading, error) {
	if b.Agent != ClaudeCode || b.ClaudeSessionID == "" {
		return Read(b.Agent, b.Transcript)
	}
	canonical, err := canonicalTranscript(b.Transcript)
	if err != nil || filepath.Ext(canonical) != ".jsonl" {
		return Reading{}, fmt.Errorf("the bound transcript %s is not readable", b.Transcript)
	}
	if canonical != b.Transcript {
		return Reading{}, fmt.Errorf("the bound transcript %s now leads elsewhere through a link", b.Transcript)
	}
	if _, err := os.Lstat(b.Transcript); errors.Is(err, os.ErrNotExist) {
		return Reading{Agent: ClaudeCode, Status: StatusOK, NotWritten: true}, nil
	}
	return Read(ClaudeCode, b.Transcript)
}

// eachLine calls fn with each line of r, without its newline. With
// midLine, the first (partial) line is skipped. Lines over maxLine are
// skipped whole.
func eachLine(r io.Reader, midLine bool, fn func([]byte)) error {
	br := bufio.NewReaderSize(r, 64<<10)
	var line []byte
	skipping := midLine
	for {
		chunk, err := br.ReadSlice('\n')
		if !skipping {
			if len(line)+len(chunk) > maxLine {
				skipping, line = true, line[:0]
			} else {
				line = append(line, chunk...)
			}
		}
		switch {
		case err == bufio.ErrBufferFull:
			continue
		case err != nil && err != io.EOF:
			return err
		}
		if !skipping {
			if l := bytes.TrimRight(line, "\r\n"); len(l) > 0 {
				fn(l)
			}
		}
		line, skipping = line[:0], false
		if err == io.EOF {
			return nil
		}
	}
}

// openOwned opens path for reading if it is a regular file owned by this
// user, not a symbolic link, and is still that file once open.
func openOwned(path string) (*os.File, os.FileInfo, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, nil, fmt.Errorf("the transcript %s is not readable", path)
	}
	if !info.Mode().IsRegular() {
		return nil, nil, fmt.Errorf("the transcript %s is not a regular file", path)
	}
	if err := ownedByUser(info); err != nil {
		return nil, nil, fmt.Errorf("the transcript %s %s", path, err)
	}
	f, err := openNoFollow(path)
	if err != nil {
		return nil, nil, fmt.Errorf("the transcript %s is not readable", path)
	}
	opened, err := f.Stat()
	if err != nil || !os.SameFile(info, opened) {
		f.Close()
		return nil, nil, fmt.Errorf("the transcript %s changed while it was opened", path)
	}
	return f, opened, nil
}

// stamp normalises a transcript timestamp to RFC 3339 UTC, or "".
func stamp(s string) string {
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}
