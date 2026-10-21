package notify

import (
	"bytes"
	"clawbridge/internal/bus"
	"context"
	"encoding/json"
	"testing"
)

func TestCoordinationWakePolicyPreservesBacklogAcrossRestart(t *testing.T) {
	c := config(t)
	var out bytes.Buffer
	c.Output = &out
	policy := "hold"
	c.Executor = executorFunc(func(_ context.Context, r bus.Request) (bus.Result, error) {
		if r.Operation != "notification_status" {
			t.Fatal(r.Operation)
		}
		after := r.Args["after"].(int64)
		fresh := int64(1)
		if after >= 7 {
			fresh = 0
		}
		return bus.Result{Source: "local-authority", Data: NotificationStatus{Status: Status{"ws-id", "prj-id", "ses-id", 1, 7, fresh}, Policy: policy}}, nil
	})
	for _, p := range []string{"hold", "refuse"} {
		policy = p
		if err := Run(context.Background(), c); err != nil {
			t.Fatal(err)
		}
		if out.Len() != 0 || load(t, c.StatePath).After != 0 {
			t.Fatal("suppression lost backlog")
		}
	}
	policy = "accept"
	if err := Run(context.Background(), c); err != nil {
		t.Fatal(err)
	}
	before := out.Len()
	if before == 0 || load(t, c.StatePath).After != 7 {
		t.Fatal("release missed backlog")
	}
	if err := Run(context.Background(), c); err != nil {
		t.Fatal(err)
	}
	if out.Len() != before {
		t.Fatal("unchanged notice repeated")
	}
}
func TestCoordinationNotificationStatusStrictAndInboxUnchanged(t *testing.T) {
	base := Status{"ws-id", "prj-id", "ses-id", 1, 7, 1}
	if _, err := ParseStatus(bus.Result{Source: "server", Data: base}); err != nil {
		t.Fatal(err)
	}
	if _, err := ParseNotificationStatus(bus.Result{Source: "server", Data: base}); err == nil {
		t.Fatal("old status accepted for policy")
	}
	for _, raw := range []string{`null`, `""`, `"unknown"`, `1`, `true`} {
		b, _ := json.Marshal(NotificationStatus{Status: base, Policy: "accept"})
		var fields map[string]json.RawMessage
		json.Unmarshal(b, &fields)
		fields["policy"] = json.RawMessage(raw)
		if _, err := ParseNotificationStatus(bus.Result{Source: "server", Data: fields}); err == nil {
			t.Fatal(raw)
		}
	}
}
