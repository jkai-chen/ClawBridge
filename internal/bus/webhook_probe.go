package bus

import (
	"context"
	"strings"
	"time"
)

// WebhookProbeRow is one recorded delivery as the operator's webhook probe
// reads it (FR14 §9 P1): kinds, key names, a size and times, never text.
type WebhookProbeRow struct {
	DeliveryID, WebhookID, EventType, Action, Outcome, UpdatedKeys string
	LinearAt, ReceivedAt                                           time.Time
	Size                                                           int
}

// WebhookProbeDeliveries lists team's deliveries received at or after
// since, oldest first.
func (s *Store) WebhookProbeDeliveries(ctx context.Context, team string, since time.Time) ([]WebhookProbeRow, error) {
	tx, err := s.beginIntake(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	rows, err := query(ctx, tx, `SELECT delivery_id,webhook_id,event_type,action,outcome,updated_keys,linear_at,received_at,size FROM webhook_deliveries WHERE team_id=? AND received_at>=? ORDER BY received_at,delivery_id`, strings.ToLower(team), layout(since))
	if err != nil {
		return nil, err
	}
	out := make([]WebhookProbeRow, 0, len(rows))
	for _, r := range rows {
		out = append(out, WebhookProbeRow{
			DeliveryID: str(r, "delivery_id"), WebhookID: str(r, "webhook_id"), EventType: str(r, "event_type"), Action: str(r, "action"),
			Outcome: str(r, "outcome"), UpdatedKeys: str(r, "updated_keys"), LinearAt: at(str(r, "linear_at")), ReceivedAt: at(str(r, "received_at")),
			Size: asInt(r["size"]),
		})
	}
	return out, tx.Commit()
}

// WebhookProbeCounts sums the counters of team and the unattributed ("")
// by reason, over the hours from since's on.
func (s *Store) WebhookProbeCounts(ctx context.Context, team string, since time.Time) (map[string]map[string]int, error) {
	tx, err := s.beginIntake(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	rows, err := query(ctx, tx, `SELECT team_id,reason,SUM(n) AS n FROM webhook_counts WHERE (team_id=? OR team_id='') AND hour>=? GROUP BY team_id,reason`, strings.ToLower(team), webhookHour(since))
	if err != nil {
		return nil, err
	}
	out := map[string]map[string]int{}
	for _, r := range rows {
		t := str(r, "team_id")
		if out[t] == nil {
			out[t] = map[string]int{}
		}
		out[t][str(r, "reason")] += asInt(r["n"])
	}
	return out, tx.Commit()
}
