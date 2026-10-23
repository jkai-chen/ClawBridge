package linearboard

import (
	"context"
	"encoding/json"
	"errors"
	"time"
)

// LedgerStore keeps a hosted server's operation records; *bus.Store
// implements it. Every call is one short transaction, so the shared ledger
// never holds a lock while Linear is called.
type LedgerStore interface {
	LedgerInsert(ctx context.Context, namespace, operationID, record string) (string, bool, error)
	LedgerGet(ctx context.Context, namespace, operationID string) (string, bool, error)
	LedgerReplace(ctx context.Context, namespace, operationID, from, to string) error
	LedgerDelete(ctx context.Context, namespace, operationID, record string) error
	LedgerMarkUnsettled(ctx context.Context, namespace, operationID string, unsettled bool) error
	LedgerUnsettled(ctx context.Context, namespace string, limit int) ([]string, error)
}

// SharedLedger is the operation ledger of a hosted server, shared by its
// instances. It keeps the private ledger's records and answers; where that
// one holds a file lock through dispatch, this one inserts the pending
// record first (a second caller then finds it and dispatches nothing) and
// settles it after, by compare-and-swap.
func SharedLedger(store LedgerStore, now func() time.Time) func(context.Context) OperationLedger {
	if now == nil {
		now = time.Now
	}
	return func(ctx context.Context) OperationLedger { return sharedLedger{ctx, store, now} }
}

type sharedLedger struct {
	ctx   context.Context
	store LedgerStore
	now   func() time.Time
}

func (l sharedLedger) stamp() string { return l.now().UTC().Format(time.RFC3339Nano) }

func encodeRecord(r ledgerRecord) string {
	raw, _ := json.Marshal(r)
	return string(raw)
}

// inProgressFor is how long a pending record without a reason is taken to be
// one another call is still dispatching: the server's write deadline plus a
// margin. An older one is uncertain.
const inProgressFor = 75 * time.Second

// decodeRecord reads a stored record and checks it as the private ledger
// checks its files. A young pending record without a reason is still being
// dispatched by another call: in progress, not uncertain, so the caller
// retries the same operation_id instead of reading back too early.
// matchPayload compares the payload digest; only a replay (lookupAny) reads
// without it.
func decodeRecord(raw, hash string, now time.Time, matchPayload bool) (any, bool, error) {
	var previous ledgerRecord
	if json.Unmarshal([]byte(raw), &previous) != nil || previous.Version != 1 || len(previous.Digest) != 64 {
		return nil, true, errors.New("corrupt operation ledger")
	}
	if _, e := time.Parse(time.RFC3339Nano, previous.UpdatedAt); e != nil {
		return nil, true, errors.New("corrupt operation timestamp")
	}
	if matchPayload && previous.Digest != hash {
		return nil, true, errors.New("operation_id already used with a different scoped payload")
	}
	if at, _ := time.Parse(time.RFC3339Nano, previous.UpdatedAt); previous.State == "pending" && previous.Reason == "" && now.Sub(at) < inProgressFor {
		return nil, true, InProgressError{"in_progress: this operation is being sent to Linear by another request; retry the same operation_id shortly"}
	}
	return recordOutcome(previous)
}

func (l sharedLedger) lookup(namespace, operationID string, payload any) (any, bool, error) {
	return l.find(namespace, operationID, digest(payload), true)
}

func (l sharedLedger) lookupAny(namespace, operationID string) (any, bool, error) {
	return l.find(namespace, operationID, "", false)
}

func (l sharedLedger) find(namespace, operationID, hash string, matchPayload bool) (any, bool, error) {
	if !operationPattern.MatchString(operationID) {
		return nil, false, errors.New("invalid operation ledger selection")
	}
	raw, found, err := l.store.LedgerGet(l.ctx, digest(namespace), operationID)
	if err != nil {
		return nil, true, errors.New("cannot read the shared operation ledger")
	}
	if !found {
		return nil, false, nil
	}
	return decodeRecord(raw, hash, l.now(), matchPayload)
}

func (l sharedLedger) write(namespace, operationID string, payload any, dispatch func() (any, error)) (any, error) {
	if !operationPattern.MatchString(operationID) {
		return nil, errors.New("operation_id requires 1–128 ASCII identifier characters")
	}
	ns, hash := digest(namespace), digest(payload)
	record := ledgerRecord{Version: 1, Digest: hash, State: "pending", UpdatedAt: l.stamp()}
	pending := encodeRecord(record)
	existing, created, err := l.store.LedgerInsert(l.ctx, ns, operationID, pending)
	if err != nil {
		return nil, errors.New("cannot record the operation in the shared ledger; nothing was sent")
	}
	if !created {
		receipt, _, e := decodeRecord(existing, hash, l.now(), true)
		return receipt, e
	}
	receipt, err := dispatch()
	settle := func(next ledgerRecord) error {
		next.UpdatedAt = l.stamp()
		return l.store.LedgerReplace(l.ctx, ns, operationID, pending, encodeRecord(next))
	}
	if never, ok := err.(notDispatchedError); ok && receipt == nil {
		if l.store.LedgerDelete(l.ctx, ns, operationID, pending) != nil {
			return nil, errors.New("operation ledger could not release a never-dispatched operation; inspect before retrying")
		}
		return nil, never
	}
	if refused, ok := err.(refusedBeforeDispatchError); ok && receipt == nil {
		reason := refused.err.Error()
		if len(reason) > 512 {
			reason = reason[:512]
		}
		record.State, record.Reason = "refused", reason
		if settle(record) != nil {
			return nil, errors.New("refused before dispatch; nothing was sent, but the refusal could not be recorded and the operation stays pending; inspect before retrying")
		}
		return nil, refusedBeforeDispatch(refusalError(reason))
	}
	var receiptError *ReceiptError
	if err != nil && (receipt == nil || !errors.As(err, &receiptError)) {
		reason := safeFailure(err)
		message := "Linear operation may have been dispatched; outcome uncertain"
		if reason != "" {
			message += " (" + reason + ")"
		}
		message += ", inspect Linear before retrying"
		if errors.Is(err, errBindingChangedAfterProviderError) {
			message += "; the task-board binding also changed"
		}
		// An uncertain record always keeps a reason, so it is never taken
		// for one still being dispatched.
		record.Reason = reason
		if record.Reason == "" {
			record.Reason = "uncertain"
		}
		_ = settle(record)
		return nil, UncertainError{message}
	}
	if receiptError != nil {
		record.Outcome = receiptError.Outcome
	}
	raw, e := json.Marshal(receipt)
	if e != nil || len(raw) > 65536 {
		return receipt, &ReceiptError{Outcome: "accepted_receipt_unrecorded"}
	}
	record.State, record.Receipt = "accepted", raw
	if settle(record) != nil {
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

func (l sharedLedger) markUnsettled(namespace, operationID string) error {
	if err := l.store.LedgerMarkUnsettled(l.ctx, digest(namespace), operationID, true); err != nil {
		return errors.New("cannot record the comment for its bus settle")
	}
	return nil
}

func (l sharedLedger) dropUnsettled(namespace, operationID string) {
	_ = l.store.LedgerMarkUnsettled(l.ctx, digest(namespace), operationID, false)
}

func (l sharedLedger) unsettled(namespace string, limit int) []string {
	ops, err := l.store.LedgerUnsettled(l.ctx, digest(namespace), limit)
	if err != nil {
		return nil
	}
	out := []string{}
	for _, op := range ops {
		if operationPattern.MatchString(op) {
			out = append(out, op)
		}
	}
	return out
}

func (l sharedLedger) peek(namespace, operationID string) (state, id string, ok bool) {
	raw, found, err := l.store.LedgerGet(l.ctx, digest(namespace), operationID)
	if err != nil {
		return "", "", false
	}
	if !found {
		return "absent", "", true
	}
	var record ledgerRecord
	if json.Unmarshal([]byte(raw), &record) != nil {
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
