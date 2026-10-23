package linearboard

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"clawbridge/internal/linearprovider"
)

func asList(v any) []any {
	list, _ := v.([]any)
	return list
}

type conflict struct{}

func (conflict) Error() string { return "the comment watch lease was lost; acquire it again" }
func (conflict) Code() string  { return "conflict" }

// watchOnce runs one watch cycle over issueID, whose comments read returns
// comments, and returns the cycle.
func watchOnce(t *testing.T, setup func(*Service, *authorityV2), comments ...linearprovider.Comment) (WatchCycle, *authorityV2, error) {
	t.Helper()
	s, a, p, scope := testV2(t)
	read, since, deadlines := []string{}, []string{}, []time.Time{}
	result := linearprovider.Comments{Items: comments, Complete: true}
	c := commentingV2{providerV2: p, read: &read, since: &since, deadlines: &deadlines, result: &result}
	s.cfg.Open = func(context.Context, *http.Client, bool) (Provider, error) { return c, nil }
	a.comments.due = []any{map[string]any{"issue_uuid": issueID, "since": "2026-10-22T07:00:00.000000000Z"}}
	if setup != nil {
		setup(s, a)
	}
	var got WatchCycle
	err := s.Watch(context.Background(), scope, WatchOptions{Budget: 50, Once: true, Cycle: func(c WatchCycle) error { got = c; return nil }})
	if len(since) > 1 || len(since) == 1 && since[0] != "2026-10-22T07:00:00.000000000Z" {
		t.Fatal("the issue was not read once since the bus cursor:", since)
	}
	return got, a, err
}

func TestWatchReportsOnlyCommentsThatMayBeNews(t *testing.T) {
	now := time.Now()
	agent := commentRow(lateComment, "bot", now)
	agent.AuthorKind = "agent"
	parent := humanComment
	reply := commentRow("c1000000-0000-4000-8000-000000000004", "follow-up", now.Add(time.Second))
	reply.ParentID = &parent
	cycle, a, err := watchOnce(t, func(_ *Service, a *authorityV2) {
		a.comments.flags = map[string]any{ourComment: map[string]any{"session_id": "ses_1", "session_name": "w"}}
	}, commentRow(humanComment, "question", now), commentRow(ourComment, "answer", now), agent, reply)
	if err != nil {
		t.Fatal(err)
	}
	if a.comments.leases[0]["action"] != "acquire" || a.comments.leases[0]["budget"] != 50 || a.comments.leases[len(a.comments.leases)-1]["action"] != "release" {
		t.Fatal("lease not acquired with the budget and released:", a.comments.leases)
	}
	if len(a.comments.reports) != 1 {
		t.Fatal(a.comments.reports)
	}
	r := a.comments.reports[0]
	if r["lease_token"] != "tok" || r["generation"] != 1 || r["issue_uuid"] != issueID || r["error"] != nil || r["calls"].(int) < 1 {
		t.Fatal("report not fenced or not counted:", r)
	}
	ids := []string{}
	for _, c := range asList(r["comments"]) {
		m := c.(map[string]any)
		ids = append(ids, m["comment_id"].(string))
		if m["author_kind"] != "unknown" || len(m) > 4 {
			t.Fatal("report carries more than typed fields:", m)
		}
		if m["comment_id"] == reply.ID && m["parent_id"] != humanComment {
			t.Fatal("reply reported without its thread:", m)
		}
	}
	if strings.Join(ids, ",") != reply.ID+","+humanComment {
		t.Fatal("reported comments:", ids)
	}
	if cycle.Polled != 1 || len(cycle.Notified) != 2 {
		t.Fatal(cycle)
	}
}

func TestWatchHoldsWhatItCannotDecide(t *testing.T) {
	for name, setup := range map[string]func(*Service, *authorityV2){
		"young_pending":         func(_ *Service, a *authorityV2) { a.comments.young = true },
		"ownership_unavailable": func(_ *Service, a *authorityV2) { a.comments.unreadable = errors.New("bus down") },
		"out_of_binding": func(s *Service, a *authorityV2) {
			p := s.cfg.Open
			s.cfg.Open = func(ctx context.Context, h *http.Client, ro bool) (Provider, error) {
				prov, err := p(ctx, h, ro)
				prov.(commentingV2).issues[issueID] = linearprovider.IssueV2{Issue: linearprovider.Issue{ID: issueID, TeamID: teamID, ProjectID: projectB}, ProjectKnown: true}
				return prov, err
			}
		},
	} {
		cycle, a, err := watchOnce(t, setup, commentRow(humanComment, "question", time.Now()))
		if err != nil {
			t.Fatal(name, err)
		}
		if len(a.comments.reports) != 1 || a.comments.reports[0]["error"] != name || a.comments.reports[0]["comments"] != nil || cycle.Held[issueID] != name {
			t.Fatal(name, "not held:", a.comments.reports, cycle)
		}
	}
}

// An old reservation matched by body is settled under the lease before the
// report, and its comment is not reported.
func TestWatchSettlesABodyMatchBeforeReporting(t *testing.T) {
	reserved := time.Now().Add(-time.Hour).UTC()
	_, a, err := watchOnce(t, func(_ *Service, a *authorityV2) {
		a.comments.pending = []any{map[string]any{"operation_digest": strings.Repeat("b", 64), "body_sha256": sha("ours"), "reserved_at": reserved.Format(time.RFC3339Nano)}}
	}, commentRow(ourComment, "ours", reserved.Add(time.Minute)), commentRow(humanComment, "question", reserved.Add(2*time.Minute)))
	if err != nil {
		t.Fatal(err)
	}
	m := a.comments.settleMatches
	if len(m) != 1 || m[0]["comment_id"] != ourComment || m[0]["issue_uuid"] != issueID || m[0]["lease_token"] != "tok" || m[0]["operation_digest"] != strings.Repeat("b", 64) {
		t.Fatal("body match not settled under the lease:", m)
	}
	if got := asList(a.comments.reports[0]["comments"]); len(got) != 1 || got[0].(map[string]any)["comment_id"] != humanComment {
		t.Fatal("our matched comment was reported:", got)
	}
}

// A lost lease ends the watcher.
func TestWatchStopsWhenTheLeaseIsLost(t *testing.T) {
	_, _, err := watchOnce(t, func(_ *Service, a *authorityV2) { a.comments.reportErr = conflict{} }, commentRow(humanComment, "q", time.Now()))
	var coded interface{ Code() string }
	if !errors.As(err, &coded) || coded.Code() != "conflict" {
		t.Fatal("watcher kept going without its lease:", err)
	}
}

// countingReader reports sent calls as the native provider does: each
// comments read here costs three tool calls (pages).
type countingReader struct {
	commentingV2
	sent *uint64
}

func (c countingReader) Sent() uint64 { return *c.sent }
func (c countingReader) IssueComments(ctx context.Context, q linearprovider.CommentsQuery) (linearprovider.Comments, error) {
	*c.sent += 3
	return c.commentingV2.IssueComments(ctx, q)
}

// A report carries the Linear calls the poll actually sent, so the bus
// reconciles the budget it charged up front.
func TestWatchReportsTheCallsAPollSent(t *testing.T) {
	_, a, err := watchOnce(t, func(s *Service, _ *authorityV2) {
		open := s.cfg.Open
		var sent uint64
		s.cfg.Open = func(ctx context.Context, h *http.Client, ro bool) (Provider, error) {
			p, err := open(ctx, h, ro)
			return countingReader{commentingV2: p.(commentingV2), sent: &sent}, err
		}
	}, commentRow(humanComment, "q", time.Now()))
	if err != nil {
		t.Fatal(err)
	}
	if got := a.comments.reports[0]["calls"]; got != 3 {
		t.Fatal("report did not carry the calls sent:", got)
	}
}
