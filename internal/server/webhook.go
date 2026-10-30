package server

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"log"
	"mime"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"clawbridge/internal/bus"
)

// Linear webhooks (FR14, live/FR14-WEBHOOKS-DESIGN.md §2): Linear posts a
// signed delivery here when an issue or a comment changes. A verified one
// is a doorbell: the bus records it and marks the concerned watch cursors
// due; polling stays the record of progress. The body is data, never
// instructions: only IDs, kinds and times are decoded (R4).

// LinearWebhookPath is the prefix of the route Linear's webhooks post to,
// /linear/webhook/TEAM_UUID: each team's webhook URL names its team, so a
// delivery is checked against that team's secrets only. It is not a
// ClawBridge API call, so it is not under /v1/.
const LinearWebhookPath = "/linear/webhook/"

const (
	// webhookBodyCap is the largest body read; a larger one is refused
	// before any HMAC (Q2).
	webhookBodyCap = 1 << 20
	// webhookWindow is how far webhookTimestamp may be from the clock (Q3).
	webhookWindow = bus.WebhookWindow
	// webhookRejectsPerMinute is each address's refused requests a minute;
	// verified deliveries never count (C1).
	webhookRejectsPerMinute = 30
	// webhookVerifiedPerMinute is the server-wide ceiling on verified
	// deliveries (8a).
	webhookVerifiedPerMinute = 600
	// webhookOrgCacheFor is how long the app's Linear org ID is kept.
	webhookOrgCacheFor = 10 * time.Minute
	// webhookFlushEvery is the least time between two flushes of the
	// unattributed counters.
	webhookFlushEvery = time.Minute
	// webhookRetryAfter is the Retry-After of a 503 that isn't a drain.
	webhookRetryAfter = 5
	// webhookTimeout bounds a delivery's org read and record.
	webhookTimeout = 10 * time.Second
	// The secrets' limits (CLAWBRIDGE_LINEAR_WEBHOOK_SECRETS).
	webhookSecretsPerTeam = 2
	webhookSecretsTotal   = 32
	webhookSecretMin      = 16
	webhookSecretMax      = 256
)

// WebhookStore records verified deliveries and counts refusals.
type WebhookStore interface {
	WebhookRecord(ctx context.Context, d bus.WebhookDelivery) (bus.WebhookResult, error)
	WebhookCount(ctx context.Context, team, reason string, n int, now time.Time) error
}

var _ WebhookStore = (*bus.Store)(nil)

// WebhookOptions serve POST /linear/webhook. The route exists only with at
// least one secret and OrgID (the Linear app configured).
type WebhookOptions struct {
	// Secrets maps each lower-case team UUID to its 1..2 signing secrets
	// (two during a rotation).
	Secrets map[string][]string
	// OrgID is the app's Linear organization ID, cached.
	OrgID func(ctx context.Context) (string, error)
	// Nudge asks for an early pass of a project whose cursors a delivery
	// marked; nil sends none. It runs after the answer and never blocks it.
	Nudge func(projectID string)
	// flush is where the route registers its counter flush, for the
	// sweeper; nil registers nothing.
	flush *webhookFlush
}

// webhookFlush hands the route's in-memory counters to the sweeper, which
// flushes them even when no later request does: a lone refusal reaches the
// database within about a minute.
type webhookFlush struct {
	mu sync.Mutex
	h  *webhookHandler
}

func (f *webhookFlush) set(h *webhookHandler) {
	f.mu.Lock()
	f.h = h
	f.mu.Unlock()
}

// FlushCounts flushes the route's counters when they are due; before the
// route exists it does nothing.
func (f *webhookFlush) FlushCounts(ctx context.Context) {
	f.mu.Lock()
	h := f.h
	f.mu.Unlock()
	if h != nil {
		h.FlushCounts(ctx)
	}
}

// webhookMAC makes the HMAC a signature is checked with; tests count it.
var webhookMAC = func(key []byte) hash.Hash { return hmac.New(sha256.New, key) }

type webhookHandler struct {
	store   WebhookStore
	secrets map[string][][]byte // lower team UUID -> its 1..2 secrets
	orgID   func(context.Context) (string, error)
	nudge   func(string)
	now     func() time.Time
	drain   *Drain
	rejects *preauthLimiter
	// spawn runs a counter flush off the request; tests run it inline.
	spawn func(func())

	mu            sync.Mutex
	verifiedStart time.Time
	verified      int
	// counts are the refusals not yet flushed, by team ("" when
	// unattributed) and reason (Ruling: in memory, at most one write a
	// minute, so a flood writes nothing per request; C2). Only
	// WebhookRecord writes per request.
	counts    map[webhookCountKey]int
	flushedAt time.Time
	flushing  bool
}

func newWebhookHandler(store WebhookStore, o *WebhookOptions, now func() time.Time, drain *Drain) *webhookHandler {
	if now == nil {
		now = time.Now
	}
	h := &webhookHandler{store: store, orgID: o.OrgID, nudge: o.Nudge, now: now, drain: drain,
		rejects: newPreauthLimiter(webhookRejectsPerMinute, now), spawn: func(f func()) { go f() },
		counts: map[webhookCountKey]int{}, flushedAt: now(), secrets: map[string][][]byte{}}
	for team, list := range o.Secrets {
		for _, s := range list {
			h.secrets[strings.ToLower(team)] = append(h.secrets[strings.ToLower(team)], []byte(s))
		}
	}
	if o.flush != nil {
		o.flush.set(h)
	}
	return h
}

func (h *webhookHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		fail(w, 405, "method_not_allowed", "POST required")
		return
	}
	// 1. An address over its refusals is refused before anything else.
	key := addressKey(r)
	if h.rejects.blocked(key) {
		h.unattributed("rate_limited")
		fail(w, 429, "rate_limited", "too many refused webhook deliveries from this address; try again later")
		return
	}
	// 2. A drain answers before the body is read.
	if h.drain != nil {
		if retry, on := h.drain.Check(r.Context()); on {
			failMaintenance(w, retry)
			return
		}
	}
	// refuse is a refusal before the HMAC verified: only those count
	// against the address (C1). After it, the sender is Linear itself (a
	// stale retry after an outage, say), and throttling its address would
	// refuse valid deliveries.
	refuse := func(status int, code, message string) {
		h.rejects.reject(key)
		fail(w, status, code, message)
	}
	// 3.
	if media, _, err := mime.ParseMediaType(r.Header.Get("Content-Type")); err != nil || media != "application/json" {
		refuse(415, "unsupported_media_type", "Content-Type must be application/json")
		return
	}
	// 4. The cap, before any HMAC.
	if r.ContentLength > webhookBodyCap {
		h.unattributed("too_large")
		refuse(413, "too_large", "the delivery is larger than 1 MiB")
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, webhookBodyCap))
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			h.unattributed("too_large")
			refuse(413, "too_large", "the delivery is larger than 1 MiB")
			return
		}
		refuse(400, "invalid", "the delivery could not be read")
		return
	}
	// 5 and 6, under the path's team's secrets only. A team that isn't
	// configured is answered exactly as a bad signature, with no MAC, so
	// the route never tells which teams are.
	team := strings.TrimPrefix(r.URL.Path, LinearWebhookPath)
	if !h.verify(team, r.Header.Values("Linear-Signature"), body) {
		h.unattributed("signature")
		refuse(401, "signature", "the delivery's signature does not verify")
		return
	}
	// 7. The typed subset only; the raw bytes go no further than the
	// dedup key and their length (the probe's evidence on the cap, Q2).
	d, ok := decodeWebhook(body)
	sum := sha256.Sum256(body)
	d.Size = len(body)
	body = nil
	if !ok {
		h.tally(team, "bad_payload")
		fail(w, 400, "bad_payload", "the delivery is not a Linear webhook payload")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), webhookTimeout)
	defer cancel()
	org, err := h.orgID(ctx)
	if err != nil {
		w.Header().Set("Retry-After", strconv.Itoa(webhookRetryAfter))
		fail(w, 503, "unavailable", "the Linear app's identity is unavailable; retry later")
		return
	}
	if d.org != org {
		h.tally(team, "org_mismatch")
		fail(w, 401, "org_mismatch", "the delivery is from another Linear organization")
		return
	}
	// A delivery without a team (a comment may carry none; P1 verifies)
	// is its secret's team's.
	if d.TeamID == "" {
		d.TeamID = team
	}
	if d.TeamID != team {
		h.tally(team, "team_mismatch")
		fail(w, 401, "team_mismatch", "the delivery's team is not its secret's team")
		return
	}
	// 8. Freshness, before dedup: the replay defence.
	now := h.now()
	if gap := now.Sub(d.LinearAt); gap > webhookWindow || gap < -webhookWindow {
		h.tally(team, "stale")
		fail(w, 401, "stale", "the delivery's webhookTimestamp is outside the window")
		return
	}
	// 8a. The server-wide ceiling: not a refusal of the address.
	if !h.admit(now) {
		h.tally(team, "verified_throttled")
		w.Header().Set("Retry-After", "60")
		fail(w, 429, "verified_throttled", "too many webhook deliveries this minute; retry later")
		return
	}
	// 9 and 10. Dedup and the hint commit together in the bus.
	d.DeliveryID = webhookDeliveryID(r.Header.Values("Linear-Delivery"), sum)
	d.ReceivedAt = now
	res, err := h.store.WebhookRecord(ctx, d.WebhookDelivery)
	if err != nil {
		log.Print("linear webhooks: a delivery could not be recorded; Linear retries it")
		w.Header().Set("Retry-After", strconv.Itoa(webhookRetryAfter))
		fail(w, 503, "unavailable", "the delivery could not be recorded; retry later")
		return
	}
	// 11. The answer, then the nudges, which never hold it.
	w.WriteHeader(http.StatusOK)
	if h.nudge != nil && res.Outcome == "applied" {
		for _, pid := range res.Projects {
			go h.nudge(pid)
		}
	}
}

// webhookDeliveryID is the dedup key: Linear's one Linear-Delivery UUID,
// lower-cased, or, without a well-formed one, "sha256:" and the body's
// hash. The header catches a retry with a fresh webhookTimestamp, which
// changes the body.
func webhookDeliveryID(headers []string, sum [sha256.Size]byte) string {
	if len(headers) == 1 {
		if id := strings.ToLower(strings.TrimSpace(headers[0])); webhookUUID.MatchString(id) {
			return id
		}
	}
	return "sha256:" + hex.EncodeToString(sum[:])
}

// verify checks the one hex Linear-Signature against each of team's
// secrets, every one every time, in constant time. A team with no secret
// (not configured, or not a lower-case UUID) verifies nothing.
func (h *webhookHandler) verify(team string, headers []string, body []byte) bool {
	keys := h.secrets[team]
	if len(keys) == 0 || len(headers) != 1 {
		return false
	}
	sig, err := hex.DecodeString(strings.TrimSpace(headers[0]))
	if err != nil || len(sig) != sha256.Size {
		return false
	}
	matched := false
	for _, key := range keys {
		m := webhookMAC(key)
		m.Write(body)
		if hmac.Equal(m.Sum(nil), sig) {
			matched = true
		}
	}
	return matched
}

// admit counts one verified delivery against the minute's ceiling.
func (h *webhookHandler) admit(now time.Time) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if now.Sub(h.verifiedStart) >= time.Minute || now.Before(h.verifiedStart) {
		h.verifiedStart, h.verified = now, 0
	}
	if h.verified >= webhookVerifiedPerMinute {
		return false
	}
	h.verified++
	return true
}

// webhookCountKey is a counter: team "" is unattributed.
type webhookCountKey struct{ team, reason string }

// unattributed counts a refusal no team is known for, in memory.
func (h *webhookHandler) unattributed(reason string) { h.tally("", reason) }

// tally counts one refusal under team ("" unattributed) in memory; a minute after the last flush, it flushes
// off the request.
func (h *webhookHandler) tally(team, reason string) {
	h.mu.Lock()
	h.counts[webhookCountKey{team, reason}]++
	due := !h.flushing && h.now().Sub(h.flushedAt) >= webhookFlushEvery
	if due {
		h.flushing = true
	}
	h.mu.Unlock()
	if due {
		h.spawn(func() {
			ctx, cancel := context.WithTimeout(context.Background(), webhookTimeout)
			defer cancel()
			h.flushCounts(ctx)
		})
	}
}

// FlushCounts flushes the counters a minute after the last flush, when
// there are any and no flush runs: the sweeper's call (FR14 T3).
func (h *webhookHandler) FlushCounts(ctx context.Context) {
	h.mu.Lock()
	due := !h.flushing && len(h.counts) > 0 && h.now().Sub(h.flushedAt) >= webhookFlushEvery
	if due {
		h.flushing = true
	}
	h.mu.Unlock()
	if due {
		h.flushCounts(ctx)
	}
}

// flushCounts writes the counters, one call per team and reason. What a
// failed write held is lost: they are 24-hour warning counters.
func (h *webhookHandler) flushCounts(ctx context.Context) {
	h.mu.Lock()
	counts, now := h.counts, h.now()
	h.counts, h.flushedAt, h.flushing = map[webhookCountKey]int{}, now, true
	h.mu.Unlock()
	failed := false
	for k, n := range counts {
		if err := h.store.WebhookCount(ctx, k.team, k.reason, n, now); err != nil {
			failed = true
		}
	}
	if failed {
		log.Print("linear webhooks: the refusal counters could not be written")
	}
	h.mu.Lock()
	h.flushing = false
	h.mu.Unlock()
}

// webhookDecoded is a delivery's typed fields and its organization.
type webhookDecoded struct {
	bus.WebhookDelivery
	org string
}

// present records that a key is there and drops its value unread.
type present bool

func (p *present) UnmarshalJSON([]byte) error { *p = true; return nil }

var (
	webhookUUID = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
	webhookWord = regexp.MustCompile(`^[A-Za-z]{1,64}$`)
)

// decodeWebhook decodes the typed subset (R4): no title, body, actor or
// URL is decoded, and updatedFrom's values are dropped unread (C5). Every
// ID must be a UUID; ok is false otherwise.
func decodeWebhook(body []byte) (webhookDecoded, bool) {
	var p struct {
		Type           string `json:"type"`
		Action         string `json:"action"`
		OrganizationID string `json:"organizationId"`
		Timestamp      int64  `json:"webhookTimestamp"`
		WebhookID      string `json:"webhookId"`
		Data           struct {
			ID         string  `json:"id"`
			TeamID     string  `json:"teamId"`
			IssueID    string  `json:"issueId"`
			LabelIDs   present `json:"labelIds"`
			DelegateID present `json:"delegateId"`
		} `json:"data"`
		UpdatedFrom map[string]json.RawMessage `json:"updatedFrom"`
	}
	if json.Unmarshal(body, &p) != nil {
		return webhookDecoded{}, false
	}
	lower := strings.ToLower
	org, team, entity, issue, hook := lower(p.OrganizationID), lower(p.Data.TeamID), lower(p.Data.ID), lower(p.Data.IssueID), lower(p.WebhookID)
	if !webhookWord.MatchString(p.Type) || !webhookWord.MatchString(p.Action) || !webhookUUID.MatchString(org) || !webhookUUID.MatchString(entity) ||
		(issue != "" && !webhookUUID.MatchString(issue)) || (hook != "" && !webhookUUID.MatchString(hook)) || (team != "" && !webhookUUID.MatchString(team)) {
		return webhookDecoded{}, false
	}
	keys := []string{}
	for k := range p.UpdatedFrom {
		if webhookWord.MatchString(k) && len(keys) < 64 {
			keys = append(keys, k)
		}
	}
	p.UpdatedFrom = nil
	// A create carries its set keys, by presence only.
	if p.Action == "create" {
		if p.Data.LabelIDs {
			keys = append(keys, "labelIds")
		}
		if p.Data.DelegateID {
			keys = append(keys, "delegateId")
		}
	}
	sort.Strings(keys)
	return webhookDecoded{WebhookDelivery: bus.WebhookDelivery{
		TeamID: team, WebhookID: hook, EventType: p.Type, Action: p.Action, EntityID: entity, IssueID: issue,
		UpdatedKeys: keys, LinearAt: time.UnixMilli(p.Timestamp).UTC(),
	}, org: org}, true
}

// cachedOrgID keeps the app's Linear org ID for webhookOrgCacheFor; a
// failed or malformed read is not kept.
func cachedOrgID(fetch func(context.Context) (string, error), now func() time.Time) func(context.Context) (string, error) {
	var mu sync.Mutex
	id, at := "", time.Time{}
	return func(ctx context.Context) (string, error) {
		mu.Lock()
		if id != "" && now().Sub(at) < webhookOrgCacheFor {
			v := id
			mu.Unlock()
			return v, nil
		}
		mu.Unlock()
		v, err := fetch(ctx)
		v = strings.ToLower(v)
		if err != nil || !webhookUUID.MatchString(v) {
			return "", errors.New("the Linear app's identity is unavailable")
		}
		mu.Lock()
		id, at = v, now()
		mu.Unlock()
		return v, nil
	}
}

// parseWebhookSecrets reads CLAWBRIDGE_LINEAR_WEBHOOK_SECRETS:
// TEAM_UUID=SECRET entries, comma-separated, at most 2 per team and 32 in
// all. A secret is 16-256 printable ASCII characters with no space or
// comma, and names one team. Errors name the entry, never its value.
func parseWebhookSecrets(raw string) (map[string][]string, error) {
	out := map[string][]string{}
	if raw == "" {
		return out, nil
	}
	entries := strings.Split(raw, ",")
	if len(entries) > webhookSecretsTotal {
		return nil, fmt.Errorf("CLAWBRIDGE_LINEAR_WEBHOOK_SECRETS holds at most %d secrets", webhookSecretsTotal)
	}
	seen := map[string]bool{}
	for i, entry := range entries {
		team, secret, ok := strings.Cut(entry, "=")
		team = strings.ToLower(team)
		if !ok || !webhookUUID.MatchString(team) {
			return nil, fmt.Errorf("CLAWBRIDGE_LINEAR_WEBHOOK_SECRETS entry %d is not TEAM_UUID=SECRET", i+1)
		}
		if len(secret) < webhookSecretMin || len(secret) > webhookSecretMax {
			return nil, fmt.Errorf("CLAWBRIDGE_LINEAR_WEBHOOK_SECRETS entry %d: a secret is %d-%d characters", i+1, webhookSecretMin, webhookSecretMax)
		}
		for _, c := range []byte(secret) {
			if c <= ' ' || c > '~' {
				return nil, fmt.Errorf("CLAWBRIDGE_LINEAR_WEBHOOK_SECRETS entry %d: a secret is printable ASCII with no space", i+1)
			}
		}
		if seen[secret] {
			return nil, fmt.Errorf("CLAWBRIDGE_LINEAR_WEBHOOK_SECRETS entry %d repeats a secret; each secret names one team", i+1)
		}
		seen[secret] = true
		if len(out[team]) >= webhookSecretsPerTeam {
			return nil, fmt.Errorf("CLAWBRIDGE_LINEAR_WEBHOOK_SECRETS entry %d: at most %d secrets per team", i+1, webhookSecretsPerTeam)
		}
		out[team] = append(out[team], secret)
	}
	return out, nil
}

// webhookStartStore is what a start records about webhooks.
type webhookStartStore interface {
	WebhookTeamsConfigured(ctx context.Context, teams []string, now time.Time) error
	WebhookGateOn(ctx context.Context, now time.Time) error
}

// webhookStartup records the configured teams (clearing those no longer
// configured) and the gate's going on, and returns the route's options, or
// nil without secrets. Secrets without the Linear app refuse the start. It
// logs the team count only.
func webhookStartup(ctx context.Context, store webhookStartStore, secrets map[string][]string, proven bool, orgID func(context.Context) (string, error), now time.Time) (*WebhookOptions, error) {
	if len(secrets) > 0 && orgID == nil {
		return nil, errors.New("CLAWBRIDGE_LINEAR_WEBHOOK_SECRETS is set but the Linear app is not configured")
	}
	teams := make([]string, 0, len(secrets))
	for team := range secrets {
		teams = append(teams, team)
	}
	sort.Strings(teams)
	if err := store.WebhookTeamsConfigured(ctx, teams, now); err != nil {
		return nil, errors.New("cannot record the Linear webhook teams")
	}
	if proven {
		if err := store.WebhookGateOn(ctx, now); err != nil {
			return nil, errors.New("cannot record the webhooks gate")
		}
	}
	log.Printf("linear webhooks: %d teams configured", len(teams))
	if len(teams) == 0 {
		return nil, nil
	}
	return &WebhookOptions{Secrets: secrets, OrgID: orgID, flush: &webhookFlush{}}, nil
}

// webhookCatchUpStore is what a drain's end records and marks.
type webhookCatchUpStore interface {
	WebhookDrainEnded(ctx context.Context, now time.Time) error
	WebhookCatchUp(ctx context.Context, now time.Time) ([]string, error)
}

// webhookDrainEnd is drain.onEnd (§6 P6): it wakes the streams (endDrain),
// records the drain's end, so no miss counts for an event of the drain,
// marks every live team's cursors due once, and nudges each project the
// catch-up moved. nudge is read at the end, since the watcher starts after
// the drain is wired; with none the next tick polls the due cursors. The
// marks bound a refused delivery's delay to the resume grace and a pass.
func webhookDrainEnd(endDrain func(), store webhookCatchUpStore, nudge func() func(string), now func() time.Time) func() {
	return func() {
		endDrain()
		ctx, cancel := context.WithTimeout(context.Background(), webhookTimeout)
		defer cancel()
		at := now()
		if err := store.WebhookDrainEnded(ctx, at); err != nil {
			log.Print("linear webhooks: the drain's end could not be recorded")
		}
		projects, err := store.WebhookCatchUp(ctx, at)
		if err != nil {
			log.Print("linear webhooks: the drain's catch-up failed; the next polls catch up")
			return
		}
		if send := nudge(); send != nil {
			for _, pid := range projects {
				send(pid)
			}
		}
	}
}
