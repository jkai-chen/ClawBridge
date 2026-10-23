package linearboard

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"clawbridge/internal/bus"
	"clawbridge/internal/linearprovider"
)

// fakeCommentLedger is the bus comment ledger as the client sees it.
type fakeCommentLedger struct {
	reserves, settles []map[string]any
	// refuse fails every reserve; unavailable fails every settle.
	refuse, unavailable error
	answers             string
	posted              map[string]string
	// flags, young and pending answer linear_comments_posted; unreadable
	// fails it.
	flags      map[string]any
	young      bool
	pending    []any
	unreadable error
	lookups    []map[string]any
	// due is what comment_watch_due hands out; the rest record the watch.
	due                    []any
	leases                 []map[string]any
	reports, settleMatches []map[string]any
	reportErr              error
	// gos answers marker_go per part; markerSettles records marker_settle.
	gos           map[string]map[string]any
	markerGoes    []map[string]any
	markerSettles []map[string]any
	markersDue    []any
}

func (f *fakeCommentLedger) execute(r bus.Request) (bus.Result, error) {
	switch r.Operation {
	case "linear_comment_reserve":
		f.reserves = append(f.reserves, r.Args)
		if f.refuse != nil {
			return bus.Result{}, f.refuse
		}
		op, _ := r.Args["operation_id"].(string)
		if id, ok := f.posted[op]; ok {
			return bus.Result{Source: "local-authority", Data: map[string]any{"state": "posted", "replay": true, "comment_id": id}}, nil
		}
		answers := f.answers
		if v, _ := r.Args["in_response_to"].(string); v != "" {
			answers = v
		}
		return bus.Result{Source: "local-authority", Data: map[string]any{"state": "pending", "in_response_to": answers}}, nil
	case "linear_comments_posted":
		f.lookups = append(f.lookups, r.Args)
		if f.unreadable != nil {
			return bus.Result{}, f.unreadable
		}
		flags := f.flags
		if flags == nil {
			flags = map[string]any{}
		}
		pending := f.pending
		if pending == nil {
			pending = []any{}
		}
		return bus.Result{Source: "local-authority", Data: map[string]any{"posted": flags, "young_pending": f.young, "pending": pending}}, nil
	case "comment_watch_lease":
		f.leases = append(f.leases, r.Args)
		return bus.Result{Source: "local-authority", Data: map[string]any{"lease_token": "tok", "generation": 1, "budget_limit": 200, "budget_used": 0}}, nil
	case "comment_watch_due":
		due := f.due
		f.due = nil
		if due == nil {
			due = []any{}
		}
		return bus.Result{Source: "local-authority", Data: map[string]any{"issues": due, "budget_used": 3, "budget_limit": 200, "budget_exhausted": false}}, nil
	case "comment_watch_report":
		f.reports = append(f.reports, r.Args)
		if f.reportErr != nil {
			return bus.Result{}, f.reportErr
		}
		notified := []string{}
		for _, c := range asList(r.Args["comments"]) {
			notified = append(notified, c.(map[string]any)["comment_id"].(string))
		}
		return bus.Result{Source: "local-authority", Data: map[string]any{"issue_uuid": r.Args["issue_uuid"], "notified": notified}}, nil
	case "linear_comment_settle_match":
		f.settleMatches = append(f.settleMatches, r.Args)
		return bus.Result{Source: "local-authority", Data: map[string]any{"state": "posted"}}, nil
	case "marker_go":
		f.markerGoes = append(f.markerGoes, r.Args)
		g, ok := f.gos[r.Args["part"].(string)]
		if !ok {
			g = map[string]any{"action": "none"}
		}
		return bus.Result{Source: "local-authority", Data: g}, nil
	case "marker_settle":
		f.markerSettles = append(f.markerSettles, r.Args)
		return bus.Result{Source: "local-authority", Data: map[string]any{"state": r.Args["outcome"]}}, nil
	case "marker_due":
		due := f.markersDue
		f.markersDue = nil
		if due == nil {
			due = []any{}
		}
		return bus.Result{Source: "local-authority", Data: map[string]any{"markers": due}}, nil
	case "linear_comment_settle":
		f.settles = append(f.settles, r.Args)
		if f.unavailable != nil {
			return bus.Result{}, f.unavailable
		}
		return bus.Result{Source: "local-authority", Data: map[string]any{"state": r.Args["outcome"]}}, nil
	}
	return bus.Result{}, errors.New("unexpected comment ledger operation")
}

// sentCounting reports sent calls as the native provider does: a call that
// provably never reached Linear is not counted.
type sentCounting struct {
	*providerV2
	sent uint64
}

func (c *sentCounting) Sent() uint64 { return c.sent }
func (c *sentCounting) Comment(ctx context.Context, issue, body string) (linearprovider.Receipt, error) {
	r, err := c.providerV2.Comment(ctx, issue, body)
	var unsent *linearprovider.Error
	if !errors.As(err, &unsent) || !unsent.NotSent {
		c.sent++
	}
	return r, err
}

func sha(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

func sidecars(t *testing.T, dir string) []string {
	t.Helper()
	found, err := filepath.Glob(filepath.Join(dir, "*", "*.settle"))
	if err != nil {
		t.Fatal(err)
	}
	return found
}

// A comment is reserved with the digest of the exact body posted, sent, and
// settled with the comment Linear returned.
func TestCommentIsReservedSentAndSettled(t *testing.T) {
	s, a, p, scope := testV2(t)
	a.comments.answers = parentA
	out, err := s.WriteV2(context.Background(), scope, WriteInputV2{WriteInput: WriteInput{Operation: "comment", OperationID: "c1", IssueID: issueID, Body: "hello"}})
	if err != nil || len(p.bodies) != 1 {
		t.Fatal(err, p.bodies)
	}
	if len(a.comments.reserves) != 1 || a.comments.reserves[0]["body_sha256"] != sha(p.bodies[0]) || a.comments.reserves[0]["issue_uuid"] != issueID || a.comments.reserves[0]["operation_id"] != "c1" {
		t.Fatal("reserve did not name the posted body:", a.comments.reserves)
	}
	receipt := out.(ReceiptV2)
	if len(a.comments.settles) != 1 || a.comments.settles[0]["outcome"] != "posted" || a.comments.settles[0]["comment_id"] != receipt.ID || a.comments.settles[0]["via"] != "receipt" {
		t.Fatal("not settled with the posted comment:", a.comments.settles)
	}
	if receipt.InResponseTo != parentA {
		t.Fatal("receipt lacks the answered comment:", receipt)
	}
	if left := sidecars(t, s.cfg.LedgerDirectory); len(left) != 0 {
		t.Fatal("a settled comment kept its sidecar:", left)
	}
}

// A refused reserve sends nothing, and the same operation_id can be retried.
func TestRefusedReserveSendsNothing(t *testing.T) {
	s, a, p, scope := testV2(t)
	a.comments.refuse = errors.New("at most 3 ClawBridge comments per issue per hour; next slot at T")
	in := WriteInputV2{WriteInput: WriteInput{Operation: "comment", OperationID: "capped", IssueID: issueID, Body: "hello"}}
	if _, err := s.WriteV2(context.Background(), scope, in); err == nil || !strings.Contains(err.Error(), "next slot") || len(p.bodies) != 0 {
		t.Fatal("a refused reserve still posted:", err, p.bodies)
	}
	if left := sidecars(t, s.cfg.LedgerDirectory); len(left) != 0 {
		t.Fatal(left)
	}
	a.comments.refuse = nil
	if _, err := s.WriteV2(context.Background(), scope, in); err != nil || len(p.bodies) != 1 {
		t.Fatal("the refused operation_id cannot be retried:", err)
	}
}

// A comment Linear refused before it was sent settles as failed, so it
// holds no cap slot.
func TestUnsentCommentSettlesFailed(t *testing.T) {
	s, a, p, scope := testV2(t)
	counting := &sentCounting{providerV2: p}
	s.cfg.Open = func(context.Context, *http.Client, bool) (Provider, error) { return counting, nil }
	p.commentErr = &linearprovider.Error{Kind: "rate_limited", Status: 429, NotSent: true}
	if _, err := s.WriteV2(context.Background(), scope, WriteInputV2{WriteInput: WriteInput{Operation: "comment", OperationID: "unsent", IssueID: issueID, Body: "hello"}}); err == nil {
		t.Fatal("refused comment accepted")
	}
	if len(a.comments.settles) != 1 || a.comments.settles[0]["outcome"] != "failed" {
		t.Fatal("an unsent comment did not settle failed:", a.comments.settles)
	}
}

// A settle the bus missed is retried from the file ledger by the next
// comment write of the session.
func TestMissedSettleIsSweptFromTheFileLedger(t *testing.T) {
	s, a, p, scope := testV2(t)
	a.comments.unavailable = errors.New("bus unavailable")
	first, err := s.WriteV2(context.Background(), scope, WriteInputV2{WriteInput: WriteInput{Operation: "comment", OperationID: "first", IssueID: issueID, Body: "one"}})
	if err != nil {
		t.Fatal("a missed settle failed the write:", err)
	}
	if left := sidecars(t, s.cfg.LedgerDirectory); len(left) != 1 {
		t.Fatal("no sidecar kept for the missed settle:", left)
	}
	a.comments.unavailable, a.comments.settles = nil, nil
	if _, err = s.WriteV2(context.Background(), scope, WriteInputV2{WriteInput: WriteInput{Operation: "comment", OperationID: "second", IssueID: issueID, Body: "two"}}); err != nil || len(p.bodies) != 2 {
		t.Fatal(err)
	}
	if len(a.comments.settles) != 2 || a.comments.settles[0]["operation_id"] != "first" || a.comments.settles[0]["via"] != "file_ledger_sweep" || a.comments.settles[0]["comment_id"] != first.(ReceiptV2).ID {
		t.Fatal("the missed settle was not swept:", a.comments.settles)
	}
	if left := sidecars(t, s.cfg.LedgerDirectory); len(left) != 0 {
		t.Fatal(left)
	}
}

// An uncertain outcome stays pending on the bus: the sweep never settles it.
func TestUncertainCommentIsNotSwept(t *testing.T) {
	s, a, p, scope := testV2(t)
	p.commentErr = errors.New("connection reset after send")
	if _, err := s.WriteV2(context.Background(), scope, WriteInputV2{WriteInput: WriteInput{Operation: "comment", OperationID: "lost", IssueID: issueID, Body: "one"}}); err == nil || !strings.Contains(err.Error(), "uncertain") {
		t.Fatal(err)
	}
	p.commentErr = nil
	if _, err := s.WriteV2(context.Background(), scope, WriteInputV2{WriteInput: WriteInput{Operation: "comment", OperationID: "next", IssueID: issueID, Body: "two"}}); err != nil {
		t.Fatal(err)
	}
	for _, settle := range a.comments.settles {
		if settle["operation_id"] == "lost" {
			t.Fatal("an uncertain comment was settled:", settle)
		}
	}
	if left := sidecars(t, s.cfg.LedgerDirectory); len(left) != 1 {
		t.Fatal("the uncertain comment's sidecar was dropped:", left)
	}
}

// The bus already holds this operation as posted (the file ledger is gone):
// nothing is sent again.
func TestBusPostedReplayIsNotResent(t *testing.T) {
	s, a, p, scope := testV2(t)
	a.comments.posted = map[string]string{"again": parentA}
	_, err := s.WriteV2(context.Background(), scope, WriteInputV2{WriteInput: WriteInput{Operation: "comment", OperationID: "again", IssueID: issueID, Body: "hello"}})
	if err == nil || !strings.Contains(err.Error(), parentA) || len(p.bodies) != 0 {
		t.Fatal("a bus-posted operation was sent again:", err, p.bodies)
	}
}

// The V1 comment path goes through the same ledger.
func TestV1CommentIsLedgered(t *testing.T) {
	s, a, p, scope := testService(t)
	if _, err := s.Write(context.Background(), scope, WriteInput{Operation: "comment", OperationID: "v1", IssueID: issueID, Body: "hello"}); err != nil {
		t.Fatal(err)
	}
	if len(a.comments.reserves) != 1 || a.comments.reserves[0]["body_sha256"] != sha(p.bodies[0]) || len(a.comments.settles) != 1 || a.comments.settles[0]["outcome"] != "posted" {
		t.Fatal("V1 comment not ledgered:", a.comments.reserves, a.comments.settles)
	}
	if _, err := os.Stat(s.cfg.LedgerDirectory); err != nil {
		t.Fatal(err)
	}
}

const (
	humanComment = "c1000000-0000-4000-8000-000000000001"
	ourComment   = "c1000000-0000-4000-8000-000000000002"
	lateComment  = "c1000000-0000-4000-8000-000000000003"
)

// commentRead reads issueID's comments from a fake that returns comments.
func commentRead(t *testing.T, comments ...linearprovider.Comment) (*Service, *authorityV2, func() IssueCommentsV2) {
	t.Helper()
	s, a, p, scope := testV2(t)
	read, since, deadlines := []string{}, []string{}, []time.Time{}
	result := linearprovider.Comments{Items: comments, Complete: true}
	c := commentingV2{providerV2: p, read: &read, since: &since, deadlines: &deadlines, result: &result}
	s.cfg.Open = func(context.Context, *http.Client, bool) (Provider, error) { return c, nil }
	return s, a, func() IssueCommentsV2 {
		t.Helper()
		out, err := s.IssueV2WithComments(context.Background(), scope, issueID, CommentsInputV2{})
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
}

func commentRow(id, body string, created time.Time) linearprovider.Comment {
	return linearprovider.Comment{ID: id, AuthorKind: "unknown", CreatedAt: created.UTC().Format(time.RFC3339Nano), UpdatedAt: created.UTC().Format(time.RFC3339Nano), UntrustedBody: body}
}

func TestCommentReadFlagsClawBridgeComments(t *testing.T) {
	now := time.Now()
	_, a, read := commentRead(t, commentRow(humanComment, "question", now), commentRow(ourComment, "answer", now))
	a.comments.flags = map[string]any{ourComment: map[string]any{"session_id": "ses_1", "session_name": "worker-1", "posted_at": "x", "kind": "reply"}}
	out := read()
	if len(a.comments.lookups) != 1 || a.comments.lookups[0]["issue_uuid"] != issueID || len(a.comments.lookups[0]["comment_ids"].([]any)) != 2 {
		t.Fatal("ownership not looked up once for the read:", a.comments.lookups)
	}
	if out.CommentsOwnership != "available" || *out.Items[0].PostedByClawBridge || !*out.Items[1].PostedByClawBridge || out.Items[1].ClawBridgeMatch != "ledger" || out.Items[1].ClawBridgeSession.Name != "worker-1" {
		t.Fatal("comments not flagged:", out.Items)
	}
	raw, _ := json.Marshal(out)
	for _, key := range []string{`"posted_by_clawbridge":false`, `"posted_by_clawbridge":true`, `"clawbridge_session":{"id":"ses_1","name":"worker-1"}`, `"comments_ownership":"available"`, `"untrusted_body":"question"`} {
		if !strings.Contains(string(raw), key) {
			t.Fatal("read lacks", key, string(raw))
		}
	}
}

// Without the bus the read still succeeds, flags nothing and says so.
func TestCommentReadWithoutTheBusSaysOwnershipIsUnavailable(t *testing.T) {
	_, a, read := commentRead(t, commentRow(humanComment, "question", time.Now()))
	a.comments.unreadable = errors.New("bus unavailable")
	out := read()
	raw, _ := json.Marshal(out)
	if out.CommentsOwnership != "unavailable" || out.Items[0].PostedByClawBridge != nil || strings.Contains(string(raw), "posted_by_clawbridge") {
		t.Fatal("unknown ownership was guessed:", string(raw))
	}
}

func TestCommentReadReportsYoungPending(t *testing.T) {
	_, a, read := commentRead(t, commentRow(humanComment, "question", time.Now()))
	a.comments.young = true
	if out := read(); !out.CommentsYoungPending {
		t.Fatal("young pending not reported")
	}
}

// An old pending reservation claims the earliest comment in its window
// whose body is exactly the one reserved, and nothing else.
func TestOldPendingReservationMatchesOnlyItsExactBodyInItsWindow(t *testing.T) {
	reserved := time.Now().Add(-time.Hour).UTC()
	body := "answer\n\n— 🤖 ClawBridge agent · session ses"
	_, a, read := commentRead(t,
		commentRow(humanComment, body+" ", reserved.Add(time.Minute)),
		commentRow(ourComment, body, reserved.Add(2*time.Minute)),
		commentRow(lateComment, body, reserved.Add(3*time.Minute)),
	)
	a.comments.pending = []any{map[string]any{"operation_digest": strings.Repeat("a", 64), "body_sha256": sha(body), "reserved_at": reserved.Format(time.RFC3339Nano)}}
	out := read()
	got := map[string]string{}
	for _, c := range out.Items {
		got[c.ID] = c.ClawBridgeMatch
	}
	if got[ourComment] != "pending_body" || got[humanComment] != "" || got[lateComment] != "" {
		t.Fatal("pending body match:", got)
	}
	// Outside the window nothing matches.
	a.comments.pending = []any{map[string]any{"operation_digest": strings.Repeat("a", 64), "body_sha256": sha(body), "reserved_at": reserved.Add(-15 * time.Minute).Format(time.RFC3339Nano)}}
	for _, c := range read().Items {
		if *c.PostedByClawBridge {
			t.Fatal("a comment outside the window matched:", c.ID)
		}
	}
}

// A reply to a reply is reserved against the thread root it is sent to,
// naming the comment it answers.
func TestReplyIsReservedAgainstItsThreadRoot(t *testing.T) {
	s, a, p, scope := testV2(t)
	p.caps.CommentReply = true
	r := &replier{providerV2: p, parents: map[string]bool{replyParent: true, commentOther: true}, nested: map[string]string{commentOther: replyParent}}
	s.cfg.Open = func(context.Context, *http.Client, bool) (Provider, error) { return r, nil }
	got, err := s.ExecuteV2(context.Background(), scope, "comment", map[string]any{"id": issueID, "body": "an answer", "operation_id": "answer", "parent_id": commentOther, "in_response_to": commentOther})
	if err != nil {
		t.Fatal(err)
	}
	if len(a.comments.reserves) != 1 || a.comments.reserves[0]["parent_id"] != replyParent || a.comments.reserves[0]["in_response_to"] != commentOther {
		t.Fatal("reply not reserved against its root:", a.comments.reserves)
	}
	if got.(ReceiptV2).InResponseTo != commentOther {
		t.Fatal(got)
	}
	// in_response_to without a reply is refused before anything is read.
	if _, err = s.ExecuteV2(context.Background(), scope, "comment", map[string]any{"id": issueID, "body": "x", "operation_id": "top", "in_response_to": commentOther}); err == nil || len(a.comments.reserves) != 1 {
		t.Fatal("in_response_to without a reply accepted:", err)
	}
}

type oldServer struct{ *authorityV2 }

func (o oldServer) Execute(ctx context.Context, r bus.Request) (bus.Result, error) {
	if r.Operation == "taskboard_access" && r.Args["ledger"] != nil {
		return bus.Result{}, errors.New(`unsupported argument "ledger" for taskboard_access`)
	}
	return o.authorityV2.Execute(ctx, r)
}

// A new client against an older server says which side to upgrade.
func TestOlderServerAsksForAServerUpgrade(t *testing.T) {
	s, a, p, scope := testV2(t)
	s.cfg.Executor = oldServer{a}
	_, err := s.WriteV2(context.Background(), scope, WriteInputV2{WriteInput: WriteInput{Operation: "comment", OperationID: "c", IssueID: issueID, Body: "x"}})
	if err == nil || !strings.Contains(err.Error(), "upgrade the server first") || len(p.bodies) != 0 {
		t.Fatal(err)
	}
}
