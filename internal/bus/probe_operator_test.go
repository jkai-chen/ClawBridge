package bus

import (
	"context"
	"reflect"
	"strings"
	"testing"
)

// The A4 probe's guards: a project any binding names is bound, whatever
// its case; the bound teams are every binding's.
func TestOperatorProbeBindings(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	got, err := h.s.OperatorProbeBindings(ctx, cpOne)
	if err != nil || got.ProjectBound || len(got.Teams) != 0 {
		t.Fatal("no binding:", got, err)
	}
	h.link(0, []string{cpOne, cpTwo})
	for _, lp := range []string{cpOne, strings.ToUpper(cpTwo)} {
		if got, err = h.s.OperatorProbeBindings(ctx, lp); err != nil || !got.ProjectBound || !reflect.DeepEqual(got.Teams, []string{sfTeam}) {
			t.Fatal(lp, got, err)
		}
	}
	if got, err = h.s.OperatorProbeBindings(ctx, cpThree); err != nil || got.ProjectBound || !reflect.DeepEqual(got.Teams, []string{sfTeam}) {
		t.Fatal("unbound project:", got, err)
	}
	if _, err = h.s.OperatorProbeBindings(ctx, "not-a-uuid"); err == nil {
		t.Fatal("a non-UUID was read")
	}
}
