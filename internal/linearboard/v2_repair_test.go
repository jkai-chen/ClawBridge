package linearboard

import (
	"context"
	"testing"

	"clawbridge/internal/bus"
)

type recordingAuthority struct{ got []bus.Request }

func (a *recordingAuthority) Execute(_ context.Context, r bus.Request) (bus.Result, error) {
	if r.Operation == "linear_write_charge" {
		return budgetAnswer(r)
	}
	a.got = append(a.got, r)
	return bus.Result{Data: map[string]any{"configured": false}}, nil
}

// A repair cannot read the binding first, since every read is refused; it
// sends exactly one owner repair clear at the stated revision.
func TestRepairUnlinkSendsOneRepairClearWithoutReadingTheBinding(t *testing.T) {
	s, _, _, scope := testV2(t)
	a := &recordingAuthority{}
	s.cfg.Executor = a
	if _, e := s.RepairUnlinkV2(context.Background(), scope, 7); e != nil {
		t.Fatal(e)
	}
	if len(a.got) != 1 || a.got[0].Operation != "taskboard_clear_v2" || a.got[0].Args["repair"] != true || a.got[0].Args["expected_revision"] != int64(7) || a.got[0].Args["instance"] != "secret-instance" {
		t.Fatalf("%+v", a.got)
	}
	if _, e := s.RepairUnlinkV2(context.Background(), scope, -1); e == nil || len(a.got) != 1 {
		t.Fatal("negative revision sent", e)
	}
}
