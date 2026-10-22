package integration

import (
	"context"
	"strings"
	"testing"

	"clawbridge/internal/linearboard"
	"clawbridge/internal/linearprovider"
)

type l1PagingProvider struct {
	*l1SyntheticProvider
	queries []linearprovider.IssueQuery
	page    func(linearprovider.IssueQuery) linearprovider.Page[linearprovider.Issue]
}

func (p *l1PagingProvider) Issues(_ context.Context, q linearprovider.IssueQuery) (linearprovider.Page[linearprovider.Issue], error) {
	p.queries = append(p.queries, q)
	return p.page(q), nil
}

func TestL1BoardCannotInventProviderPageCompleteness(t *testing.T) {
	for _, mode := range []string{"one-team", "first-of-two", "last-of-two", "contradictory-cursor"} {
		t.Run(mode, func(t *testing.T) {
			f := newFixture(t)
			f.setup("w", "p")
			args := l1MappingArgs(0)
			if mode == "one-team" {
				args["linear_team_ids"] = []string{l1TeamA}
			}
			f.call("taskboard_set", "w", "p", "", args)
			p := &l1PagingProvider{l1SyntheticProvider: new(l1SyntheticProvider)}
			p.page = func(q linearprovider.IssueQuery) linearprovider.Page[linearprovider.Issue] {
				page := linearprovider.Page[linearprovider.Issue]{Items: []linearprovider.Issue{{ID: l1IssueID, TeamID: q.TeamID, Title: "valid bound issue"}}, Complete: false}
				if mode == "last-of-two" && q.TeamID == l1TeamA {
					page.Complete = true
				}
				if mode == "contradictory-cursor" {
					page.Complete = true
					page.NextCursor = "contradictory-provider-continuation"
				}
				return page
			}
			service := l1ServiceFor(t, f.store, p)
			scope := f.request("", "w", "p", "worker", actor("worker", nil))
			cursor := ""
			if mode == "last-of-two" {
				page, err := service.Issues(context.Background(), scope, 2, "")
				if err != nil || page.Complete || page.NextCursor == "" {
					t.Fatal("valid first team control could not continue", err)
				}
				cursor = page.NextCursor
			}
			before := len(p.queries)
			page, err := service.Issues(context.Background(), scope, 2, cursor)
			if err == nil {
				if page.Complete || page.NextCursor != "" {
					t.Fatalf("unknown provider completeness became false exhaustion/skipped next-team cursor: complete=%v next=%v", page.Complete, page.NextCursor != "")
				}
				if mode == "contradictory-cursor" {
					t.Fatal("contradictory exhaustion+cursor accepted")
				}
			} else if !strings.Contains(strings.ToLower(err.Error()), "incomplete") && !strings.Contains(strings.ToLower(err.Error()), "completeness") && !strings.Contains(strings.ToLower(err.Error()), "pagination") {
				t.Fatalf("incomplete provider page failed without actionable paging diagnosis: %v", err)
			}
			if len(p.queries) != before+1 {
				t.Fatal("incomplete page silently fetched/skipped other provider pages")
			}
		})
	}
}

func TestL1BoardEmptyPagesContinueAndOnlyVerifiedEndExhausts(t *testing.T) {
	f := newFixture(t)
	f.setup("w", "p")
	f.call("taskboard_set", "w", "p", "", l1MappingArgs(0))
	p := &l1PagingProvider{l1SyntheticProvider: new(l1SyntheticProvider)}
	p.page = func(q linearprovider.IssueQuery) linearprovider.Page[linearprovider.Issue] {
		if q.TeamID == l1TeamA && q.Cursor == "" {
			return linearprovider.Page[linearprovider.Issue]{Items: []linearprovider.Issue{}, NextCursor: "provider-next-A", Complete: false}
		}
		if q.TeamID == l1TeamA && q.Cursor != "provider-next-A" {
			t.Error("provider cursor was lost or reinterpreted")
		}
		return linearprovider.Page[linearprovider.Issue]{Items: []linearprovider.Issue{{ID: l1IssueID, TeamID: q.TeamID, Title: "bound page"}}, Complete: true}
	}
	cfg := l1ServiceConfig(t, f.store, p)
	service := linearboard.New(cfg)
	scope := f.request("", "w", "p", "worker", actor("worker", nil))
	cursor := ""
	var priorCursor string
	for n := 0; n < 3; n++ {
		page, err := service.Issues(context.Background(), scope, 2, cursor)
		if err != nil {
			t.Fatal(err)
		}
		if len(p.queries) != n+1 {
			t.Fatal("one board-page action fetched an unbounded provider scan")
		}
		if n == 0 && len(page.Items) != 0 {
			t.Fatal("empty provider page replaced with invented rows")
		}
		if n < 2 {
			if page.Complete || page.NextCursor == "" {
				t.Fatal("board declared exhaustion before verified team pages finished")
			}
			cursor = page.NextCursor
			priorCursor = cursor
		} else if !page.Complete || page.NextCursor != "" {
			t.Fatal("verified last team did not exhaust")
		}
	}
	if p.queries[0].TeamID != l1TeamA || p.queries[1].TeamID != l1TeamA || p.queries[2].TeamID != l1TeamB {
		t.Fatal("empty page caused team skip")
	}
	before := len(p.queries)
	changed := cfg
	changed.ProfileID = "lin_bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	if _, err := linearboard.New(changed).Issues(context.Background(), scope, 2, priorCursor); err == nil {
		t.Fatal("cursor crossed profile")
	}
	f.call("taskboard_set", "w", "p", "", l1MappingArgs(1))
	if _, err := service.Issues(context.Background(), scope, 2, priorCursor); err == nil {
		t.Fatal("cursor crossed mapping revision")
	}
	if len(p.queries) != before {
		t.Fatal("foreign cursor reached provider paging")
	}
}
