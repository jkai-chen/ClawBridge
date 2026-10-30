package bus

import (
	"context"
	"testing"
	"time"
)

// The probe's reads on Postgres: a random team, as the reviewer's
// Postgres is shared.
func TestPostgresWebhookProbeReads(t *testing.T) {
	f := newPGIdentityFixture(t)
	ctx := context.Background()
	team := whRandomTeam()
	t.Cleanup(func() {
		for _, q := range []string{`DELETE FROM webhook_deliveries WHERE team_id=$1`, `DELETE FROM webhook_counts WHERE team_id=$1`} {
			f.s.db.ExecContext(context.Background(), q, team)
		}
	})
	start := time.Now().UTC()
	d := WebhookDelivery{DeliveryID: "sha256:" + newID(""), TeamID: team, EventType: "Issue", Action: "update", EntityID: whIssue, UpdatedKeys: []string{"labelIds"}, LinearAt: start, ReceivedAt: start.Add(time.Second), Size: 321}
	if _, err := f.s.WebhookRecord(ctx, d); err != nil {
		t.Fatal(err)
	}
	rows, err := f.s.WebhookProbeDeliveries(ctx, team, start)
	if err != nil || len(rows) != 1 || rows[0].DeliveryID != d.DeliveryID || rows[0].Size != 321 || rows[0].UpdatedKeys != "labelIds" || !rows[0].ReceivedAt.Equal(d.ReceivedAt) {
		t.Fatal(rows, err)
	}
	if err = f.s.WebhookCount(ctx, team, "stale", 2, start); err != nil {
		t.Fatal(err)
	}
	counts, err := f.s.WebhookProbeCounts(ctx, team, start)
	if err != nil || counts[team]["stale"] != 2 {
		t.Fatal(counts, err)
	}
}
