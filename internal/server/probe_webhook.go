package server

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"sort"
	"strings"
	"time"

	"clawbridge/internal/bus"
	"clawbridge/internal/linearapp"
	"clawbridge/internal/linearprovider"
)

// The webhook probe (live/FR14-WEBHOOKS-DESIGN.md §9): the host operator's
// proof that Linear's deliveries reach the route, verify and are recorded,
// on a probe-only team bound to a probe-only project.
//
//   - P1, the round trip: as the app, one issue in --team, a comment, the
//     needs-agent label added and removed, and the issue archived; then up
//     to two minutes for their deliveries in webhook_deliveries. It also
//     tries Linear's read-only webhooks query once (Q7).
//   - P2 and P3(a), --forged: five forged or expired bodies POSTed to the
//     public route must be refused 401, 401, 401, 401 and 413, and counted,
//     with no row; one good body sent twice under one Linear-Delivery is
//     one row. It writes nothing to Linear.
//
// It prints classes only: never a secret, body, ID or text. The one
// exception is the probe issue's ID when its archive failed, so the owner
// can archive it by hand.

const (
	// webhookProbeTitle and webhookProbeText are the probe issue's title,
	// and its description and comment.
	webhookProbeTitle = "ClawBridge webhook probe, will be archived"
	webhookProbeText  = "ClawBridge webhook probe: an issue, a comment and a label change, to prove Linear's webhook deliveries. It is archived at once."
	// webhookProbeLabel is the label added and removed.
	webhookProbeLabel = "needs-agent"
	// webhookProbeWait bounds P1's wait for its deliveries; webhookProbePoll
	// is how often it reads them.
	webhookProbeWait = 2 * time.Minute
	webhookProbePoll = 5 * time.Second
	// webhookProbeStep is the pause between two of P1's writes, so each is
	// its own delivery.
	webhookProbeStep = 2 * time.Second
	// webhookProbeCountWait is how long --forged waits before it checks the
	// counters: the route flushes them at most once a minute.
	webhookProbeCountWait = webhookFlushEvery + 5*time.Second
	// webhookProbeFlushSettle is the wait after the flushing request: the
	// route writes its counters off the request.
	webhookProbeFlushSettle = 3 * time.Second
)

// webhookProbeQuiet is printed with every round trip: its events are told
// apart by kind, keys and order, never by ID.
const webhookProbeQuiet = "deliveries are matched to events by event type, updatedFrom keys and order, so this assumes a quiet probe team: no other edit in it during the 2-minute wait"

// webhooksQuery is Q7's read: whether the app may list webhooks without
// admin rights. Nothing depends on the answer; no field is printed.
const webhooksQuery = `query { webhooks(first: 1) { nodes { id } } }`

// webhookProbeApp is what the probe asks of the Linear app.
type webhookProbeApp interface {
	OrgID(ctx context.Context) (string, error)
	CreateIssue(ctx context.Context, team, title, description string) (string, error)
	Comment(ctx context.Context, issue, body string) error
	NeedsAgentLabel(ctx context.Context, team string) (string, error)
	SetLabel(ctx context.Context, issue, label string, add bool) error
	ArchiveIssue(ctx context.Context, issue string) (string, error)
	WebhooksQuery(ctx context.Context) error
	Close() error
}

// webhookProbeStore is what it reads of the bus.
type webhookProbeStore interface {
	WebhookProbeDeliveries(ctx context.Context, team string, since time.Time) ([]bus.WebhookProbeRow, error)
	WebhookProbeCounts(ctx context.Context, team string, since time.Time) (map[string]map[string]int, error)
}

var _ webhookProbeStore = (*bus.Store)(nil)

// appWebhookProbe is the app with its MCP connection, opened on the first
// write.
type appWebhookProbe struct {
	app      *linearapp.App
	provider *linearprovider.Provider
}

// newWebhookProbeApp builds the app identity for the probe; tests replace
// it. Like newProbeApp, it never mints.
var newWebhookProbeApp = func(store linearapp.Store) (webhookProbeApp, error) {
	app, err := newProbeApp(store)
	if err != nil {
		return nil, err
	}
	a, ok := app.(*linearapp.App)
	if !ok {
		return nil, errors.New("the Linear app cannot write issues")
	}
	return &appWebhookProbe{app: a}, nil
}

func (a *appWebhookProbe) mcp(ctx context.Context) (*linearprovider.Provider, error) {
	if a.provider == nil {
		p, err := a.app.Provider(ctx, false)
		if err != nil {
			return nil, err
		}
		a.provider = p
	}
	return a.provider, nil
}

func (a *appWebhookProbe) OrgID(ctx context.Context) (string, error) {
	id, err := a.app.Identity(ctx)
	return strings.ToLower(id.LinearOrgID), err
}

func (a *appWebhookProbe) CreateIssue(ctx context.Context, team, title, description string) (string, error) {
	p, err := a.mcp(ctx)
	if err != nil {
		return "", err
	}
	issue, err := p.Create(ctx, linearprovider.CreateInput{TeamID: team, Title: title, Description: description})
	if err != nil {
		return "", err
	}
	if !linearprovider.ValidID(strings.ToLower(issue.ID)) {
		return "", errors.New("the created issue has no ID")
	}
	return strings.ToLower(issue.ID), nil
}

func (a *appWebhookProbe) Comment(ctx context.Context, issue, body string) error {
	p, err := a.mcp(ctx)
	if err != nil {
		return err
	}
	_, err = p.Comment(ctx, issue, body)
	return err
}

// NeedsAgentLabel is the team's one usable needs-agent label.
func (a *appWebhookProbe) NeedsAgentLabel(ctx context.Context, team string) (string, error) {
	p, err := a.mcp(ctx)
	if err != nil {
		return "", err
	}
	labels, _, err := p.TeamLabelsNamed(ctx, team, webhookProbeLabel)
	if err != nil {
		return "", err
	}
	found := ""
	for _, l := range labels {
		if l.Group || l.Retired {
			continue
		}
		if found != "" {
			return "", errors.New("the team has two needs-agent labels")
		}
		found = strings.ToLower(l.ID)
	}
	if found == "" {
		return "", errors.New("the team has no needs-agent label")
	}
	return found, nil
}

func (a *appWebhookProbe) SetLabel(ctx context.Context, issue, label string, add bool) error {
	p, err := a.mcp(ctx)
	if err != nil {
		return err
	}
	in := linearprovider.UpdateInputV2{UpdateInput: linearprovider.UpdateInput{ID: issue}}
	if add {
		in.AddLabels = []string{label}
	} else {
		in.RemoveLabels = []string{label}
	}
	_, err = p.UpdateV2(ctx, in)
	return err
}

func (a *appWebhookProbe) ArchiveIssue(ctx context.Context, issue string) (string, error) {
	return a.app.ArchiveIssue(ctx, issue)
}

func (a *appWebhookProbe) WebhooksQuery(ctx context.Context) error {
	var out struct {
		Webhooks *struct {
			Nodes []struct{ ID string }
		}
	}
	if err := a.app.GraphQL(ctx, webhooksQuery, nil, &out); err != nil {
		return err
	}
	if out.Webhooks == nil {
		return errors.New("no webhooks field")
	}
	return nil
}

func (a *appWebhookProbe) Close() error {
	if a.provider != nil {
		return a.provider.Close()
	}
	return nil
}

// webhookProbeConfig is one run's settings; tests set the clock, sleep and
// client.
type webhookProbeConfig struct {
	team   string // lower-case UUID
	forged bool
	secret string // --forged: the team's signing secret, from the server's env
	origin string // --forged: the public origin, https://HOST
	client *http.Client
	now    func() time.Time
	sleep  func(ctx context.Context, d time.Duration) error
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// runLinearWebhookProbe is operator linear-webhook-probe: it parses and
// checks the flags, refuses without --confirm before anything is opened,
// and reads the team's secret from the server's own env, never from an
// argument.
func runLinearWebhookProbe(ctx context.Context, args []string, stdout io.Writer) error {
	f := flag.NewFlagSet("operator linear-webhook-probe", flag.ContinueOnError)
	f.SetOutput(io.Discard)
	confirm := f.Bool("confirm", false, "confirm the probe's writes")
	team := f.String("team", "", "the probe-only Linear team's UUID")
	forged := f.Bool("forged", false, "P2 and P3(a): POST forged, expired and repeated bodies to the public route; no Linear write")
	origin := f.String("public-origin", "", "with --forged: the server's public origin, https://HOST")
	dsnEnv := f.String("database-url-env", "CLAWBRIDGE_DATABASE_URL", "environment variable holding the Postgres connection URL")
	if err := f.Parse(args); err != nil || f.NArg() > 0 {
		return errors.New("usage: clawbridge operator linear-webhook-probe --confirm --team UUID [--forged --public-origin https://HOST]")
	}
	cfg := webhookProbeConfig{team: strings.ToLower(*team), forged: *forged, now: time.Now, sleep: sleepCtx}
	if !*confirm {
		if cfg.forged {
			return errors.New("linear-webhook-probe --forged POSTs five forged or expired bodies and one good body twice to the public webhook route, from this host; it writes nothing to Linear; repeat with --confirm")
		}
		return errors.New("linear-webhook-probe creates one issue in the probe team as the Linear app, comments on it, adds and removes needs-agent, archives it, and waits up to 2 minutes for the deliveries; keep the probe team quiet meanwhile (no other edit in it), as deliveries are matched by event type, keys and order; repeat with --confirm")
	}
	if !webhookUUID.MatchString(cfg.team) {
		return errors.New("linear-webhook-probe needs --team, the probe team's UUID")
	}
	if cfg.forged {
		if *origin == "" {
			return errors.New("--forged needs --public-origin https://HOST, the server's public origin")
		}
		o, err := ParsePublicOrigin(*origin)
		if err != nil {
			return err
		}
		secrets, err := parseWebhookSecrets(os.Getenv("CLAWBRIDGE_LINEAR_WEBHOOK_SECRETS"))
		if err != nil {
			return err
		}
		if len(secrets[cfg.team]) == 0 {
			return errors.New("CLAWBRIDGE_LINEAR_WEBHOOK_SECRETS holds no secret for --team; run the probe in the server's own environment")
		}
		cfg.origin, cfg.secret = o, secrets[cfg.team][0]
		cfg.client = &http.Client{Timeout: 30 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	} else if *origin != "" {
		return errors.New("--public-origin goes with --forged")
	}
	dsn := os.Getenv(*dsnEnv)
	if dsn == "" {
		return errors.New("Postgres connection environment variable is empty")
	}
	store, err := bus.OpenPostgresOperator(ctx, dsn)
	if err != nil {
		return fmt.Errorf("cannot open Postgres: %w", err)
	}
	defer store.Close()
	app, err := newWebhookProbeApp(store)
	if err != nil {
		return err
	}
	return linearWebhookProbe(ctx, store, app, cfg, stdout)
}

// linearWebhookProbe runs P1, or with cfg.forged P2 and P3(a), and prints
// one object under "webhook_probe".
func linearWebhookProbe(ctx context.Context, store webhookProbeStore, app webhookProbeApp, cfg webhookProbeConfig, stdout io.Writer) (err error) {
	defer func() {
		if e := app.Close(); err == nil && e != nil {
			err = probeFailure("close", e)
		}
	}()
	out := map[string]any{}
	if cfg.forged {
		out["mode"] = "forged"
		err = webhookProbeForged(ctx, store, app, cfg, out)
	} else {
		out["mode"] = "round_trip"
		err = webhookProbeRoundTrip(ctx, store, app, cfg, out)
	}
	if e := json.NewEncoder(stdout).Encode(map[string]any{"webhook_probe": out}); e != nil {
		return e
	}
	return err
}

// webhookProbeEvents are P1's events, in the order the app makes them.
var webhookProbeEvents = []struct{ event, step string }{
	{"issue_created", "create"}, {"comment_created", "comment"}, {"label_added", "label_add"}, {"label_removed", "label_remove"}, {"issue_archived", "archive"},
}

func webhookProbeRoundTrip(ctx context.Context, store webhookProbeStore, app webhookProbeApp, cfg webhookProbeConfig, out map[string]any) error {
	start := cfg.now()
	steps := map[string]string{}
	out["steps"] = steps
	issue, err := app.CreateIssue(ctx, cfg.team, webhookProbeTitle, webhookProbeText)
	steps["create"] = probeClass(err)
	if err != nil {
		out["pass"] = false
		return errors.New("linear-webhook-probe: the app could not create the probe issue (" + steps["create"] + "); nothing else was sent")
	}
	pause := func() error { return cfg.sleep(ctx, webhookProbeStep) }
	if err = pause(); err != nil {
		return err
	}
	steps["comment"] = probeClass(app.Comment(ctx, issue, webhookProbeText))
	label, err := "", pause()
	if err != nil {
		return err
	}
	if label, err = app.NeedsAgentLabel(ctx, cfg.team); err != nil {
		steps["label_add"], steps["label_remove"] = probeClass(err), probeClass(err)
	} else {
		steps["label_add"] = probeClass(app.SetLabel(ctx, issue, label, true))
		if err = pause(); err != nil {
			return err
		}
		steps["label_remove"] = probeClass(app.SetLabel(ctx, issue, label, false))
	}
	if err = pause(); err != nil {
		return err
	}
	archived, err := app.ArchiveIssue(ctx, issue)
	if err != nil {
		archived = probeClass(err)
	}
	steps["archive"] = archived
	var failed error
	if archived != "archived" {
		out["cleanup"] = "the archive failed: the owner archives issue " + issue + " in Linear"
		failed = errors.New("linear-webhook-probe: the probe issue was not archived; archive it by the printed ID")
	}
	if err = app.WebhooksQuery(ctx); err != nil {
		out["webhooks_query_without_admin"] = probeClass(err)
	} else {
		out["webhooks_query_without_admin"] = "ok"
	}
	// The wait: until every sent event's delivery is in, or two minutes.
	expected := []string{}
	for _, e := range webhookProbeEvents {
		if steps[e.step] == "ok" || e.step == "archive" && archived == "archived" {
			expected = append(expected, e.event)
		}
	}
	waitStart := cfg.now()
	var rows []bus.WebhookProbeRow
	var events map[string]bool
	for {
		if rows, err = store.WebhookProbeDeliveries(ctx, cfg.team, start); err != nil {
			return errors.New("linear-webhook-probe: cannot read the deliveries")
		}
		events = webhookProbeAttribute(rows)
		all := true
		for _, e := range expected {
			all = all && events[e]
		}
		if all || cfg.now().Sub(waitStart) >= webhookProbeWait {
			break
		}
		if err = cfg.sleep(ctx, webhookProbePoll); err != nil {
			return err
		}
	}
	out["waited_seconds"] = math.Round(cfg.now().Sub(waitStart).Seconds())
	out["attribution_note"] = webhookProbeQuiet
	evidence := webhookProbeEvidence(rows)
	for k, v := range evidence {
		out[k] = v
	}
	delivered := map[string]bool{}
	// The Linear-Delivery header is evidence, not a condition: without it
	// the route dedups on the body's hash (design amendment A4).
	pass := len(expected) == len(webhookProbeEvents)
	for _, e := range webhookProbeEvents {
		delivered[e.event] = events[e.event]
		pass = pass && events[e.event]
	}
	out["events"] = delivered
	out["pass"] = pass && failed == nil
	return failed
}

// webhookProbeAttribute tells which of P1's events were delivered. A row
// holds no entity ID, so each is told by its kind, its keys and its
// order: an Issue create; a Comment create; an archive is an Issue remove
// or an update whose updatedFrom names archivedAt; the other Issue
// updates are the label's, added first. On a probe-only team, these are
// the probe's own.
func webhookProbeAttribute(rows []bus.WebhookProbeRow) map[string]bool {
	ev := map[string]bool{}
	labels := 0
	for _, r := range rows {
		keys := "," + r.UpdatedKeys + ","
		switch {
		case r.EventType == "Issue" && r.Action == "create":
			ev["issue_created"] = true
		case r.EventType == "Comment" && r.Action == "create":
			ev["comment_created"] = true
		case r.EventType == "Issue" && (r.Action == "remove" || r.Action == "update" && strings.Contains(keys, ",archivedAt,")):
			ev["issue_archived"] = true
		case r.EventType == "Issue" && r.Action == "update":
			labels++
		}
	}
	ev["label_added"], ev["label_removed"] = labels >= 1, labels >= 2
	return ev
}

// webhookProbeEvidence is P1's classes: whether the Linear-Delivery header
// came (a row's key is its UUID, not the body's hash), the field shapes,
// the updatedFrom key names (Q11), the largest body (Q2), the skew and
// the outcomes.
func webhookProbeEvidence(rows []bus.WebhookProbeRow) map[string]any {
	share := func(n int) string {
		switch {
		case len(rows) == 0:
			return "no_deliveries"
		case n == len(rows):
			return "all"
		case n == 0:
			return "none"
		}
		return "some"
	}
	header, hook, kind, action, largest := 0, 0, 0, 0, 0
	keys := map[string]bool{}
	kinds, outcomes := map[string]int{}, map[string]int{}
	skewMin, skewMax := math.Inf(1), math.Inf(-1)
	for _, r := range rows {
		if webhookUUID.MatchString(r.DeliveryID) {
			header++
		}
		if r.WebhookID != "" {
			hook++
		}
		if r.EventType != "" {
			kind++
		}
		if r.Action != "" {
			action++
		}
		if r.Size > largest {
			largest = r.Size
		}
		for _, k := range strings.Split(r.UpdatedKeys, ",") {
			if k != "" {
				keys[k] = true
			}
		}
		kinds[r.EventType+"/"+r.Action]++
		outcomes[r.Outcome]++
		s := r.ReceivedAt.Sub(r.LinearAt).Seconds()
		skewMin, skewMax = math.Min(skewMin, s), math.Max(skewMax, s)
	}
	names := []string{}
	for k := range keys {
		names = append(names, k)
	}
	sort.Strings(names)
	out := map[string]any{
		"deliveries":             len(rows),
		"kinds":                  kinds,
		"outcomes":               outcomes,
		"linear_delivery_header": share(header),
		"field_shapes":           map[string]string{"event_type": share(kind), "action": share(action), "webhook_id": share(hook)},
		"updated_from_keys":      names,
		"largest_payload_bytes":  largest,
	}
	if len(rows) > 0 {
		round := func(v float64) float64 { return math.Round(v*1000) / 1000 }
		out["skew_seconds"] = map[string]float64{"min": round(skewMin), "max": round(skewMax)}
	}
	return out
}

// newProbeUUID is a random version 4 UUID.
func newProbeUUID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	h := hex.EncodeToString(b[:])
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:]
}

// webhookProbeBody is a synthetic Issue update for team at t: IDs and kinds
// only, a fresh issue ID that names no real issue.
func webhookProbeBody(org, team string, t time.Time) []byte {
	b, _ := json.Marshal(map[string]any{
		"type": "Issue", "action": "update", "organizationId": org, "webhookTimestamp": t.UnixMilli(), "webhookId": newProbeUUID(),
		"data": map[string]any{"id": newProbeUUID(), "teamId": team},
	})
	return b
}

func webhookProbeSign(secret string, body []byte) string {
	m := hmac.New(sha256.New, []byte(secret))
	m.Write(body)
	return hex.EncodeToString(m.Sum(nil))
}

// webhookProbePost POSTs body to the team's route and answers the status
// and the error code, never the body's text.
func webhookProbePost(ctx context.Context, cfg webhookProbeConfig, body []byte, signature, delivery string) (int, string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, cfg.origin+LinearWebhookPath+cfg.team, bytes.NewReader(body))
	if err != nil {
		return 0, "", err
	}
	req.Header.Set("Content-Type", "application/json")
	if signature != "" {
		req.Header.Set("Linear-Signature", signature)
	}
	req.Header.Set("Linear-Delivery", delivery)
	res, err := cfg.client.Do(req)
	if err != nil {
		return 0, "", errors.New("linear-webhook-probe: the route could not be reached")
	}
	defer res.Body.Close()
	var e errorBody
	_ = json.NewDecoder(io.LimitReader(res.Body, 4096)).Decode(&e)
	code := e.Error.Code
	if !webhookWord.MatchString(strings.ReplaceAll(code, "_", "")) {
		code = ""
	}
	return res.StatusCode, code, nil
}

func webhookProbeForged(ctx context.Context, store webhookProbeStore, app webhookProbeApp, cfg webhookProbeConfig, out map[string]any) error {
	org, err := app.OrgID(ctx)
	if err != nil || !webhookUUID.MatchString(org) {
		out["pass"] = false
		return errors.New("linear-webhook-probe: the Linear app's organization is unavailable; nothing was sent")
	}
	start := cfg.now()
	before, err := store.WebhookProbeCounts(ctx, cfg.team, start)
	if err != nil {
		return errors.New("linear-webhook-probe: cannot read the counters")
	}
	randomKey := make([]byte, 32)
	_, _ = rand.Read(randomKey)
	oversize := append([]byte(`{"pad":"`), bytes.Repeat([]byte("x"), webhookBodyCap)...)
	oversize = append(oversize, []byte(`"}`)...)
	other := newProbeUUID()
	cases := []struct {
		name, want string
		body       func() ([]byte, string)
	}{
		{"unsigned", "401 signature", func() ([]byte, string) { return webhookProbeBody(org, cfg.team, cfg.now()), "" }},
		{"random_key", "401 signature", func() ([]byte, string) {
			b := webhookProbeBody(org, cfg.team, cfg.now())
			return b, webhookProbeSign(hex.EncodeToString(randomKey), b)
		}},
		{"stale", "401 stale", func() ([]byte, string) {
			b := webhookProbeBody(org, cfg.team, cfg.now().Add(-10*time.Minute))
			return b, webhookProbeSign(cfg.secret, b)
		}},
		{"other_team", "401 team_mismatch", func() ([]byte, string) {
			b := webhookProbeBody(org, other, cfg.now())
			return b, webhookProbeSign(cfg.secret, b)
		}},
		{"oversize", "413 too_large", func() ([]byte, string) { return oversize, "" }},
	}
	pass := true
	refusals := []map[string]any{}
	for _, c := range cases {
		body, sig := c.body()
		status, code, err := webhookProbePost(ctx, cfg, body, sig, newProbeUUID())
		if err != nil {
			out["refusals"] = refusals
			out["pass"] = false
			return err
		}
		got := fmt.Sprint(status)
		if code != "" {
			got += " " + code
		}
		ok := got == c.want
		pass = pass && ok
		refusals = append(refusals, map[string]any{"case": c.name, "want": c.want, "got": got, "ok": ok})
	}
	out["refusals"] = refusals
	// P3(a): one good body, twice, under one Linear-Delivery.
	delivery := newProbeUUID()
	good := webhookProbeBody(org, cfg.team, cfg.now())
	sig := webhookProbeSign(cfg.secret, good)
	first, _, err := webhookProbePost(ctx, cfg, good, sig, delivery)
	if err != nil {
		out["pass"] = false
		return err
	}
	second, _, err := webhookProbePost(ctx, cfg, good, sig, delivery)
	if err != nil {
		out["pass"] = false
		return err
	}
	rows, err := store.WebhookProbeDeliveries(ctx, cfg.team, start)
	if err != nil {
		return errors.New("linear-webhook-probe: cannot read the deliveries")
	}
	mine, others := 0, 0
	for _, r := range rows {
		if r.DeliveryID == delivery {
			mine++
		} else {
			others++
		}
	}
	// The refusals wrote no row; on a quiet probe team, no other row came.
	out["rows_from_refusals"] = others
	pass = pass && others == 0 && first == 200 && second == 200 && mine == 1
	// The counters: the route flushes them at most once a minute, so wait,
	// send one more unsigned body to flush, and read them again.
	if err = cfg.sleep(ctx, webhookProbeCountWait); err != nil {
		return err
	}
	if _, _, err = webhookProbePost(ctx, cfg, webhookProbeBody(org, cfg.team, cfg.now()), "", newProbeUUID()); err != nil {
		out["pass"] = false
		return err
	}
	if err = cfg.sleep(ctx, webhookProbeFlushSettle); err != nil {
		return err
	}
	after, err := store.WebhookProbeCounts(ctx, cfg.team, start)
	if err != nil {
		return errors.New("linear-webhook-probe: cannot read the counters")
	}
	delta := func(team, reason string) int { return after[team][reason] - before[team][reason] }
	out["redelivery"] = map[string]any{"first": first, "second": second, "rows": mine, "duplicates_counted": delta(cfg.team, "duplicate")}
	pass = pass && delta(cfg.team, "duplicate") >= 1
	// Unsigned, random key and the flush are unattributed signature
	// refusals; the oversize one an unattributed too_large; the stale and
	// other-team ones are the team's.
	counts := map[string]any{
		"signature": delta("", "signature"), "too_large": delta("", "too_large"),
		"stale": delta(cfg.team, "stale"), "team_mismatch": delta(cfg.team, "team_mismatch"),
	}
	counted := counts["signature"].(int) >= 3 && counts["too_large"].(int) >= 1 && counts["stale"].(int) >= 1 && counts["team_mismatch"].(int) >= 1
	counts["ok"] = counted
	out["counted"] = counts
	out["pass"] = pass && counted
	return nil
}
