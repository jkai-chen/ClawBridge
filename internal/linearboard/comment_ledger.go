package linearboard

import (
	"clawbridge/internal/bus"
	"clawbridge/internal/linearprovider"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// commentIntent is one comment as the bus comment ledger records it: Body
// is the exact text posted, agent marker included.
type commentIntent struct {
	Issue, OperationID, Body, Parent, InResponseTo string
}

// sweepLimit bounds how many unsettled comments one write settles first.
const sweepLimit = 20

// sendComment posts one comment through the bus comment ledger: reserve,
// send, settle. The bus enforces the per-issue cap and the one-answer rule
// at reserve, so a refusal sends nothing. A private sidecar names the
// operation until the bus settles it, so a settle the bus missed is retried
// by a later sweep. send posts the comment and checks its receipt; it must
// classify a failure that sent nothing as notDispatched. The result is the
// comment the bus recorded this one as answering, if any.
func (s *Service) sendComment(ctx context.Context, scope bus.Request, l ledger, namespace string, c commentIntent, send func() (linearprovider.Receipt, error)) (linearprovider.Receipt, string, error) {
	if err := l.markUnsettled(namespace, c.OperationID); err != nil {
		return linearprovider.Receipt{}, "", notDispatched(err)
	}
	reserved, err := s.commentLedger(ctx, scope, "linear_comment_reserve", c.reserveArgs())
	if err != nil {
		l.dropUnsettled(namespace, c.OperationID)
		return linearprovider.Receipt{}, "", notDispatched(err)
	}
	if reserved["state"] == "posted" {
		l.dropUnsettled(namespace, c.OperationID)
		return linearprovider.Receipt{}, "", refusedBeforeDispatch(fmt.Errorf("this operation_id already posted comment %v (bus comment ledger); nothing was sent", reserved["comment_id"]))
	}
	answers, _ := reserved["in_response_to"].(string)
	r, err := send()
	if err != nil {
		if _, never := err.(notDispatchedError); never {
			s.settleComment(ctx, scope, l, namespace, c.OperationID, "", "receipt")
		}
		return r, "", err
	}
	s.settleComment(ctx, scope, l, namespace, c.OperationID, r.ID, "receipt")
	return r, answers, nil
}

func (c commentIntent) reserveArgs() map[string]any {
	sum := sha256.Sum256([]byte(c.Body))
	a := map[string]any{"issue_uuid": c.Issue, "operation_id": c.OperationID, "body_sha256": hex.EncodeToString(sum[:])}
	if c.Parent != "" {
		a["parent_id"] = c.Parent
	}
	if c.InResponseTo != "" {
		a["in_response_to"] = c.InResponseTo
	}
	return a
}

// commentLedger runs one bus comment-ledger operation as the caller's
// session.
func (s *Service) commentLedger(ctx context.Context, scope bus.Request, operation string, args map[string]any) (map[string]any, error) {
	scope.Operation = operation
	a := instanceArgs(scope.Args)
	for k, v := range args {
		a[k] = v
	}
	scope.Args = a
	result, err := s.cfg.Executor.Execute(ctx, scope)
	if err != nil {
		return nil, err
	}
	data, ok := result.Data.(map[string]any)
	if !ok {
		return nil, errors.New("invalid comment ledger response")
	}
	return data, nil
}

// settleComment records a comment's outcome on the bus: posted with its
// comment ID, or failed when nothing was sent. The sidecar is dropped once
// the bus has an answer; a bus that cannot be reached leaves it for a sweep.
func (s *Service) settleComment(ctx context.Context, scope bus.Request, l ledger, namespace, operationID, commentID, via string) {
	args := map[string]any{"operation_id": operationID, "outcome": "failed", "via": via}
	if commentID != "" {
		args["outcome"], args["comment_id"] = "posted", commentID
	}
	_, err := s.commentLedger(ctx, scope, "linear_comment_settle", args)
	var coded interface{ Code() string }
	if err == nil || errors.As(err, &coded) && (coded.Code() == "not_found" || coded.Code() == "conflict" || coded.Code() == "invalid") {
		l.dropUnsettled(namespace, operationID)
	}
}

// sweepComments settles this session's comments whose bus settle was
// missed, from the file ledger: an accepted record settles as posted, a
// refused or released one as failed. A pending record is an uncertain
// outcome and stays for the bus's body match.
func (s *Service) sweepComments(ctx context.Context, scope bus.Request, l ledger, namespace string) {
	for _, op := range l.unsettled(namespace, sweepLimit) {
		state, id, ok := l.peek(namespace, op)
		switch {
		case !ok || state == "pending":
		case state == "accepted" && linearprovider.ValidID(id):
			s.settleComment(ctx, scope, l, namespace, op, id, "file_ledger_sweep")
		case state == "refused" || state == "absent":
			s.settleComment(ctx, scope, l, namespace, op, "", "file_ledger_sweep")
		}
	}
}

// The sidecar for an operation sits beside its ledger record and holds the
// operation_id, which the record's file name only digests.
func (l ledger) sidecar(namespace, operationID string) string {
	return filepath.Join(l.dir, digest(namespace), digest(operationID)+".settle")
}

func (l ledger) markUnsettled(namespace, operationID string) error {
	path := l.sidecar(namespace, operationID)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0600)
	if err != nil {
		return errors.New("cannot record the comment for its bus settle")
	}
	if err = makePrivate(path); err == nil {
		_, err = io.WriteString(f, operationID)
	}
	if err == nil {
		err = f.Sync()
	}
	if e := f.Close(); err == nil {
		err = e
	}
	if err != nil {
		_ = os.Remove(path)
		return errors.New("cannot record the comment for its bus settle")
	}
	return syncDirectory(filepath.Dir(path))
}

func (l ledger) dropUnsettled(namespace, operationID string) {
	_ = os.Remove(l.sidecar(namespace, operationID))
}

// unsettled lists up to limit operation_ids with a sidecar, skipping any
// whose operation is in progress.
func (l ledger) unsettled(namespace string, limit int) []string {
	directory := filepath.Join(l.dir, digest(namespace))
	entries, err := os.ReadDir(directory)
	if err != nil {
		return nil
	}
	out := []string{}
	for _, e := range entries {
		name := e.Name()
		if len(out) == limit || !strings.HasSuffix(name, ".settle") || !e.Type().IsRegular() {
			continue
		}
		path := filepath.Join(directory, name)
		if _, err := os.Lstat(strings.TrimSuffix(path, ".settle") + ".json.lock"); err == nil {
			continue
		}
		raw, err := os.ReadFile(path)
		op := string(raw)
		if err != nil || !operationPattern.MatchString(op) || digest(op)+".settle" != name {
			continue
		}
		out = append(out, op)
	}
	return out
}

// peek reads an operation's ledger record state and, when accepted, its
// receipt's comment ID; state is "absent" when no record exists.
func (l ledger) peek(namespace, operationID string) (state, id string, ok bool) {
	path := filepath.Join(l.dir, digest(namespace), digest(operationID)+".json")
	raw, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return "absent", "", true
	}
	var record ledgerRecord
	if err != nil || privatePath(path, false) != nil || json.Unmarshal(raw, &record) != nil {
		return "", "", false
	}
	var receipt struct {
		ID string `json:"id"`
	}
	if record.State == "accepted" && json.Unmarshal(record.Receipt, &receipt) != nil {
		return "", "", false
	}
	return record.State, receipt.ID, true
}
