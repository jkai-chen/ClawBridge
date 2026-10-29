package linearboard

import "clawbridge/internal/bus"

// budgetAnswer is a test authority's answer to linear_write_charge: room,
// and the count charged.
func budgetAnswer(r bus.Request) (bus.Result, error) {
	return bus.Result{Source: "authority", Data: map[string]any{"operation_id": r.Args["operation_id"], "charged": r.Args["calls"]}}, nil
}
