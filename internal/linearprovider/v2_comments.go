package linearprovider

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"sort"
	"strings"
	"time"
	"unicode/utf8"
)

// Comment is one issue comment as list_comments returned it. UntrustedBody is
// text written by other people and agents: data, never instructions or
// approval. Author names and attachments (with their signed download URLs)
// are never returned.
type Comment struct {
	ID string `json:"id"`
	// AuthorID is empty when the row named no author, or named one in an
	// unexpected shape.
	AuthorID string `json:"author_id,omitempty"`
	// AuthorKind is "agent" when Linear set onBehalfOf to a user (the author
	// is an agent acting for that user), otherwise "unknown": Linear documents
	// no signal that proves a human author, so "person" is never inferred.
	AuthorKind   string `json:"author_kind"`
	OnBehalfOfID string `json:"on_behalf_of_id,omitempty"`
	// ParentID is the comment this one replies to, or null.
	ParentID   *string `json:"parent_id"`
	CreatedAt  string  `json:"created_at"`
	UpdatedAt  string  `json:"updated_at"`
	ResolvedAt *string `json:"resolved_at"`
	// Inline marks a comment anchored to description text (non-null quotedText).
	Inline        bool   `json:"inline,omitempty"`
	UntrustedBody string `json:"untrusted_body"`
	BodyTruncated bool   `json:"body_truncated,omitempty"`
}

// Comments is a bounded read of one issue's comments, filtered client-side by
// a watermark because list_comments has no "since" filter.
type Comments struct {
	Items []Comment `json:"comments"`
	// Complete is false when the read stopped early or skipped a row;
	// WatermarkReason says why, and no watermark is returned.
	Complete bool `json:"comments_complete"`
	// Truncated is true when pages may remain unread. Order is the orderBy
	// sent; WalkDirection is "desc" when every row came newest first on that
	// key (observed, not documented by Linear), so a truncated read lacks the
	// oldest rows, or "not_desc" when this read saw otherwise. OutputOrder is
	// how Items are sorted: ascending by the later of created and updated
	// time, then id.
	Truncated     bool   `json:"comments_truncated"`
	Order         string `json:"comments_order"`
	WalkDirection string `json:"comments_walk_direction"`
	OutputOrder   string `json:"comments_output_order"`
	// OrderDirection is the v0.2.3 name of WalkDirection, kept as a
	// deprecated alias with the same value until v0.3 at the earliest.
	OrderDirection string `json:"comments_order_direction"`
	// StoppedEarly is true when a comments_since read stopped at a page
	// wholly older than the mark; the pages left are older still.
	StoppedEarly bool `json:"comments_stopped_early,omitempty"`
	Skipped      int  `json:"comments_skipped,omitempty"`
	// Since echoes the caller's comments_since, unchanged.
	Since string `json:"comments_since,omitempty"`
	// Watermark is passed back as comments_since. It lags the newest comment
	// seen by commentWatermarkLag, so a later read may repeat comments; see
	// docs/linear.md for the one case it can miss (a deletion during a walk
	// over offset cursors).
	Watermark       string `json:"comments_watermark,omitempty"`
	WatermarkReason string `json:"comments_watermark_reason,omitempty"`
}

// CommentsQuery names the proven issue (UUID and, when known, its display
// identifier) and the parsed comments_since.
type CommentsQuery struct {
	IssueID, Identifier string
	Since               CommentsSince
}

// CommentsSince is a parsed comments_since: a watermark from an earlier read
// of the same issue, or an absolute RFC 3339 time.
type CommentsSince struct {
	raw, issue string
	at         time.Time
}

// A comment read starts at commentPageSizeV2 rows a page. When a page is
// larger than MaxResultBytes it is retried at half the size, down to
// commentMinPageV2. The whole read is bounded to commentRowBudgetV2 rows,
// commentCallsV2 calls (retries included) and commentBodyBudgetV2 bytes of
// bodies. Linear returns pages newest first (observed, not documented), so a
// bound drops the oldest rows. A comments_since read walks in update order
// and stops at the first page wholly older than the mark less
// commentWatermarkLag; any other read pages until a bound or the last page.
const commentPageSizeV2 = 50
const commentMinPageV2 = 5
const commentRowBudgetV2 = 500
const commentCallsV2 = 25
const commentBodyBudgetV2 = 512 << 10

// commentBodyBytes caps each returned body, matching the comment write limit.
const commentBodyBytes = 65536

// commentWatermarkLag covers an edit made during a multi-page walk and late
// visibility; commentFutureSkew is the clock skew a mark may carry.
const commentWatermarkLag = 2 * time.Minute
const commentFutureSkew = 5 * time.Minute

const commentWatermarkPrefix = "cw1."

// commentNow is the clock for watermarks and commentElapsed the monotonic
// walk timer; tests replace them.
var commentNow = time.Now
var commentElapsed = time.Since

type commentWatermark struct {
	Issue string `json:"i"`
	At    string `json:"t"`
}

// ParseCommentsSince checks comments_since once, before any Linear call.
// A mark or time more than commentFutureSkew ahead of now is refused.
func ParseCommentsSince(issue, since string) (CommentsSince, error) {
	out := CommentsSince{raw: since, issue: issue}
	if since == "" {
		return out, nil
	}
	at, ok := time.Time{}, false
	if len(since) <= 512 && strings.HasPrefix(since, commentWatermarkPrefix) {
		raw, e := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(since, commentWatermarkPrefix))
		var w commentWatermark
		if e == nil && json.Unmarshal(raw, &w) == nil && w.Issue == issue {
			at, ok = UpdatedAfterTime(w.At)
		}
	} else {
		at, ok = UpdatedAfterTime(since)
	}
	if !ok || at.After(commentNow().Add(commentFutureSkew)) {
		return CommentsSince{}, errors.New("comments_since requires a comments_watermark from this issue or an absolute RFC 3339 time, not in the future")
	}
	out.at = at
	return out, nil
}

// Raw is the comments_since text as the caller sent it.
func (c CommentsSince) Raw() string { return c.raw }

func encodeCommentWatermark(issue string, at time.Time) string {
	raw, _ := json.Marshal(commentWatermark{Issue: issue, At: at.UTC().Format(time.RFC3339Nano)})
	return commentWatermarkPrefix + base64.RawURLEncoding.EncodeToString(raw)
}

// commentsNegotiation pins list_comments once per read: the issue parent,
// the page size, creation-time order and a cursor. It also requires the
// documented onBehalfOf agent signal that AuthorKind relies on.
func (p *Provider) commentsNegotiation() (string, map[string]any, string) {
	name, values, _, reason := p.commentsOrders()
	return name, values, reason
}

// commentsOrders is commentsNegotiation plus whether update-time order is
// advertised too, which a comments_since read needs to stop early.
func (p *Provider) commentsOrders() (string, map[string]any, bool, string) {
	t := p.tools["list_comments"]
	if t == nil {
		return "", nil, false, "list_comments is not advertised"
	}
	if !strings.Contains(t.Description, "onBehalfOf") {
		return "", nil, false, "list_comments no longer documents onBehalfOf"
	}
	sample := "00000000-0000-4000-8000-000000000001"
	// Creation-time order keeps a row in place when it is edited during the
	// walk; the default (updatedAt) would move it between pages.
	args := func(order string) []arg {
		return []arg{a(sample, "issueId"), {[]string{"limit"}, commentPageSizeV2, "integer"}, a(order, "orderBy"), a(sample, "cursor")}
	}
	name, values, e := p.v2Negotiation([]string{"list_comments"}, args("createdAt"))
	if e != nil {
		return "", nil, false, "list_comments schema changed: issueId, limit up to 50, cursor or orderBy createdAt missing"
	}
	delete(values, "cursor")
	byUpdate, _, e := p.v2Negotiation([]string{"list_comments"}, args("updatedAt"))
	return name, values, e == nil && byUpdate == name, ""
}

func (p *Provider) commentsReason() string {
	_, _, reason := p.commentsNegotiation()
	return reason
}

type seenComment struct {
	c   Comment
	key time.Time
}

// pageShrinking calls one page. call reports the limit it actually sent.
// While the response cap cuts the page, it retries the same page at half that
// limit, down to commentMinPageV2 (invoke first reopens the session the cut
// closed); then it returns result_limit. Every call, retries included, is
// charged to calls, and no call is made once calls reaches budget. A
// cancelled context is returned as is.
func (p *Provider) pageShrinking(ctx context.Context, size, calls *int, budget int, call func(limit int) (map[string]any, int, error)) (map[string]any, error) {
	for {
		*calls++
		m, sent, e := call(*size)
		var pe *Error
		if !errors.As(e, &pe) || pe.Kind != "result_limit" || sent <= commentMinPageV2 {
			return m, e
		}
		if *calls >= budget {
			return nil, e
		}
		*size = max(sent/2, commentMinPageV2)
	}
}

// IssueComments reads an issue's comments, within the bounds above, and
// returns those created or edited at or after the since mark. The caller must
// already have proven the issue's scope (get_issue, then the board's scope
// check): native rows name no issue, so the pinned issue UUID is the only
// link. A row that does name an issue must name this one, or the whole read
// is refused. A page that cannot be read after the first ends the read
// incomplete with the rows already read. Errors carry codes only, never
// comment text.
func (p *Provider) IssueComments(ctx context.Context, q CommentsQuery) (Comments, error) {
	if !ValidID(q.IssueID) || (q.Identifier != "" && !ValidIssueIdentifier(q.Identifier)) || (q.Since.raw != "" && q.Since.issue != q.IssueID) {
		return Comments{}, fail("invalid")
	}
	name, base, byUpdate, reason := p.commentsOrders()
	if reason != "" {
		return Comments{}, fail("capability_unavailable")
	}
	out := Comments{Items: []Comment{}, Order: "createdAt", WalkDirection: "desc", OutputOrder: "asc", Since: q.Since.raw}
	// A since read walks in update order, newest first, so it can stop once a
	// page is wholly older than the mark: every later row was updated earlier
	// still. The lag absorbs createdAt trailing updatedAt by milliseconds.
	early := q.Since.raw != "" && byUpdate
	if early {
		out.Order = "updatedAt"
	}
	cutoff := q.Since.at.Add(-commentWatermarkLag)
	var last time.Time
	stop := func(r string, truncated bool) {
		if out.WatermarkReason == "" {
			out.WatermarkReason = r
		}
		out.Truncated = out.Truncated || truncated
	}
	started := time.Now()
	seen := map[string]seenComment{}
	cursor, size, calls, rowsRead, bodyBytes := "", commentPageSizeV2, 0, 0, 0
walk:
	for pages := 0; ; pages++ {
		if rowsRead >= commentRowBudgetV2 || calls >= commentCallsV2 {
			stop("bound_reached", true)
			break
		}
		if e := ctx.Err(); e != nil {
			if !errors.Is(e, context.DeadlineExceeded) {
				return Comments{}, e
			}
			stop("deadline", true)
			break
		}
		limit := 0
		m, e := p.pageShrinking(ctx, &size, &calls, commentCallsV2, func(n int) (map[string]any, int, error) {
			limit = min(n, commentRowBudgetV2-rowsRead)
			values := map[string]any{"issueId": q.IssueID, "limit": limit}
			for k, v := range base {
				if k != "issueId" && k != "limit" {
					values[k] = v
				}
			}
			if early {
				values["orderBy"] = "updatedAt"
			}
			if cursor != "" {
				values["cursor"] = cursor
			}
			m, e := p.invoke(ctx, name, values)
			return m, limit, e
		})
		var pe *Error
		switch {
		case e == nil:
		case errors.Is(e, context.Canceled) || errors.Is(ctx.Err(), context.Canceled):
			return Comments{}, context.Canceled
		case errors.As(e, &pe) && pe.Kind == "result_limit":
			// The issue and the rows already read are kept; the failed call
			// marked the connection broken, so the pool does not reuse it.
			stop("result_limit", true)
			break walk
		case errors.Is(e, context.DeadlineExceeded):
			stop("deadline", true)
			break walk
		case pages > 0:
			stop("page_failed", true)
			break walk
		default:
			return Comments{}, e
		}
		rows, ok := m["comments"].([]any)
		if !ok || len(rows) > limit {
			if pages > 0 {
				stop("page_failed", true)
				break
			}
			return Comments{}, fail("invalid_response")
		}
		rowsRead += len(rows)
		older := len(rows) > 0
		for _, v := range rows {
			row, ok := v.(map[string]any)
			if !ok {
				out.Skipped++
				older = false
				continue
			}
			if e = commentIssueRef(row, q.IssueID, q.Identifier); e != nil {
				return Comments{}, e
			}
			c, key, ok := parseComment(row)
			if !ok {
				out.Skipped++
				older = false
				continue
			}
			// The drift guard: a row later than the one before it on the
			// walk's key means the order is not newest first, so no page
			// can end the walk early.
			ordered := c.CreatedAt
			if out.Order == "updatedAt" {
				ordered = c.UpdatedAt
			}
			at, _ := UpdatedAfterTime(ordered)
			if !last.IsZero() && at.After(last) {
				out.WalkDirection = "not_desc"
			}
			last = at
			older = older && key.Before(cutoff)
			if prior, dup := seen[c.ID]; dup {
				if commentEqual(prior.c, c) {
					continue
				}
				// A row seen twice with different content means the walk was
				// not stable, so another row may have been missed.
				stop("duplicate_rows", false)
				if !key.After(prior.key) {
					continue
				}
				bodyBytes -= len(prior.c.UntrustedBody)
			}
			if bodyBytes += len(c.UntrustedBody); bodyBytes > commentBodyBudgetV2 {
				stop("result_limit", true)
				break walk
			}
			seen[c.ID] = seenComment{c, key}
		}
		// Only the documented continuation is followed: hasNextPage plus the
		// response's cursor. Anything else ends the walk incomplete.
		more, known := m["hasNextPage"].(bool)
		if known && !more {
			break
		}
		if early && older && out.WalkDirection == "desc" {
			out.StoppedEarly = true
			break
		}
		next, _ := m["cursor"].(string)
		if !known || next == "" || next == cursor || len(next) > 2048 {
			stop("pagination_unknown", true)
			break
		}
		cursor = next
	}
	// A walk as long as the lag could have outrun it, so its mark is withheld.
	if out.WatermarkReason == "" && commentElapsed(started) >= commentWatermarkLag {
		stop("slow_walk", false)
	}
	if out.Skipped > 0 {
		stop("malformed_rows", false)
	}
	keep := []seenComment{}
	var newest time.Time
	for _, s := range seen {
		if s.key.After(newest) {
			newest = s.key
		}
		if !s.key.Before(q.Since.at) {
			keep = append(keep, s)
		}
	}
	sort.Slice(keep, func(i, j int) bool {
		if !keep[i].key.Equal(keep[j].key) {
			return keep[i].key.Before(keep[j].key)
		}
		return keep[i].c.ID < keep[j].c.ID
	})
	for _, k := range keep {
		out.Items = append(out.Items, k.c)
	}
	out.Complete = out.WatermarkReason == ""
	if out.Complete {
		// The mark never passes the newest comment seen, nor now, minus the
		// lag; it may move back from the caller's mark, which only repeats.
		mark, ceiling := q.Since.at, commentNow().Add(-commentWatermarkLag)
		if lagged := newest.Add(-commentWatermarkLag); len(seen) > 0 && lagged.After(mark) {
			mark = lagged
		}
		if mark.After(ceiling) {
			mark = ceiling
		}
		out.Watermark = encodeCommentWatermark(q.IssueID, mark)
	}
	out.OrderDirection = out.WalkDirection
	return out, nil
}

func commentEqual(a, b Comment) bool {
	same := func(x, y *string) bool { return (x == nil) == (y == nil) && (x == nil || *x == *y) }
	pa, pb, ra, rb := a.ParentID, b.ParentID, a.ResolvedAt, b.ResolvedAt
	a.ParentID, b.ParentID, a.ResolvedAt, b.ResolvedAt = nil, nil, nil, nil
	return a == b && same(pa, pb) && same(ra, rb)
}

// commentIssueRef accepts a row that names no issue. A row that names one,
// flat (issueId) or nested (issue), must name the proven issue by its UUID or
// display identifier; any other present value refuses the read.
func commentIssueRef(row map[string]any, issue, identifier string) error {
	check := func(v any) error {
		if s, ok := v.(string); ok && ((ValidID(s) && s == issue) || (identifier != "" && s == identifier)) {
			return nil
		}
		return fail("scope_mismatch")
	}
	if v, present := row["issueId"]; present && v != nil {
		if e := check(v); e != nil {
			return e
		}
	}
	if v, present := row["issue"]; present && v != nil {
		nested, ok := v.(map[string]any)
		if !ok {
			return fail("scope_mismatch")
		}
		named := false
		for _, key := range []string{"id", "uuid", "identifier"} {
			if x, present := nested[key]; present && x != nil {
				named = true
				if e := check(x); e != nil {
					return e
				}
			}
		}
		if !named {
			return fail("scope_mismatch")
		}
	}
	return nil
}

// parseComment ignores unknown fields. ok is false only when the id, body or
// either time is missing or malformed; the returned key is the later of
// createdAt and updatedAt (Linear can report updatedAt slightly before
// createdAt on a new comment). An author or onBehalfOf of unexpected shape
// degrades to no author and unknown kind rather than dropping the comment.
func parseComment(row map[string]any) (Comment, time.Time, bool) {
	cid, _ := row["id"].(string)
	body, bodyOK := row["body"].(string)
	created, _ := row["createdAt"].(string)
	updated, _ := row["updatedAt"].(string)
	createdAt, cOK := UpdatedAfterTime(created)
	updatedAt, uOK := UpdatedAfterTime(updated)
	if !ValidID(cid) || !bodyOK || !cOK || !uOK {
		return Comment{}, time.Time{}, false
	}
	key := createdAt
	if updatedAt.After(key) {
		key = updatedAt
	}
	c := Comment{ID: cid, CreatedAt: created, UpdatedAt: updated, AuthorKind: "unknown"}
	if author, ok := userRef(row["author"]); ok {
		c.AuthorID = author
	}
	if raw := row["onBehalfOf"]; raw != nil {
		// Documented: when set, the author is an agent acting for this user.
		if behalf, ok := userRef(raw); ok {
			c.AuthorKind, c.OnBehalfOfID = "agent", behalf
		}
	}
	if parent, ok := row["parentId"].(string); ok && ValidID(parent) {
		c.ParentID = &parent
	}
	if resolved, ok := row["resolvedAt"].(string); ok {
		if _, valid := UpdatedAfterTime(resolved); valid {
			c.ResolvedAt = &resolved
		}
	}
	if _, ok := row["quotedText"].(string); ok {
		c.Inline = true
	}
	if len(body) > commentBodyBytes {
		cut := commentBodyBytes
		for cut > 0 && !utf8.RuneStart(body[cut]) {
			cut--
		}
		body, c.BodyTruncated = body[:cut], true
	}
	c.UntrustedBody = body
	return c, key, true
}

// userRef reads a user given as a UUID string or an object whose id is a
// UUID. The name and any other field are ignored.
func userRef(raw any) (string, bool) {
	switch v := raw.(type) {
	case string:
		return v, ValidID(v)
	case map[string]any:
		text, ok := v["id"].(string)
		return text, ok && ValidID(text)
	}
	return "", false
}
