package linearboard

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"testing"

	"clawbridge/internal/linearprovider"
)

// pagedCatalogV2 serves a team label listing over several pages and counts
// how many pages each command reads.
type pagedCatalogV2 struct {
	*providerV2
	pages [][]linearprovider.Label
	reads *int
}

func (p pagedCatalogV2) Labels(_ context.Context, _ string, o linearprovider.PageOptions) (linearprovider.Page[linearprovider.Label], error) {
	*p.reads++
	n := 0
	if o.Cursor != "" {
		n, _ = strconv.Atoi(o.Cursor)
	}
	page := linearprovider.Page[linearprovider.Label]{Items: p.pages[n], Complete: n == len(p.pages)-1}
	if !page.Complete {
		page.NextCursor = strconv.Itoa(n + 1)
	}
	return page, nil
}

func TestLinearV2LabelReplacementWalksTheListingOncePerPass(t *testing.T) {
	label := func(n int) linearprovider.Label {
		return linearprovider.Label{ID: fmt.Sprintf("00000000-0000-4000-8000-%012d", n), FilterScoped: true, EligibilityKnown: true}
	}
	pages := [][]linearprovider.Label{{label(1), label(2)}, {label(3), label(4)}, {label(5), label(6)}}
	requested := []string{label(1).ID, label(2).ID, label(3).ID, label(5).ID, label(6).ID}
	s, _, p, scope := testV2(t)
	reads := 0
	s.cfg.Open = func(context.Context, *http.Client, bool) (Provider, error) {
		return pagedCatalogV2{providerV2: p, pages: pages, reads: &reads}, nil
	}
	in := WriteInputV2{WriteInput: WriteInput{Operation: "update", OperationID: "five-labels", IssueID: issueID}, Labels: linearprovider.Field[[]string]{Present: true, Value: requested}, ExpectedLabels: linearprovider.Field[[]string]{Present: true, Value: []string{}}}
	if _, e := s.WriteV2(context.Background(), scope, in); e != nil || p.writes != 1 {
		t.Fatal("five-label replacement failed", e)
	}
	// One three-page walk at preflight and one inside the lock, not one walk
	// per label.
	if reads != 6 {
		t.Fatalf("label listing read %d pages, want 6", reads)
	}

	// A walk still stops as soon as every requested label is found.
	reads = 0
	in.OperationID, in.Labels.Value = "early-stop", []string{label(1).ID, label(2).ID}
	if _, e := s.WriteV2(context.Background(), scope, in); e != nil || reads != 2 {
		t.Fatalf("early-found labels read %d pages: %v", reads, e)
	}

	// Every walked row keeps the contradiction check, requested or not.
	reads = 0
	conflicting := label(4)
	conflicting.Archived = true
	pages[1] = []linearprovider.Label{label(3), label(4), conflicting}
	in.OperationID, in.Labels.Value = "contradiction", []string{label(5).ID}
	if _, e := s.WriteV2(context.Background(), scope, in); e == nil || p.writes != 2 {
		t.Fatal("contradictory unrequested row on a walked page accepted", e)
	}

	// A label absent from the complete listing is refused after one walk.
	pages[1] = []linearprovider.Label{label(3), label(4)}
	reads = 0
	in.OperationID, in.Labels.Value = "absent", []string{label(1).ID, "00000000-0000-4000-8000-000000000099"}
	if _, e := s.WriteV2(context.Background(), scope, in); e == nil || reads != 3 || p.writes != 2 {
		t.Fatalf("absent label: %v after %d pages", e, reads)
	}
}
