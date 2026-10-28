package linearboard

import (
	"bytes"
	"clawbridge/internal/linearprovider"
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
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

var operationPattern = regexp.MustCompile(`\A[A-Za-z0-9._-]{1,128}\z`)

// errBindingChangedAfterProviderError is context for an uncertain provider
// outcome. It is deliberately not a ReceiptError: a binding change observed
// after a provider error must never replace the uncertain result.
var errBindingChangedAfterProviderError = errors.New("task-board binding changed after the provider error")

// Dispatch closures classify every failure before their first provider
// mutation call; neither marker may wrap an error from, or after, that call.
//
// notDispatchedError is a transient failure that learned nothing about Linear
// or binding state (cancellation, provider or authority read errors). The
// ledger removes the pending record this call wrote, so the same
// operation_id can be retried.
//
// refusedBeforeDispatchError is observed drift (binding changed, access
// revoked, actor changed, refreshed proof mismatch). The ledger records a
// terminal refusal: a same-ID retry returns it without any Linear I/O, because
// retrying would act on state the caller never saw.
// notDispatchedError keeps its cause, when the provider gave one, so a
// caller can tell Linear's rate limit from other "not sent" failures.
type notDispatchedError struct{ err, cause error }
type refusedBeforeDispatchError struct{ err error }

func (e notDispatchedError) Error() string { return e.err.Error() }
func (e notDispatchedError) Unwrap() []error {
	if e.cause == nil {
		return []error{e.err}
	}
	return []error{e.err, e.cause}
}
func notDispatched(err error) error                { return notDispatchedError{err: err} }
func (e refusedBeforeDispatchError) Error() string { return e.err.Error() }
func (e refusedBeforeDispatchError) Unwrap() error { return e.err }
func refusedBeforeDispatch(err error) error        { return refusedBeforeDispatchError{err} }

// UncertainError is an operation that may have reached Linear: a retry
// with the same operation_id sends nothing, and the caller reads Linear back.
type UncertainError struct{ Message string }

func (e UncertainError) Error() string { return e.Message }

// InProgressError is an operation another call is dispatching right now:
// retrying the same operation_id shortly gets its outcome.
type InProgressError struct{ Message string }

func (e InProgressError) Error() string { return e.Message }

// IsUncertain reports an outcome that may have reached Linear.
func IsUncertain(err error) bool { var u UncertainError; return errors.As(err, &u) }

// IsInProgress reports an operation still being dispatched elsewhere.
func IsInProgress(err error) bool { var p InProgressError; return errors.As(err, &p) }

var errActorChanged = errors.New("task-board actor changed before dispatch")
var errWriteAccessDenied = errors.New("current ClawBridge project role/actor does not permit task-board writes")

// authorityFailure classifies a binding or write-access check: a definite
// change or denial is drift, any other authority error is a failed read.
func authorityFailure(err error) error {
	var changed *ReceiptError
	if errors.As(err, &changed) || errors.Is(err, errWriteAccessDenied) {
		return refusedBeforeDispatch(err)
	}
	return notDispatched(err)
}

// proofFailure classifies a refreshed Linear proof: a provider read failure or
// cancellation is transient, a board verification mismatch is drift.
func proofFailure(err error) error {
	var provider *linearprovider.Error
	if errors.As(err, &provider) || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return notDispatched(err)
	}
	return refusedBeforeDispatch(err)
}

// sentCount reads how many tool calls a provider may have sent to Linear;
// known is false for a provider that cannot tell.
func sentCount(p any) (count uint64, known bool) {
	c, ok := p.(interface{ Sent() uint64 })
	if !ok {
		return 0, false
	}
	return c.Sent(), true
}

// writeFailure classifies a failed provider write. When the provider proves
// no tool call reached Linear since before was read, nothing was dispatched;
// any other failure goes to post and stays possibly dispatched.
func writeFailure(p any, before uint64, known bool, err error, post func(error) error) error {
	if after, ok := sentCount(p); known && ok && after == before {
		return notDispatchedError{fmt.Errorf("nothing was sent to Linear (%s); retry with the same operation_id", cmp.Or(safeFailure(err), "unavailable")), err}
	}
	return post(err)
}

// RateLimitedNotSent reports a write Linear's rate limit (or ClawBridge's
// pacer) kept from being sent, and how long Linear asked to wait, 0 when
// it didn't say.
func RateLimitedNotSent(err error) (time.Duration, bool) {
	var never notDispatchedError
	var provider *linearprovider.Error
	if !errors.As(err, &never) || never.cause == nil || !errors.As(never.cause, &provider) || provider.Kind != "rate_limited" {
		return 0, false
	}
	seconds, _ := strconv.Atoi(provider.RetryAfter)
	return time.Duration(max(0, seconds)) * time.Second, true
}

// safeFailure names a provider failure without its text, or returns "" for
// a failure that is not the provider's.
func safeFailure(err error) string {
	var provider *linearprovider.Error
	switch {
	case errors.As(err, &provider):
		return provider.Detail()
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout"
	case errors.Is(err, context.Canceled):
		return "canceled"
	}
	return ""
}

func refusalError(reason string) error {
	return errors.New("refused before dispatch; nothing was sent: " + reason + "; re-read and submit a new operation_id")
}

type ledgerRecord struct {
	Outcome   string          `json:"outcome,omitempty"`
	Version   int             `json:"version"`
	Digest    string          `json:"digest"`
	State     string          `json:"state"`
	Receipt   json.RawMessage `json:"receipt,omitempty"`
	Reason    string          `json:"reason,omitempty"`
	UpdatedAt string          `json:"updated_at"`
}

// OperationLedger records each write's operation ID before dispatch and its
// outcome after, so a retry never sends twice. The private ledger directory
// (ledger) is one; a hosted server's shared ledger is another. It also keeps
// the comments whose bus settle is still owed.
type OperationLedger interface {
	write(namespace, operationID string, payload any, dispatch func() (any, error)) (any, error)
	lookup(namespace, operationID string, payload any) (any, bool, error)
	// lookupAny skips only the payload-digest check, for a replay of a
	// recorded result.
	lookupAny(namespace, operationID string) (any, bool, error)
	markUnsettled(namespace, operationID string) error
	dropUnsettled(namespace, operationID string)
	unsettled(namespace string, limit int) []string
	peek(namespace, operationID string) (state, id string, ok bool)
}

// operations is the service's operation ledger.
func (s *Service) operations(ctx context.Context) OperationLedger {
	if s.cfg.Ledger != nil {
		return s.cfg.Ledger(ctx)
	}
	return ledger{s.cfg.LedgerDirectory, s.cfg.Now}
}

type ledger struct {
	dir string
	now func() time.Time
}

func digest(v any) string {
	raw, _ := json.Marshal(v)
	h := sha256.Sum256(raw)
	return hex.EncodeToString(h[:])
}

// A pending record is durable before dispatch. It is never automatically cleared
// or retried: process loss and unknown provider outcomes require operator readback.
// The only exceptions are closure failures explicitly marked notDispatched
// (record removed) or refusedBeforeDispatch (record made a terminal refusal).
func (l ledger) write(profile, operationID string, payload any, dispatch func() (any, error)) (any, error) {
	if !operationPattern.MatchString(operationID) {
		return nil, errors.New("operation_id requires 1–128 ASCII identifier characters")
	}
	if !filepath.IsAbs(l.dir) {
		return nil, errors.New("private operation ledger directory is required")
	}
	if l.now == nil {
		l.now = time.Now
	}
	if err := os.MkdirAll(l.dir, 0700); err != nil {
		return nil, errors.New("cannot create private operation ledger")
	}
	if err := privatePath(l.dir, true); err != nil {
		return nil, err
	}
	directory := filepath.Join(l.dir, digest(profile))
	if err := os.MkdirAll(directory, 0700); err != nil {
		return nil, errors.New("cannot create private operation ledger")
	}
	if err := privatePath(directory, true); err != nil {
		return nil, err
	}
	path := filepath.Join(directory, digest(operationID)+".json")
	lock, err := os.OpenFile(path+".lock", os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return nil, errors.New("operation ledger locked; inspect prior operation before retrying")
	}
	info, _ := lock.Stat()
	defer func() {
		_ = lock.Close()
		if current, e := os.Lstat(path + ".lock"); e == nil && os.SameFile(info, current) {
			_ = os.Remove(path + ".lock")
		}
	}()
	if err = makePrivate(path + ".lock"); err != nil {
		return nil, errors.New("cannot protect operation ledger lock")
	}
	hash := digest(payload)
	record := ledgerRecord{Version: 1, Digest: hash, State: "pending", UpdatedAt: l.now().UTC().Format(time.RFC3339Nano)}
	if receipt, exists, e := readLedger(path, hash, true); exists || e != nil {
		return receipt, e
	}
	if err = saveLedger(path, record); err != nil {
		return nil, err
	}
	receipt, err := dispatch()
	if never, ok := err.(notDispatchedError); ok && receipt == nil {
		// This call created the record under the exclusive lock and nothing
		// was dispatched, so removing it restores the prior absent state. A
		// failed removal leaves it pending, which still fails closed.
		if os.Remove(path) != nil || syncDirectory(directory) != nil {
			return nil, errors.New("operation ledger could not release a never-dispatched operation; inspect before retrying")
		}
		// The error keeps its type, so a caller can tell nothing was sent.
		return nil, never
	}
	if refused, ok := err.(refusedBeforeDispatchError); ok && receipt == nil {
		reason := refused.err.Error()
		if len(reason) > 512 {
			reason = reason[:512]
		}
		record.State = "refused"
		record.Reason = reason
		record.UpdatedAt = l.now().UTC().Format(time.RFC3339Nano)
		if saveLedger(path, record) != nil {
			return nil, errors.New("refused before dispatch; nothing was sent, but the refusal could not be recorded and the operation stays pending; inspect before retrying")
		}
		return nil, refusedBeforeDispatch(refusalError(reason))
	}
	var receiptError *ReceiptError
	if err != nil && (receipt == nil || !errors.As(err, &receiptError)) {
		// The record stays pending; its reason keeps only the safe failure code.
		reason := safeFailure(err)
		message := "Linear operation may have been dispatched; outcome uncertain"
		if reason != "" {
			message += " (" + reason + ")"
		}
		message += ", inspect Linear before retrying"
		if errors.Is(err, errBindingChangedAfterProviderError) {
			message += "; the task-board binding also changed"
		}
		record.Reason = reason
		record.UpdatedAt = l.now().UTC().Format(time.RFC3339Nano)
		_ = saveLedger(path, record)
		return nil, UncertainError{message}
	}
	if receiptError != nil {
		record.Outcome = receiptError.Outcome
	}
	raw, err := json.Marshal(receipt)
	if err != nil || len(raw) > 65536 {
		return receipt, &ReceiptError{Outcome: "accepted_receipt_unrecorded"}
	}
	record.State = "accepted"
	record.Receipt = raw
	record.UpdatedAt = l.now().UTC().Format(time.RFC3339Nano)
	if err = saveLedger(path, record); err != nil {
		outcome := "accepted_ledger_unrecorded"
		if receiptError != nil {
			outcome = receiptError.Outcome + "_ledger_unrecorded"
		}
		return receipt, &ReceiptError{Outcome: outcome}
	}
	if receiptError != nil {
		return receipt, receiptError
	}
	return receipt, nil
}
func saveLedger(path string, r ledgerRecord) error {
	f, err := os.CreateTemp(filepath.Dir(path), ".operation-*")
	if err != nil {
		return errors.New("cannot stage operation ledger")
	}
	name := f.Name()
	defer os.Remove(name)
	if err = makePrivate(name); err == nil {
		err = json.NewEncoder(f).Encode(r)
	}
	if err == nil {
		err = f.Sync()
	}
	if e := f.Close(); err == nil {
		err = e
	}
	if err != nil {
		return errors.New("cannot persist operation ledger")
	}
	if _, err = os.Lstat(path); err == nil {
		if err = privatePath(path, false); err != nil {
			return err
		}
	} else if !os.IsNotExist(err) {
		return errors.New("unsafe operation ledger")
	}
	if err = os.Rename(name, path); err != nil {
		return errors.New("cannot atomically replace operation ledger")
	}
	return syncDirectory(filepath.Dir(path))
}

// readLedger reads one record. matchPayload compares its payload digest with
// hash; only a replay (lookupAny) reads without it.
func readLedger(path, hash string, matchPayload bool) (any, bool, error) {
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return nil, false, nil
	}
	if err != nil {
		return nil, true, errors.New("cannot inspect operation ledger")
	}
	if err = privatePath(path, false); err != nil {
		return nil, true, err
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, true, errors.New("cannot read operation ledger")
	}
	opened, e := f.Stat()
	if e != nil || !os.SameFile(info, opened) {
		f.Close()
		return nil, true, errors.New("operation ledger changed during open")
	}
	raw, e := io.ReadAll(io.LimitReader(f, 131073))
	f.Close()
	if e != nil || len(raw) > 131072 {
		return nil, true, errors.New("invalid operation ledger")
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(raw, &fields) != nil {
		return nil, true, errors.New("corrupt operation ledger")
	}
	for _, key := range []string{"version", "digest", "state", "updated_at"} {
		v := bytes.TrimSpace(fields[key])
		if len(v) == 0 || bytes.Equal(v, []byte("null")) {
			return nil, true, errors.New("incomplete operation ledger")
		}
	}
	var previous ledgerRecord
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&previous) != nil || previous.Version != 1 || len(previous.Digest) != 64 {
		return nil, true, errors.New("corrupt operation ledger")
	}
	if _, e := time.Parse(time.RFC3339Nano, previous.UpdatedAt); e != nil {
		return nil, true, errors.New("corrupt operation timestamp")
	}
	if matchPayload && previous.Digest != hash {
		return nil, true, errors.New("operation_id already used with a different scoped payload")
	}
	return recordOutcome(previous)
}

// recordOutcome is what a recorded operation answers a retry: its receipt,
// its refusal, or an uncertain outcome for a pending one.
func recordOutcome(previous ledgerRecord) (any, bool, error) {
	// A refusal always has a reason; a pending record may keep the safe code
	// of its uncertain failure; nothing else has one.
	if (previous.State == "refused" && previous.Reason == "") || (previous.State != "refused" && previous.State != "pending" && previous.Reason != "") || len(previous.Reason) > 512 {
		return nil, true, errors.New("corrupt operation ledger")
	}
	if previous.State == "refused" {
		if len(previous.Receipt) != 0 || previous.Outcome != "" {
			return nil, true, errors.New("corrupt operation ledger")
		}
		return nil, true, refusalError(previous.Reason)
	}
	// pending and any unknown state remain uncertain (fail closed).
	if previous.State != "accepted" {
		message := "operation outcome uncertain"
		if previous.State == "pending" && previous.Reason != "" {
			message += " (" + previous.Reason + ")"
		}
		return nil, true, UncertainError{message + "; read back Linear before a new operation_id"}
	}
	var receipt map[string]any
	if len(previous.Receipt) > 65536 || json.Unmarshal(previous.Receipt, &receipt) != nil || receipt == nil {
		return nil, true, errors.New("invalid recorded receipt")
	}
	id, _ := receipt["id"].(string)
	if kind, _ := receipt["kind"].(string); kind == "view_create" || kind == "view_update" || kind == "view_adopt" {
		if !validViewReceipt(previous.Receipt) {
			return nil, true, errors.New("invalid recorded view receipt")
		}
	} else if kind == "view_delete" {
		if !validDeleteReceipt(previous.Receipt) {
			return nil, true, errors.New("invalid recorded view receipt")
		}
	} else if kind == "kind_apply" {
		if !validKindApplyReceipt(previous.Receipt) {
			return nil, true, errors.New("invalid recorded project kind receipt")
		}
	} else if kind == "kind_canary" {
		if !validKindCanary(receipt) {
			return nil, true, errors.New("invalid recorded canary receipt")
		}
	} else if kind == "kind_label" {
		if !validKindReceipt(previous.Receipt) {
			return nil, true, errors.New("invalid recorded project label receipt")
		}
	} else if kind, _ := receipt["kind"].(string); adminKinds[kind] {
		if !validAdminReceipt(previous.Receipt) {
			return nil, true, errors.New("invalid recorded admin receipt")
		}
	} else if _, v2 := receipt["kind"]; v2 {
		if !validReceiptV2(previous.Receipt) {
			return nil, true, errors.New("invalid recorded V2 receipt")
		}
	} else if !linearprovider.ValidID(id) {
		return nil, true, errors.New("invalid recorded receipt")
	}
	if previous.Outcome != "" {
		if previous.Outcome != "binding_changed" && previous.Outcome != "accepted_context_unverified" {
			return nil, true, errors.New("invalid recorded outcome")
		}
		return receipt, true, &ReceiptError{Outcome: previous.Outcome}
	}
	return receipt, true, nil
}
func (l ledger) lookup(profile, id string, payload any) (any, bool, error) {
	return l.find(profile, id, digest(payload), true)
}

// lookupAny is lookup whatever payload the record was written for: it skips
// only the payload-digest comparison, for a replay of a recorded result.
func (l ledger) lookupAny(profile, id string) (any, bool, error) {
	return l.find(profile, id, "", false)
}

func (l ledger) find(profile, id, hash string, matchPayload bool) (any, bool, error) {
	if !operationPattern.MatchString(id) || !filepath.IsAbs(l.dir) {
		return nil, false, errors.New("invalid operation ledger selection")
	}
	directory := filepath.Join(l.dir, digest(profile))
	path := filepath.Join(directory, digest(id)+".json")
	if _, err := os.Lstat(path + ".lock"); err == nil {
		return nil, true, errors.New("operation ledger locked; inspect prior operation")
	} else if !os.IsNotExist(err) {
		return nil, true, errors.New("cannot inspect operation lock")
	}
	if _, err := os.Lstat(directory); os.IsNotExist(err) {
		return nil, false, nil
	} else if err != nil {
		return nil, true, errors.New("cannot inspect operation ledger")
	}
	if err := privatePath(directory, true); err != nil {
		return nil, true, err
	}
	return readLedger(path, hash, matchPayload)
}
