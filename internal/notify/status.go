// Package notify polls authoritative metadata and emits opt-in inbox hints.
package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"time"

	"clawbridge/internal/bus"
	"clawbridge/internal/remote"
)

type Executor interface {
	Execute(context.Context, bus.Request) (bus.Result, error)
}
type Scope struct {
	WorkspaceID string `json:"workspace_id"`
	ProjectID   string `json:"project_id"`
	SessionID   string `json:"session_id"`
}
type Status struct {
	WorkspaceID    string `json:"workspace_id"`
	ProjectID      string `json:"project_id"`
	SessionID      string `json:"session_id"`
	UnreadCount    int64  `json:"unread_count"`
	LatestSequence int64  `json:"latest_sequence"`
	NewCount       int64  `json:"new_count"`
}

func (s Status) Scope() Scope { return Scope{s.WorkspaceID, s.ProjectID, s.SessionID} }

var identifier = regexp.MustCompile(`\A[A-Za-z0-9][A-Za-z0-9._-]{0,127}\z`)
var uuid = regexp.MustCompile(`\A[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}\z`)

const MaxSequence int64 = 9007199254740991

func ValidThread(s string) bool { return uuid.MatchString(s) }
func (s Scope) valid() bool {
	return identifier.MatchString(s.WorkspaceID) && identifier.MatchString(s.ProjectID) && identifier.MatchString(s.SessionID)
}

type permanentError struct{ error }

func Permanent(err error) error {
	if err == nil {
		return nil
	}
	return &permanentError{err}
}
func IsPermanent(err error) bool {
	var p *permanentError
	if errors.As(err, &p) {
		return true
	}
	var coded interface{ Code() string }
	if errors.As(err, &coded) {
		return coded.Code() != "unavailable" && coded.Code() != "rate_limited"
	}
	var httpErr *remote.HTTPError
	if errors.As(err, &httpErr) {
		return httpErr.Status < 500 && httpErr.Status != 429
	}
	return false
}

// ParseStatus rejects mirrors, unknown fields, fractional/overflow counts and
// untrusted text before it can become a resource or a host hint.
func ParseStatus(r bus.Result) (Status, error) {
	var s Status
	if r.Source != "server" && r.Source != "local-authority" {
		return s, Permanent(errors.New("notification requires authoritative status"))
	}
	b, err := json.Marshal(r.Data)
	if err != nil || len(b) > 4096 {
		return s, Permanent(errors.New("invalid inbox status"))
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(b, &fields) != nil || len(fields) != 6 {
		return s, Permanent(errors.New("incomplete inbox status"))
	}
	for _, value := range fields {
		if bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
			return s, Permanent(errors.New("null inbox status field"))
		}
	}
	d := json.NewDecoder(bytes.NewReader(b))
	d.DisallowUnknownFields()
	if d.Decode(&s) != nil || !s.Scope().valid() || s.UnreadCount < 0 || s.UnreadCount > MaxSequence || s.LatestSequence < 0 || s.LatestSequence > MaxSequence || s.NewCount < 0 || s.NewCount > s.UnreadCount || (s.UnreadCount == 0) != (s.LatestSequence == 0) {
		return Status{}, Permanent(errors.New("invalid inbox status fields"))
	}
	return s, nil
}
func Fetch(ctx context.Context, e Executor, r bus.Request) (bus.Result, Status, error) {
	r.Operation = "inbox_status"
	result, err := e.Execute(ctx, r)
	if ctx.Err() != nil {
		return bus.Result{}, Status{}, ctx.Err()
	}
	if err != nil {
		return bus.Result{}, Status{}, err
	}
	s, err := ParseStatus(result)
	return result, s, err
}
func Wait(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

type Binding struct {
	Authority    string `json:"authority"`
	Principal    string `json:"principal"`
	Workspace    string `json:"workspace"`
	Project      string `json:"project"`
	Session      string `json:"session"`
	Instance     string `json:"instance"`
	Adapter      string `json:"adapter"`
	Thread       string `json:"thread,omitempty"`
	CodexCommand string `json:"codex_command,omitempty"`
	CodexHome    string `json:"codex_home,omitempty"`
	// Command is the command adapter's argv, its program resolved to an
	// absolute path.
	Command []string `json:"command,omitempty"`
}
type Config struct {
	Binding        Binding
	Executor       Executor
	StatePath      string
	Interval       time.Duration
	Once           bool
	Output         io.Writer
	Diagnostics    io.Writer
	CodexCommand   string
	Runner         Runner
	Wait           func(context.Context, time.Duration) error
	RequestTimeout time.Duration
	HostTimeout    time.Duration
	// ContextDir holds the binding records; when set, the watcher reports
	// its session's context if no other process does (ReportContext).
	ContextDir string
	// CommandRunner runs the command adapter; nil runs the process.
	CommandRunner CommandRunner
}

func (c Config) validate() error {
	b := c.Binding
	if c.Executor == nil || b.Authority == "" || b.Principal == "" || len(b.Authority) > 4096 || len(b.Principal) > 1024 || !identifier.MatchString(b.Workspace) || !identifier.MatchString(b.Project) || !identifier.MatchString(b.Session) || b.Instance == "" || len(b.Instance) > 1024 {
		return errors.New("notification requires an explicit authority, identity, scope and exact instance")
	}
	if c.Interval < time.Second || c.Interval > 300*time.Second {
		return errors.New("notification interval must be 1–300 seconds")
	}
	if b.Adapter != "json" && b.Adapter != "codex" && b.Adapter != "command" {
		return errors.New("notification adapter must be json, codex or command")
	}
	if (b.Adapter == "command") != (len(b.Command) > 0) {
		return errors.New("the command adapter, and only it, takes a command")
	}
	if b.Adapter == "command" && (b.Thread != "" || b.CodexCommand != "" || b.CodexHome != "" || c.CodexCommand != "" || c.Runner != nil) {
		return errors.New("command adapter rejects Codex-only options")
	}
	if b.Adapter == "codex" && !ValidThread(b.Thread) {
		return errors.New("codex requires a canonical UUID thread")
	}
	if b.Adapter == "json" && (b.Thread != "" || b.CodexCommand != "" || b.CodexHome != "" || c.CodexCommand != "" || c.Runner != nil) {
		return errors.New("JSON adapter rejects Codex-only options")
	}
	return nil
}
func validateAfter(s Status, after int64) error {
	if (s.LatestSequence <= after && s.NewCount != 0) || (s.LatestSequence > after && s.NewCount == 0) {
		return Permanent(fmt.Errorf("inbox status cursor/count mismatch"))
	}
	return nil
}
