package linearboard

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"clawbridge/internal/bus"
)

// Uncertain issue writes (PR-B): a create, update, comment or relation the
// operation ledger holds pending may have reached Linear. The record names
// its operation ID, its operation and its project (never a title or body),
// so it can be listed back here, from this machine's ledger files and from
// the hosted server's shared ledger, and acknowledged once checked in
// Linear. An acknowledgement only hides it: a retry of the same operation
// ID still sends nothing.

// UncertainWrite is one issue write that may have reached Linear.
type UncertainWrite struct {
	OperationID string `json:"operation_id"`
	Operation   string `json:"operation"`
	// State is uncertain, or in_progress for a write still being sent.
	State  string `json:"state"`
	Reason string `json:"reason,omitempty"`
	Since  string `json:"since"`
	// Where is this_machine (this machine's ledger) or server.
	Where string `json:"where"`
}

// UncertainWrites is what this machine and the server hold for a project.
type UncertainWrites struct {
	Writes []UncertainWrite `json:"issue_writes"`
	// LocalChecked is how many of this machine's records were read; at
	// localWalkLimit the list says so and is not complete.
	LocalChecked int    `json:"local_checked"`
	LocalNote    string `json:"local_note,omitempty"`
	// OlderLocal counts pending records written before records named
	// their operation: their project is unknown, so they are counted, not
	// listed (B3).
	OlderLocal    int    `json:"older_local_records,omitempty"`
	OlderLocalHow string `json:"older_local_how,omitempty"`
	ServerNote    string `json:"server_note,omitempty"`
	ServerNext    string `json:"server_next_cursor,omitempty"`
}

// localWalkLimit bounds the ledger files one listing reads (C2).
const localWalkLimit = 500

// localRecord is one of this machine's pending records.
type localRecord struct {
	path   string
	record ledgerRecord
}

// localPending walks a ledger directory: each namespace directory, each
// record file, at most localWalkLimit files, in name order. It keeps the
// pending records not acknowledged, and reports how many it read and
// whether it stopped at the limit.
func localPending(dir string) (out []localRecord, checked int, truncated bool) {
	spaces, err := os.ReadDir(dir)
	if err != nil {
		return nil, 0, false
	}
	for _, space := range spaces {
		if !space.IsDir() || !ledgerNamespacePattern(space.Name()) {
			continue
		}
		files, err := os.ReadDir(filepath.Join(dir, space.Name()))
		if err != nil {
			continue
		}
		for _, f := range files {
			name := f.Name()
			if f.IsDir() || !strings.HasSuffix(name, ".json") || strings.HasPrefix(name, ".") {
				continue
			}
			if checked == localWalkLimit {
				return out, checked, true
			}
			checked++
			path := filepath.Join(dir, space.Name(), name)
			raw, err := os.ReadFile(path)
			if err != nil {
				continue
			}
			var r ledgerRecord
			if json.Unmarshal(raw, &r) != nil || r.Version != 1 || r.State != "pending" {
				continue
			}
			if _, err = os.Lstat(path + ".ack"); err == nil {
				continue
			}
			out = append(out, localRecord{path, r})
		}
	}
	return out, checked, false
}

func ledgerNamespacePattern(name string) bool {
	if len(name) != 64 {
		return false
	}
	for _, c := range name {
		if !strings.ContainsRune("0123456789abcdef", c) {
			return false
		}
	}
	return true
}

// uncertainState is a pending record's state: in_progress while one
// without a reason is young enough to be still sending, else uncertain.
func uncertainState(reason, updated string, now time.Time) string {
	at, err := time.Parse(time.RFC3339Nano, updated)
	if reason == "" && err == nil && now.Sub(at) < inProgressFor {
		return "in_progress"
	}
	return "uncertain"
}

// LocalUncertainWrites lists this machine's pending writes for project.
func LocalUncertainWrites(dir, project string, now time.Time) UncertainWrites {
	out := UncertainWrites{Writes: []UncertainWrite{}}
	records, checked, truncated := localPending(dir)
	out.LocalChecked = checked
	if truncated {
		out.LocalNote = fmt.Sprintf("first %d checked; this machine's list may not be complete", localWalkLimit)
	}
	for _, r := range records {
		switch {
		case r.record.OperationID == "":
			out.OlderLocal++
		case r.record.ProjectID == project:
			out.Writes = append(out.Writes, UncertainWrite{OperationID: r.record.OperationID, Operation: r.record.Operation, State: uncertainState(r.record.Reason, r.record.UpdatedAt, now), Reason: r.record.Reason, Since: r.record.UpdatedAt, Where: "this_machine"})
		}
	}
	if out.OlderLocal > 0 {
		out.OlderLocalHow = fmt.Sprintf("%d older pending records under %s don't name their operation or project (written before v0.3.9). Check Linear for writes you made before upgrading, then delete those files; a retry of their operation IDs then sends again", out.OlderLocal, dir)
	}
	return out
}

// ackLocal hides this machine's pending records of project with the
// operation ID op; the records stay pending, so a retry still sends
// nothing. One still being sent refuses the ack. It reports how many it
// acknowledged.
func ackLocal(dir, project, op string, now time.Time) (int, error) {
	records, _, _ := localPending(dir)
	var mine []localRecord
	for _, r := range records {
		if r.record.OperationID != op || r.record.ProjectID != project {
			continue
		}
		// A write still being sent has no outcome to check yet (R3).
		if uncertainState(r.record.Reason, r.record.UpdatedAt, now) == "in_progress" {
			return 0, errors.New("that issue write is still being sent; its outcome comes in a moment, so nothing was acknowledged")
		}
		mine = append(mine, r)
	}
	n := 0
	for _, r := range mine {
		f, err := os.OpenFile(r.path+".ack", os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if err != nil {
			return n, errors.New("cannot record the acknowledgement in this machine's operation ledger")
		}
		_, err = f.WriteString(now.UTC().Format(time.RFC3339Nano) + "\n")
		if e := f.Close(); err == nil {
			err = e
		}
		if err != nil {
			return n, errors.New("cannot record the acknowledgement in this machine's operation ledger")
		}
		n++
	}
	return n, nil
}

// localLedger is this service's own ledger directory, when it keeps one.
func (s *Service) localLedger() string {
	if s.cfg.Ledger != nil {
		return ""
	}
	return s.cfg.LedgerDirectory
}

func (s *Service) now() time.Time {
	if s.cfg.Now != nil {
		return s.cfg.Now()
	}
	return time.Now()
}

// UncertainIssueWrites lists the project's issue writes that may have
// reached Linear: this machine's, and the server's the caller may see.
func (s *Service) UncertainIssueWrites(ctx context.Context, scope bus.Request) (UncertainWrites, error) {
	b, e := s.BindingV2(ctx, scope)
	if e != nil {
		return UncertainWrites{}, e
	}
	now := s.now()
	out := UncertainWrites{Writes: []UncertainWrite{}}
	if dir := s.localLedger(); dir != "" {
		out = LocalUncertainWrites(dir, b.ProjectID, now)
	}
	server, next, e := s.serverUncertain(ctx, scope, "", now)
	if e != nil {
		// An older server, or one with no shared ledger, lists none.
		out.ServerNote = "the server's uncertain issue writes couldn't be read"
	}
	out.Writes = append(out.Writes, server...)
	out.ServerNext = next
	sort.SliceStable(out.Writes, func(i, j int) bool { return out.Writes[i].Since < out.Writes[j].Since })
	return out, nil
}

// serverUncertain reads one page of the server's uncertain issue writes,
// those with operation ID op when op is given.
func (s *Service) serverUncertain(ctx context.Context, scope bus.Request, op string, now time.Time) ([]UncertainWrite, string, error) {
	scope.Operation = "issue_writes_uncertain"
	scope.Args = instanceArgs(scope.Args)
	if op != "" {
		scope.Args["operation_id"] = op
	}
	r, e := s.cfg.Executor.Execute(ctx, scope)
	if e != nil {
		return nil, "", e
	}
	m, _ := r.Data.(map[string]any)
	var out []UncertainWrite
	rows, _ := m["writes"].([]any)
	if typed, ok := m["writes"].([]map[string]any); ok {
		for _, row := range typed {
			rows = append(rows, row)
		}
	}
	for _, raw := range rows {
		row, _ := raw.(map[string]any)
		id, _ := row["operation_id"].(string)
		if !operationPattern.MatchString(id) {
			continue
		}
		operation, _ := row["operation"].(string)
		reason, _ := row["reason"].(string)
		since, _ := row["updated_at"].(string)
		out = append(out, UncertainWrite{OperationID: id, Operation: operation, State: uncertainState(reason, since, now), Reason: reason, Since: since, Where: "server"})
	}
	next, _ := m["next_cursor"].(string)
	return out, next, nil
}

// AckLinearWrite is `linear writes --ack OP`: the project owner's (or, for
// an issue write, its own person's) acknowledgement of a Linear write they
// checked. An ID that names both a recorded Linear write (a project
// create, move or team add) and an issue write is refused, naming both.
func (s *Service) AckLinearWrite(ctx context.Context, scope bus.Request, op string) (any, error) {
	if !operationPattern.MatchString(op) {
		return nil, errors.New("--ack takes the write's operation ID")
	}
	if scope.Session != "" {
		return nil, errors.New("a session can't acknowledge a Linear write; the person does, in their own terminal, without --session")
	}
	b, e := s.BindingV2(ctx, scope)
	if e != nil {
		return nil, e
	}
	now := s.now()
	local := 0
	if dir := s.localLedger(); dir != "" {
		records, _, _ := localPending(dir)
		for _, r := range records {
			if r.record.OperationID == op && r.record.ProjectID == b.ProjectID {
				local++
			}
		}
	}
	server, _, serverErr := s.serverUncertain(ctx, scope, op, now)
	issueWrite := local > 0 || len(server) > 0
	recorded := false
	if listed, e := s.ProjectCreates(ctx, scope); e == nil {
		m, _ := listed.(map[string]any)
		recorded = listsOperation(m["unconfirmed"], op)
	}
	switch {
	case issueWrite && recorded:
		return nil, fmt.Errorf("operation ID %s names both a recorded Linear write (a project create, move or team add) and an issue write, so it can't tell which you checked; nothing was acknowledged; check both in Linear", op)
	case !issueWrite:
		return s.AckProjectCreate(ctx, scope, op)
	}
	out := map[string]any{"operation_id": op, "acknowledged_this_machine": 0, "acknowledged_server": 0}
	if local > 0 {
		n, e := ackLocal(s.localLedger(), b.ProjectID, op, now)
		out["acknowledged_this_machine"] = n
		if e != nil {
			return out, e
		}
	}
	if len(server) > 0 && serverErr == nil {
		scope.Operation = "issue_writes_ack"
		scope.Args = map[string]any{"operation_id": op}
		r, e := s.cfg.Executor.Execute(ctx, scope)
		if e != nil {
			return out, e
		}
		m, _ := r.Data.(map[string]any)
		out["acknowledged_server"] = m["acknowledged"]
	}
	out["note"] = "hidden from the list; a retry of the same operation ID still sends nothing"
	return out, nil
}

// listsOperation reports whether rows (a listing's []map or []any) holds
// a row with operation ID op.
func listsOperation(rows any, op string) bool {
	switch typed := rows.(type) {
	case []map[string]any:
		for _, row := range typed {
			if row["operation_id"] == op {
				return true
			}
		}
	case []any:
		for _, raw := range typed {
			if row, _ := raw.(map[string]any); row["operation_id"] == op {
				return true
			}
		}
	}
	return false
}
