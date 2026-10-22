package linearboard

import (
	"bytes"
	"clawbridge/internal/linearprovider"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"time"
)

var operationPattern = regexp.MustCompile(`\A[A-Za-z0-9._-]{1,128}\z`)

// errBindingChangedAfterProviderError is context for an uncertain provider
// outcome. It is deliberately not a ReceiptError: a binding change observed
// after a provider error must never replace the uncertain result.
var errBindingChangedAfterProviderError = errors.New("task-board binding changed after the provider error")

// notDispatchedError is returned by a dispatch closure only for a failure
// before its first provider mutation call could start. The ledger then
// removes the pending record it wrote for this call, so the same
// operation_id can be retried. It must never wrap an error from, or after,
// a provider mutation call.
type notDispatchedError struct{ err error }

func (e notDispatchedError) Error() string { return e.err.Error() }
func (e notDispatchedError) Unwrap() error { return e.err }
func notDispatched(err error) error        { return notDispatchedError{err} }

type ledgerRecord struct {
	Outcome   string          `json:"outcome,omitempty"`
	Version   int             `json:"version"`
	Digest    string          `json:"digest"`
	State     string          `json:"state"`
	Receipt   json.RawMessage `json:"receipt,omitempty"`
	UpdatedAt string          `json:"updated_at"`
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
// The only exception is a closure failure explicitly marked notDispatched.
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
	if receipt, exists, e := readLedger(path, hash); exists || e != nil {
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
		return nil, never.err
	}
	var receiptError *ReceiptError
	if err != nil && (receipt == nil || !errors.As(err, &receiptError)) {
		message := "Linear operation may have been dispatched; outcome uncertain, inspect Linear before retrying"
		if errors.Is(err, errBindingChangedAfterProviderError) {
			message += "; the task-board binding also changed"
		}
		return nil, errors.New(message)
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

func readLedger(path, hash string) (any, bool, error) {
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
	if previous.Digest != hash {
		return nil, true, errors.New("operation_id already used with a different scoped payload")
	}
	if previous.State != "accepted" {
		return nil, true, errors.New("operation outcome uncertain; read back Linear before a new operation_id")
	}
	var receipt map[string]any
	if len(previous.Receipt) > 65536 || json.Unmarshal(previous.Receipt, &receipt) != nil || receipt == nil {
		return nil, true, errors.New("invalid recorded receipt")
	}
	id, _ := receipt["id"].(string)
	if _, v2 := receipt["kind"]; v2 {
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
	return readLedger(path, digest(payload))
}
