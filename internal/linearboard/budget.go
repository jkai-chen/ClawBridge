package linearboard

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"sync/atomic"

	"clawbridge/internal/bus"
	"clawbridge/internal/linearpace"
)

// budgeted counts a write made through the person's own Linear profile in
// the project's hourly Linear count (FR8): after the write, with the
// session to agents and without one to people. It is never refused: in
// v0.3.9 there is no ClawBridge limit, and Linear's own rate limit is the
// brake (the pacer waits out a 429 and its Retry-After). The count is
// every paced call made under ctx, the write's reads included; a replay
// answered from the ledger makes none and charges nothing. App writes are
// charged by the server and never come here, and moves are charged by
// their reservation.
func (s *Service) budgeted(ctx context.Context, scope bus.Request, kind, operationID string, write func(context.Context) error) error {
	if s.cfg.ServerCharged {
		return write(ctx)
	}
	if operationID == "" {
		var b [12]byte
		_, _ = rand.Read(b[:])
		operationID = "budget-" + hex.EncodeToString(b[:])
	}
	var calls atomic.Int64
	err := write(linearpace.WithCounter(ctx, &calls))
	if n := calls.Load(); n > 0 {
		// The calls were made, whatever the write's answer. A server
		// before v0.3.9 doesn't count them; a charge that fails leaves the
		// write as it ended.
		_, _ = s.budgetCall(context.WithoutCancel(ctx), scope, map[string]any{"operation_id": operationID, "kind": kind, "calls": int(min(n, 200))})
	}
	return err
}

func (s *Service) budgetCall(ctx context.Context, scope bus.Request, args map[string]any) (map[string]any, error) {
	scope.Operation = "linear_write_charge"
	scope.Args = instanceArgs(scope.Args)
	for k, v := range args {
		scope.Args[k] = v
	}
	r, e := s.cfg.Executor.Execute(ctx, scope)
	if e != nil {
		return nil, e
	}
	m, _ := r.Data.(map[string]any)
	return m, nil
}
