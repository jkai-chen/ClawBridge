package linearboard

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"clawbridge/internal/linearprovider"
)

// commentingV2 adds comment reads and records what they were asked.
type commentingV2 struct {
	*providerV2
	read      *[]string
	since     *[]string
	deadlines *[]time.Time
	result    *linearprovider.Comments
}

func (p commentingV2) IssueComments(ctx context.Context, q linearprovider.CommentsQuery) (linearprovider.Comments, error) {
	*p.read = append(*p.read, q.IssueID+"|"+q.Identifier)
	*p.since = append(*p.since, q.Since.Raw())
	deadline, _ := ctx.Deadline()
	*p.deadlines = append(*p.deadlines, deadline)
	if p.result != nil {
		return *p.result, nil
	}
	return linearprovider.Comments{Items: []linearprovider.Comment{{ID: "c1000000-0000-4000-8000-000000000001", AuthorKind: "unknown", CreatedAt: "2026-10-22T08:00:00Z", UpdatedAt: "2026-10-22T08:00:00Z", UntrustedBody: "text"}}, Complete: true, Watermark: "cw1.x"}, nil
}

func commentingTestV2(t *testing.T) (*Service, *providerV2, *[]string, *[]string) {
	t.Helper()
	s, _, p, _ := testV2(t)
	read, since, deadlines := []string{}, []string{}, []time.Time{}
	c := commentingV2{providerV2: p, read: &read, since: &since, deadlines: &deadlines}
	s.cfg.Open = func(context.Context, *http.Client, bool) (Provider, error) { return c, nil }
	return s, p, &read, &since
}

func TestLinearV2CommentsAreReadOnlyAfterTheIssueProvesScope(t *testing.T) {
	s, p, read, since := commentingTestV2(t)
	_, _, _, scope := testV2(t)
	out, e := s.IssueV2WithComments(context.Background(), scope, issueID, CommentsInputV2{Since: "2026-10-22T07:00:00Z"})
	if e != nil || len(out.Items) != 1 || !out.Complete || out.ID != issueID || len(*read) != 1 || (*read)[0] != issueID+"|" || (*since)[0] != "2026-10-22T07:00:00Z" {
		t.Fatal("comment read", out, e, *read)
	}
	raw, _ := json.Marshal(out)
	for _, key := range []string{`"id":"` + issueID, `"comments":[`, `"untrusted_body":"text"`, `"comments_complete":true`, `"comments_watermark":"cw1.x"`} {
		if !strings.Contains(string(raw), key) {
			t.Fatal("issue read with comments lacks", key, string(raw))
		}
	}
	// An issue outside the verified pairs never reaches list_comments.
	outside := linearprovider.IssueV2{Issue: linearprovider.Issue{ID: parentA, TeamID: teamID, ProjectID: projectB}, ProjectKnown: true}
	p.issues[parentA] = outside
	if _, e = s.IssueV2WithComments(context.Background(), scope, parentA, CommentsInputV2{}); e == nil || len(*read) != 1 {
		t.Fatal("comments read for an out-of-scope issue", e, *read)
	}
	// Plain reads never read comments and keep their shape.
	plain, e := s.IssueV2(context.Background(), scope, issueID)
	raw, _ = json.Marshal(plain)
	if e != nil || len(*read) != 1 || strings.Contains(string(raw), "comments") {
		t.Fatal("plain read touched comments", string(raw), e)
	}
	// A bad comments_since is refused before any Linear read.
	reads := p.reads
	if _, e = s.IssueV2WithComments(context.Background(), scope, issueID, CommentsInputV2{Since: "-P1D"}); e == nil || p.reads != reads || len(*read) != 1 {
		t.Fatal("relative comments_since accepted or read", e)
	}
}

func TestLinearV2CommentsNeedProviderSupport(t *testing.T) {
	s, _, _, scope := testV2(t)
	if _, e := s.IssueV2WithComments(context.Background(), scope, issueID, CommentsInputV2{}); e == nil {
		t.Fatal("provider without comment reads answered")
	}
}

func TestLinearV2ExecuteCommentArguments(t *testing.T) {
	s, _, read, since := commentingTestV2(t)
	_, _, _, scope := testV2(t)
	data, e := s.ExecuteV2(context.Background(), scope, "issue", map[string]any{"id": issueID, "comments": true, "comments_since": "2026-10-22T07:00:00Z"})
	if _, ok := data.(IssueCommentsV2); e != nil || !ok || len(*read) != 1 || (*since)[0] != "2026-10-22T07:00:00Z" {
		t.Fatal("comments: true not routed", data, e)
	}
	for name, args := range map[string]map[string]any{
		"since without comments": {"id": issueID, "comments_since": "2026-10-22T07:00:00Z"},
		"comments not boolean":   {"id": issueID, "comments": "true"},
		"since not string":       {"id": issueID, "comments": true, "comments_since": 5},
	} {
		if _, e := s.ExecuteV2(context.Background(), scope, "issue", args); e == nil {
			t.Fatal(name, "accepted")
		}
	}
	if len(*read) != 1 {
		t.Fatal("refused arguments read comments", *read)
	}
	data, e = s.ExecuteV2(context.Background(), scope, "issue", map[string]any{"id": issueID, "comments": false})
	if _, ok := data.(linearprovider.IssueV2); e != nil || !ok || len(*read) != 1 {
		t.Fatal("comments: false is a plain read", data, e)
	}
}

// The issue's display identifier reaches the provider for its scope check,
// the walk stops short of the caller's deadline, and an incomplete comment
// read still returns the issue.
func TestLinearV2CommentsKeepTheIssueAndADeadlineReserve(t *testing.T) {
	s, p, _, _ := commentingTestV2(t)
	_, _, _, scope := testV2(t)
	issue := p.issues[issueID]
	issue.Identifier = "SYN-6"
	p.issues[issueID] = issue
	read, since, deadlines := []string{}, []string{}, []time.Time{}
	partial := linearprovider.Comments{Items: []linearprovider.Comment{}, Truncated: true, WatermarkReason: "result_limit", Since: "2026-10-22T07:00:00Z"}
	c := commentingV2{providerV2: p, read: &read, since: &since, deadlines: &deadlines, result: &partial}
	s.cfg.Open = func(context.Context, *http.Client, bool) (Provider, error) { return c, nil }
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	callerDeadline, _ := ctx.Deadline()
	out, e := s.IssueV2WithComments(ctx, scope, issueID, CommentsInputV2{Since: "2026-10-22T07:00:00Z"})
	if e != nil || out.ID != issueID || out.Complete || out.WatermarkReason != "result_limit" || len(read) != 1 || read[0] != issueID+"|SYN-6" {
		t.Fatal("incomplete comment read lost the issue or the identifier", out, e, read)
	}
	if deadlines[0].IsZero() || callerDeadline.Sub(deadlines[0]) < commentsReserveV2 {
		t.Fatal("comment walk not stopped short of the caller's deadline", deadlines[0], callerDeadline)
	}
	// A future comments_since is refused before any read.
	if _, e = s.IssueV2WithComments(ctx, scope, issueID, CommentsInputV2{Since: time.Now().Add(time.Hour).UTC().Format(time.RFC3339)}); e == nil || len(read) != 1 {
		t.Fatal("future comments_since accepted", e)
	}
}

// An identifier the plain read tolerates but that is not a display
// identifier never makes the comment read fail.
func TestLinearV2CommentsLeaveOutAnInvalidIdentifier(t *testing.T) {
	s, p, read, _ := commentingTestV2(t)
	_, _, _, scope := testV2(t)
	issue := p.issues[issueID]
	issue.Identifier = "not an identifier"
	p.issues[issueID] = issue
	if _, e := s.IssueV2(context.Background(), scope, issueID); e != nil {
		t.Fatal("plain read", e)
	}
	if _, e := s.IssueV2WithComments(context.Background(), scope, issueID, CommentsInputV2{}); e != nil || len(*read) != 1 || (*read)[0] != issueID+"|" {
		t.Fatal("invalid identifier passed or failed the read", e, *read)
	}
}

// identComment records whether a comment write went through CommentOnIssue
// with the issue's proven identifier.
type identComment struct {
	*providerV2
	got *[]string
}

func (p identComment) CommentOnIssue(_ context.Context, issue, identifier, _ string) (linearprovider.Receipt, error) {
	*p.got = append(*p.got, identifier)
	return linearprovider.Receipt{ID: "c1000000-0000-4000-8000-000000000001", IssueID: issue}, nil
}

func TestLinearV2CommentWritePassesTheProvenIdentifier(t *testing.T) {
	for identifier, want := range map[string]string{"SYN-6": "SYN-6", "not an identifier": "plain Comment"} {
		s, _, p, scope := testV2(t)
		issue := p.issues[issueID]
		issue.Identifier = identifier
		p.issues[issueID] = issue
		got := []string{}
		c := identComment{providerV2: p, got: &got}
		s.cfg.Open = func(context.Context, *http.Client, bool) (Provider, error) { return c, nil }
		if _, e := s.WriteV2(context.Background(), scope, WriteInputV2{WriteInput: WriteInput{Operation: "comment", OperationID: "comment-" + strings.ReplaceAll(identifier, " ", "-"), IssueID: issueID, Body: "hello"}}); e != nil {
			t.Fatal(identifier, e)
		}
		if want == "plain Comment" {
			if len(got) != 0 || p.writes != 1 {
				t.Fatal("an invalid identifier reached CommentOnIssue", got, p.writes)
			}
		} else if len(got) != 1 || got[0] != want {
			t.Fatal("proven identifier not passed to the readback", got)
		}
	}
}
