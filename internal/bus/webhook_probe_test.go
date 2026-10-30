package bus

import (
	"context"
	"testing"
	"time"
)

// The probe reads a team's deliveries received since its start, oldest
// first, and the counters since an hour: kinds, key names, sizes and times
// only.
func TestWebhookProbeReads(t *testing.T) {
	h := newWebhookHarness(t)
	ctx := context.Background()
	start := time.Now().UTC()
	old := WebhookDelivery{DeliveryID: "00000000-0000-4000-8000-00000000d001", TeamID: cpOne, EventType: "Issue", Action: "create", EntityID: whIssue, LinearAt: start.Add(-time.Hour), ReceivedAt: start.Add(-time.Hour)}
	first := WebhookDelivery{DeliveryID: "00000000-0000-4000-8000-00000000d002", TeamID: cpOne, WebhookID: "00000000-0000-4000-8000-00000000f001", EventType: "Issue", Action: "update", EntityID: whIssue, UpdatedKeys: []string{"labelIds", "updatedAt"}, LinearAt: start.Add(time.Second), ReceivedAt: start.Add(2 * time.Second), Size: 1234}
	second := WebhookDelivery{DeliveryID: "sha256:abc", TeamID: cpOne, EventType: "Comment", Action: "create", EntityID: whIssueOff, IssueID: whIssue, LinearAt: start.Add(3 * time.Second), ReceivedAt: start.Add(3 * time.Second), Size: 99}
	other := WebhookDelivery{DeliveryID: "00000000-0000-4000-8000-00000000d003", TeamID: whTeamB, EventType: "Issue", Action: "create", EntityID: whIssue, LinearAt: start, ReceivedAt: start.Add(time.Second)}
	for _, d := range []WebhookDelivery{second, old, first, other} {
		if _, err := h.s.WebhookRecord(ctx, d); err != nil {
			t.Fatal(err)
		}
	}
	rows, err := h.s.WebhookProbeDeliveries(ctx, cpOne, start)
	if err != nil || len(rows) != 2 {
		t.Fatal(rows, err)
	}
	r := rows[0]
	if r.DeliveryID != first.DeliveryID || r.EventType != "Issue" || r.Action != "update" || r.WebhookID != first.WebhookID || r.UpdatedKeys != "labelIds,updatedAt" || r.Size != 1234 || !r.LinearAt.Equal(first.LinearAt) || !r.ReceivedAt.Equal(first.ReceivedAt) || r.Outcome == "" {
		t.Fatalf("%+v", r)
	}
	if rows[1].DeliveryID != "sha256:abc" || rows[1].EventType != "Comment" {
		t.Fatalf("%+v", rows[1])
	}
	// Counters: the team's and the unattributed, summed by reason.
	for _, c := range []struct {
		team, reason string
		n            int
	}{{cpOne, "stale", 2}, {"", "signature", 3}, {whTeamB, "stale", 7}} {
		if err := h.s.WebhookCount(ctx, c.team, c.reason, c.n, start); err != nil {
			t.Fatal(err)
		}
	}
	counts, err := h.s.WebhookProbeCounts(ctx, cpOne, start.Add(-time.Hour))
	if err != nil || counts[cpOne]["stale"] != 2 || counts[""]["signature"] != 3 || counts[whTeamB] != nil {
		t.Fatal(counts, err)
	}
}
