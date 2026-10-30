package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"clawbridge/internal/bus"
	"clawbridge/internal/linearapp"
)

// Synthetic IDs and secrets only.
const (
	wpTeam   = "00000000-0000-4000-8000-00000000c0a1"
	wpOther  = "00000000-0000-4000-8000-00000000c0a2"
	wpOrg    = "00000000-0000-4000-8000-00000000c0b1"
	wpHook   = "00000000-0000-4000-8000-00000000c0c1"
	wpIssue  = "00000000-0000-4000-8000-00000000c0e1"
	wpLabel  = "00000000-0000-4000-8000-00000000c0f1"
	wpSecret = "whsec-probe-synthetic-0123456789"
)

// fakeWebhookProbeApp is the Linear app on the probe team. Each write it
// is given delivers one webhook into the store, as Linear and the route
// would, unless silent names the step.
type fakeWebhookProbeApp struct {
	t       *testing.T
	store   *bus.Store
	clock   *drainClock
	silent  map[string]bool
	fail    map[string]bool
	archive string // the archive class; "" is archived
	// noHeader records each delivery under its body's hash, as the route
	// does when Linear sends no Linear-Delivery header.
	noHeader bool
	q7       error
	calls    []string
	closed   bool
	n        int
}

func (a *fakeWebhookProbeApp) deliver(step, kind, action string, keys ...string) {
	if a.silent[step] {
		return
	}
	a.n++
	now := a.clock.Now()
	id := fmt.Sprintf("00000000-0000-4000-8000-%012d", 900+a.n)
	if a.noHeader {
		id = fmt.Sprintf("sha256:%064d", a.n)
	}
	if _, err := a.store.WebhookRecord(context.Background(), bus.WebhookDelivery{DeliveryID: id, TeamID: wpTeam, WebhookID: wpHook, EventType: kind, Action: action,
		EntityID: wpIssue, UpdatedKeys: keys, LinearAt: now.Add(-1500 * time.Millisecond), ReceivedAt: now, Size: 1000 + a.n}); err != nil {
		a.t.Fatal(err)
	}
}

func (a *fakeWebhookProbeApp) step(name string) error {
	a.calls = append(a.calls, name)
	if a.fail[name] {
		return &linearapp.Error{Code: "refused"}
	}
	return nil
}

func (a *fakeWebhookProbeApp) OrgID(context.Context) (string, error) {
	a.calls = append(a.calls, "org")
	return wpOrg, nil
}

func (a *fakeWebhookProbeApp) CreateIssue(_ context.Context, team, title, description string) (string, error) {
	if team != wpTeam || title != webhookProbeTitle || description != webhookProbeText {
		a.t.Fatal("create", team, title, description)
	}
	if err := a.step("create"); err != nil {
		return "", err
	}
	a.deliver("create", "Issue", "create")
	return wpIssue, nil
}

func (a *fakeWebhookProbeApp) Comment(_ context.Context, issue, body string) error {
	if issue != wpIssue || body != webhookProbeText {
		a.t.Fatal("comment", issue, body)
	}
	if err := a.step("comment"); err != nil {
		return err
	}
	a.deliver("comment", "Comment", "create")
	return nil
}

func (a *fakeWebhookProbeApp) NeedsAgentLabel(_ context.Context, team string) (string, error) {
	if team != wpTeam {
		a.t.Fatal("label lookup", team)
	}
	if err := a.step("label_lookup"); err != nil {
		return "", err
	}
	return wpLabel, nil
}

func (a *fakeWebhookProbeApp) SetLabel(_ context.Context, issue, label string, add bool) error {
	name := map[bool]string{true: "label_add", false: "label_remove"}[add]
	if issue != wpIssue || label != wpLabel {
		a.t.Fatal(name, issue, label)
	}
	if err := a.step(name); err != nil {
		return err
	}
	a.deliver(name, "Issue", "update", "labelIds", "updatedAt")
	return nil
}

func (a *fakeWebhookProbeApp) ArchiveIssue(_ context.Context, issue string) (string, error) {
	if issue != wpIssue {
		a.t.Fatal("archive", issue)
	}
	a.calls = append(a.calls, "archive")
	if a.archive != "" {
		return a.archive, nil
	}
	a.deliver("archive", "Issue", "update", "archivedAt", "updatedAt")
	return "archived", nil
}

func (a *fakeWebhookProbeApp) WebhooksQuery(context.Context) error {
	a.calls = append(a.calls, "webhooks_query")
	return a.q7
}

func (a *fakeWebhookProbeApp) Close() error { a.closed = true; return nil }

// wpStore is a SQLite bus store.
func wpStore(t *testing.T) *bus.Store {
	t.Helper()
	store, err := bus.Open(filepath.Join(t.TempDir(), "bus.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	return store
}

// wpConfig runs on clock: every sleep moves it on.
func wpConfig(clock *drainClock) webhookProbeConfig {
	var mu sync.Mutex
	return webhookProbeConfig{
		team: wpTeam, now: clock.Now,
		sleep: func(_ context.Context, d time.Duration) error {
			mu.Lock()
			defer mu.Unlock()
			clock.add(d)
			return nil
		},
	}
}

// wpOutput is the probe's printed object, and the whole output, which
// must name no ID, secret or text: only the archived issue's ID, when its
// archive failed, may appear.
func wpOutput(t *testing.T, out string, allowIssue bool) map[string]any {
	t.Helper()
	markers := []string{wpTeam, wpOrg, wpHook, wpLabel, wpSecret, webhookProbeTitle, webhookProbeText, "00000000-0000-4000-8000-0000000009"}
	if !allowIssue {
		markers = append(markers, wpIssue)
	}
	for _, m := range markers {
		if strings.Contains(strings.ToLower(out), strings.ToLower(m)) {
			t.Fatalf("the output names %q: %s", m, out)
		}
	}
	var got map[string]map[string]any
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatal(err, out)
	}
	return got["webhook_probe"]
}

// P1: the app makes one issue, comments, adds and removes needs-agent and
// archives it; every event's delivery is read back from the store as
// classes only.
func TestWebhookProbeRoundTrip(t *testing.T) {
	clock := &drainClock{now: whStart}
	app := &fakeWebhookProbeApp{t: t, store: wpStore(t), clock: clock}
	var out bytes.Buffer
	if err := linearWebhookProbe(context.Background(), app.store, app, wpConfig(clock), &out); err != nil {
		t.Fatal(err, out.String())
	}
	got := wpOutput(t, out.String(), false)
	if strings.Join(app.calls, ",") != "create,comment,label_lookup,label_add,label_remove,archive,webhooks_query" || !app.closed {
		t.Fatal(app.calls, app.closed)
	}
	events, _ := got["events"].(map[string]any)
	for _, e := range []string{"issue_created", "comment_created", "label_added", "label_removed", "issue_archived"} {
		if events[e] != true {
			t.Fatal(e, got)
		}
	}
	shapes, _ := got["field_shapes"].(map[string]any)
	skew, _ := got["skew_seconds"].(map[string]any)
	keys, _ := got["updated_from_keys"].([]any)
	if got["pass"] != true || got["deliveries"] != 5.0 || got["linear_delivery_header"] != "all" || shapes["webhook_id"] != "all" || shapes["event_type"] != "all" ||
		got["largest_payload_bytes"] != 1005.0 || skew["min"] != 1.5 || skew["max"] != 1.5 || got["webhooks_query_without_admin"] != "ok" ||
		fmt.Sprint(keys) != "[archivedAt labelIds updatedAt]" || got["cleanup"] != nil || !strings.Contains(fmt.Sprint(got["attribution_note"]), "quiet probe team") {
		t.Fatal(got)
	}
	steps, _ := got["steps"].(map[string]any)
	if steps["create"] != "ok" || steps["archive"] != "archived" {
		t.Fatal(steps)
	}
}

// Without the Linear-Delivery header the route keys a delivery by its
// body's hash (A4): the probe reports the header as none and still passes.
func TestWebhookProbeRoundTripWithoutDeliveryHeader(t *testing.T) {
	clock := &drainClock{now: whStart}
	app := &fakeWebhookProbeApp{t: t, store: wpStore(t), clock: clock, noHeader: true}
	var out bytes.Buffer
	if err := linearWebhookProbe(context.Background(), app.store, app, wpConfig(clock), &out); err != nil {
		t.Fatal(err, out.String())
	}
	got := wpOutput(t, out.String(), false)
	if got["pass"] != true || got["linear_delivery_header"] != "none" || got["deliveries"] != 5.0 {
		t.Fatal(got)
	}
}

// A delivery that never comes is reported missing after the two-minute
// wait, and is no pass; the webhooks query's refusal is recorded only.
func TestWebhookProbeMissingDelivery(t *testing.T) {
	clock := &drainClock{now: whStart}
	app := &fakeWebhookProbeApp{t: t, store: wpStore(t), clock: clock, silent: map[string]bool{"comment": true}, q7: &linearapp.Error{Code: "query_failed"}}
	var out bytes.Buffer
	if err := linearWebhookProbe(context.Background(), app.store, app, wpConfig(clock), &out); err != nil {
		t.Fatal(err)
	}
	got := wpOutput(t, out.String(), false)
	events, _ := got["events"].(map[string]any)
	if got["pass"] != false || events["comment_created"] != false || events["issue_created"] != true || got["webhooks_query_without_admin"] != "query_failed" {
		t.Fatal(got)
	}
	if waited, _ := got["waited_seconds"].(float64); waited < 120 || waited > 130 {
		t.Fatal("waited", got["waited_seconds"])
	}
}

// An archive that fails prints the issue's ID, the one ID the probe ever
// prints, so the owner can archive it by hand.
func TestWebhookProbeArchiveFailedNamesTheIssue(t *testing.T) {
	clock := &drainClock{now: whStart}
	app := &fakeWebhookProbeApp{t: t, store: wpStore(t), clock: clock, archive: "scope", fail: map[string]bool{"label_lookup": true}}
	var out bytes.Buffer
	err := linearWebhookProbe(context.Background(), app.store, app, wpConfig(clock), &out)
	got := wpOutput(t, out.String(), true)
	if err == nil || !strings.Contains(out.String(), wpIssue) || !strings.Contains(fmt.Sprint(got["cleanup"]), wpIssue) || got["pass"] != false {
		t.Fatal(err, got)
	}
	steps, _ := got["steps"].(map[string]any)
	if steps["label_add"] != "refused" || steps["archive"] != "scope" {
		t.Fatal(steps)
	}
	// A create that fails leaves nothing to clean up, and sends nothing more.
	app = &fakeWebhookProbeApp{t: t, store: wpStore(t), clock: clock, fail: map[string]bool{"create": true}}
	out.Reset()
	if err = linearWebhookProbe(context.Background(), app.store, app, wpConfig(clock), &out); err == nil || strings.Join(app.calls, ",") != "create" {
		t.Fatal(err, app.calls)
	}
	wpOutput(t, out.String(), false)
}

// wpRoute is the real route on a SQLite store, behind a test server, on
// clock; counters flush in the request.
func wpRoute(t *testing.T, clock *drainClock) (*bus.Store, *httptest.Server) {
	t.Helper()
	store := wpStore(t)
	h := newWebhookHandler(store, &WebhookOptions{
		Secrets: map[string][]string{wpTeam: {wpSecret}},
		OrgID:   func(context.Context) (string, error) { return wpOrg, nil },
	}, clock.Now, nil)
	h.spawn = func(f func()) { f() }
	mux := http.NewServeMux()
	mux.Handle(LinearWebhookPath, h)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return store, srv
}

// P2 and P3(a): five forged or expired deliveries are refused 401, 401,
// 401, 401 and 413, each counted, none recorded; one good body sent twice
// under one Linear-Delivery is one row.
func TestWebhookProbeForged(t *testing.T) {
	clock := &drainClock{now: whStart}
	store, srv := wpRoute(t, clock)
	app := &fakeWebhookProbeApp{t: t, store: store, clock: clock}
	cfg := wpConfig(clock)
	cfg.forged, cfg.secret, cfg.origin, cfg.client = true, wpSecret, srv.URL, srv.Client()
	var out bytes.Buffer
	if err := linearWebhookProbe(context.Background(), store, app, cfg, &out); err != nil {
		t.Fatal(err, out.String())
	}
	got := wpOutput(t, out.String(), false)
	if strings.Join(app.calls, ",") != "org" {
		t.Fatal("the forged probe writes nothing to Linear:", app.calls)
	}
	refusals, _ := got["refusals"].([]any)
	want := []string{"unsigned:401 signature", "random_key:401 signature", "stale:401 stale", "other_team:401 team_mismatch", "oversize:413 too_large"}
	if len(refusals) != len(want) {
		t.Fatal(got)
	}
	for i, r := range refusals {
		m := r.(map[string]any)
		if fmt.Sprintf("%s:%s", m["case"], m["got"]) != want[i] || m["ok"] != true {
			t.Fatal(i, m)
		}
	}
	redelivery, _ := got["redelivery"].(map[string]any)
	counted, _ := got["counted"].(map[string]any)
	if got["pass"] != true || got["rows_from_refusals"] != 0.0 || redelivery["first"] != 200.0 || redelivery["second"] != 200.0 || redelivery["rows"] != 1.0 || redelivery["duplicates_counted"] != 1.0 || counted["ok"] != true {
		t.Fatal(got)
	}
	// Every refusal is counted: three unattributed signature refusals (the
	// unsigned, the random key and the flush), one too_large, and the
	// team's stale and team_mismatch.
	if counted["signature"] != 3.0 || counted["too_large"] != 1.0 || counted["stale"] != 1.0 || counted["team_mismatch"] != 1.0 {
		t.Fatal(counted)
	}
	rows, err := store.WebhookProbeDeliveries(context.Background(), wpTeam, whStart.Add(-time.Hour))
	if err != nil || len(rows) != 1 {
		t.Fatal(rows, err)
	}
}

// A route that takes a forged body is no pass, and says which.
func TestWebhookProbeForgedAccepted(t *testing.T) {
	clock := &drainClock{now: whStart}
	store := wpStore(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) }))
	t.Cleanup(srv.Close)
	app := &fakeWebhookProbeApp{t: t, store: store, clock: clock}
	cfg := wpConfig(clock)
	cfg.forged, cfg.secret, cfg.origin, cfg.client = true, wpSecret, srv.URL, srv.Client()
	var out bytes.Buffer
	err := linearWebhookProbe(context.Background(), store, app, cfg, &out)
	got := wpOutput(t, out.String(), false)
	refusals, _ := got["refusals"].([]any)
	if err != nil || got["pass"] != false || len(refusals) != 5 || refusals[0].(map[string]any)["ok"] != false || refusals[0].(map[string]any)["got"] != "200" {
		t.Fatal(err, got)
	}
}

// Without --confirm nothing is opened or sent; the flags are checked first.
func TestWebhookProbeNeedsConfirm(t *testing.T) {
	prev := newWebhookProbeApp
	newWebhookProbeApp = func(linearapp.Store) (webhookProbeApp, error) {
		t.Fatal("the app was built")
		return nil, errors.New("unreachable")
	}
	t.Cleanup(func() { newWebhookProbeApp = prev })
	t.Setenv("CLAWBRIDGE_LINEAR_WEBHOOK_SECRETS", wpTeam+"="+wpSecret)
	t.Setenv("CLAWBRIDGE_DATABASE_URL", "")
	for _, c := range []struct {
		args []string
		want string
	}{
		{[]string{"--team", wpTeam}, "repeat with --confirm"},
		{[]string{"--team", wpTeam, "--forged", "--public-origin", "https://clawbridge.example.test"}, "repeat with --confirm"},
		{[]string{"--confirm"}, "--team"},
		{[]string{"--confirm", "--team", "not-a-uuid"}, "--team"},
		{[]string{"--confirm", "--team", wpTeam, "--forged"}, "--public-origin"},
		{[]string{"--confirm", "--team", wpTeam, "--public-origin", "https://clawbridge.example.test"}, "--public-origin goes with --forged"},
		{[]string{"--confirm", "--team", wpOther, "--forged", "--public-origin", "https://clawbridge.example.test"}, "no secret for --team"},
		{[]string{"--confirm", "--team", wpTeam}, "Postgres"},
	} {
		var out bytes.Buffer
		err := RunOperator(context.Background(), append([]string{"linear-webhook-probe"}, c.args...), &out)
		if err == nil || !strings.Contains(err.Error(), c.want) || out.Len() != 0 || strings.Contains(err.Error(), wpSecret) {
			t.Fatal(c.args, err, out.String())
		}
	}
}
