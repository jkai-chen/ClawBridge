package bus

import (
	"context"
	"strings"
	"testing"
)

// FR57: the text a trusted person vouched for is the text the row holds the
// issue to.

type textHarness struct {
	*intakeHarness
	pid string
}

func newTextHarness(t *testing.T) *textHarness {
	t.Helper()
	h := &textHarness{intakeHarness: newIntakeHarness(t)}
	if err := h.s.db.QueryRow(`SELECT id FROM projects WHERE slug='p'`).Scan(&h.pid); err != nil {
		t.Fatal(err)
	}
	return h
}

func (h *textHarness) signal(at, updated, title, description string) string {
	h.t.Helper()
	return h.signalBy("S1", at, updated, title, description)
}

func (h *textHarness) signalBy(signal, at, updated, title, description string) string {
	h.t.Helper()
	ev := IntakeEvent{Signal: signal, Actor: trustedUser, Trusted: true, At: at, Issue: IntakeIssue{UUID: intakeIssue, Identifier: "FIX-7", TitleSHA256: title, DescriptionSHA256: description, UpdatedAt: updated}}
	got, err := h.s.IntakeEnqueueSystem(context.Background(), h.pid, ev)
	if err != nil {
		h.t.Fatal(err)
	}
	return got
}

// check is the server's own comparison, as the intake pass makes it from the
// app's read.
func (h *textHarness) check(title, description string) map[string]any {
	h.t.Helper()
	st, err := h.s.IntakeTextCheck(context.Background(), h.pid, intakeIssue, title, description)
	if err != nil {
		h.t.Fatal(err)
	}
	return map[string]any{"tracked": st.Tracked, "changed": st.Changed, "maybe": st.Maybe, "signal_at": st.SignalAt}
}

// status is what a client's issue read gets: the stored verdict.
func (h *textHarness) status() map[string]any {
	h.t.Helper()
	return h.call("intake_text_status", "a", map[string]any{"issue_uuid": intakeIssue}).(map[string]any)
}

func (h *textHarness) notices() int {
	n := 0
	for _, m := range h.intakeInbox("c") {
		if body := m["body"].(string); !strings.Contains(body, "changed after the trusted signal") && !strings.Contains(body, "may have changed") {
			n++
		}
	}
	return n
}

// maybeNotices are the notices of a flag set at queueing, which says only
// that the issue changed after the signal.
func (h *textHarness) maybeNotices() []string {
	out := []string{}
	for _, m := range h.intakeInbox("c") {
		if body := m["body"].(string); strings.Contains(body, "may have changed") {
			out = append(out, body)
		}
	}
	return out
}

func (h *textHarness) followUps() int {
	n := 0
	for _, m := range h.intakeInbox("c") {
		if strings.Contains(m["body"].(string), "changed after the trusted signal") {
			n++
		}
	}
	return n
}

func (h *textHarness) reason() string {
	h.t.Helper()
	var reason string
	if err := h.s.db.QueryRow(`SELECT reason FROM intake_queue`).Scan(&reason); err != nil {
		h.t.Fatal(err)
	}
	return reason
}

const (
	signalTime = "2026-10-23T10:00:00Z"
	laterTime  = "2026-10-23T11:00:00Z"
	// withinSkew and beyondSkew are issue updates after a signalTime
	// transition.
	withinSkew = "2026-10-23T10:00:03Z"
	beyondSkew = "2026-10-23T10:00:30Z"
)

var (
	appTitle       = strings.Repeat("a", 64)
	appDescription = strings.Repeat("b", 64)
)

func TestIntakeTextEditAfterSignalMarksAndNotifiesOnce(t *testing.T) {
	h := newTextHarness(t)
	if got := h.signal(signalTime, signalTime, "t1", "d1"); got != "queued" {
		t.Fatal(got)
	}
	if out := h.check("t1", "d1"); out["tracked"] != true || out["changed"] != false || out["signal_at"] != signalTime {
		t.Fatal("matching hashes:", out)
	}
	if h.followUps() != 0 || h.reason() != "" || h.status()["changed"] != false {
		t.Fatal("a matching check flagged or told:", h.reason())
	}
	for i := 0; i < 3; i++ {
		if out := h.check("t1", "d2"); out["changed"] != true || out["tracked"] != true {
			t.Fatal("check", i, out)
		}
	}
	if h.followUps() != 1 || h.reason() != "text_changed" || h.status()["changed"] != true {
		t.Fatal("follow-ups:", h.followUps(), "reason:", h.reason())
	}
	var body string
	for _, m := range h.intakeInbox("c") {
		if strings.Contains(m["body"].(string), "changed after the trusted signal") {
			body = m["body"].(string)
		}
	}
	for _, want := range []string{"FIX-7's title or description changed after the trusted signal (issue updated at 2026-10-23T10:00:00Z)", "NOT vouched for", "ask a project lead or owner to run clawbridge intake rearm " + intakeIssue + " in their own terminal", intakeLine} {
		if !strings.Contains(body, want) {
			t.Fatalf("notice lacks %q:\n%s", want, body)
		}
	}
	if strings.Contains(body, "d2") {
		t.Fatal("the notice carries a hash:", body)
	}
}

func TestIntakeTextTitleOnlyAndDescriptionOnlyCount(t *testing.T) {
	for name, hashes := range map[string][2]string{"title": {"t2", "d1"}, "description": {"t1", "d2"}} {
		t.Run(name, func(t *testing.T) {
			h := newTextHarness(t)
			h.signal(signalTime, signalTime, "t1", "d1")
			if out := h.check(hashes[0], hashes[1]); out["changed"] != true {
				t.Fatal(out)
			}
			if h.followUps() != 1 {
				t.Fatal("follow-ups:", h.followUps())
			}
		})
	}
}

func TestIntakeTextUnknownFieldIsNotCompared(t *testing.T) {
	h := newTextHarness(t)
	h.signal(signalTime, signalTime, "t1", "d1")
	if out := h.check("t1", ""); out["changed"] != false {
		t.Fatal("an unread description counted as a change:", out)
	}
}

func TestIntakeTextUntrackedIssues(t *testing.T) {
	h := newTextHarness(t)
	if out := h.check("t1", "d1"); out["tracked"] != false || h.status()["tracked"] != false {
		t.Fatal("no row:", out)
	}
	h.signal(signalTime, signalTime, "t1", "d1")
	h.call("intake_dismiss", "c", map[string]any{"issue_uuid": intakeIssue})
	if out := h.check("t9", "d9"); out["tracked"] != false || h.followUps() != 0 || h.status()["tracked"] != false {
		t.Fatal("dismissed:", out)
	}
	if _, err := h.s.db.Exec(`UPDATE intake_queue SET state='closed',reason=''`); err != nil {
		t.Fatal(err)
	}
	if out := h.check("t9", "d9"); out["tracked"] != false || h.followUps() != 0 {
		t.Fatal("closed:", out)
	}
	if _, err := h.s.db.Exec(`UPDATE intake_queue SET state='queued',queued_title_sha256='',queued_description_sha256=''`); err != nil {
		t.Fatal(err)
	}
	if out := h.check("t9", "d9"); out["tracked"] != false || h.followUps() != 0 {
		t.Fatal("a row without hashes:", out)
	}
}

// A client's issue read gets the stored verdict and changes nothing: no row,
// no message, whatever the client's own rendering of the text.
func TestIntakeTextStatusIsReadOnly(t *testing.T) {
	h := newTextHarness(t)
	h.signal(signalTime, signalTime, "t1", "d1")
	rows := func() string {
		var a, b string
		if err := h.s.db.QueryRow(`SELECT reason||'|'||updated_at||'|'||queued_title_sha256||queued_description_sha256, (SELECT count(*) FROM messages) FROM intake_queue`).Scan(&a, &b); err != nil {
			t.Fatal(err)
		}
		return a + "#" + b
	}
	before := rows()
	for i := 0; i < 3; i++ {
		if out := h.status(); out["tracked"] != true || out["changed"] != false || out["signal_at"] != signalTime {
			t.Fatal(out)
		}
	}
	// Hashes in a client's request are not arguments at all.
	if e := h.errorOf("intake_text_status", "a", map[string]any{"issue_uuid": intakeIssue, "title_sha256": "t9", "description_sha256": "d9"}); e == "" {
		t.Fatal("hashes accepted")
	}
	if rows() != before || h.followUps() != 0 {
		t.Fatal("a status read wrote:", before, rows())
	}
}

// No client or session can run the comparison: it is not an operation.
func TestIntakeTextCheckIsNotCallableByASession(t *testing.T) {
	h := newTextHarness(t)
	h.signal(signalTime, signalTime, "t1", "d1")
	for _, session := range []string{"a", "c", ""} {
		args := map[string]any{"issue_uuid": intakeIssue, "title_sha256": "t9", "description_sha256": "d9"}
		if e := h.errorOf("intake_text_check", session, args); e == "" {
			t.Fatal("accepted")
		}
	}
	if h.reason() != "" || h.followUps() != 0 {
		t.Fatal("a refused call flagged the row")
	}
}

// R2-M1: a row nobody could be told about keeps saying so. The change is
// flagged unsent, readers see it, and the next pass with a coordinator tells
// the original notice and the change once.
func TestIntakeTextChangeWithNoCoordinatorIsToldWhenOneAppears(t *testing.T) {
	h := newTextHarness(t)
	if _, err := h.s.db.Exec(`UPDATE projects SET coordinator_session=NULL`); err != nil {
		t.Fatal(err)
	}
	h.signal(signalTime, signalTime, "t1", "d1")
	if h.reason() != "no_recipient" {
		t.Fatal(h.reason())
	}
	if out := h.check("t1", "d2"); out["changed"] != true || h.reason() != "text_changed_unsent" || h.status()["changed"] != true {
		t.Fatal(out, h.reason())
	}
	if err := h.s.IntakeTextRetry(context.Background(), h.pid); err != nil || h.reason() != "text_changed_unsent" {
		t.Fatal("told nobody:", err, h.reason())
	}
	h.call("coordinator_set", "c", map[string]any{"target_session": "c"})
	for i := 0; i < 3; i++ {
		if err := h.s.IntakeTextRetry(context.Background(), h.pid); err != nil {
			t.Fatal(err)
		}
		if out := h.check("t1", "d2"); out["changed"] != true || h.status()["changed"] != true {
			t.Fatal(out)
		}
	}
	if h.reason() != "text_changed" || h.notices() != 1 || h.followUps() != 1 {
		t.Fatal("reason:", h.reason(), "original:", h.notices(), "change:", h.followUps())
	}
}

// Clearing an unsent mark gives back the owed original notice's marker.
func TestIntakeClearingAnUnsentMarkRestoresNoRecipient(t *testing.T) {
	h := newTextHarness(t)
	if _, err := h.s.db.Exec(`UPDATE projects SET coordinator_session=NULL`); err != nil {
		t.Fatal(err)
	}
	h.signal(signalTime, signalTime, "t1", "d1")
	h.check("t1", "d2")
	h.signal("2026-10-23T12:00:00Z", "2026-10-23T12:00:01Z", "t1", "d2")
	if h.reason() != "no_recipient" || h.status()["changed"] != false {
		t.Fatal(h.reason())
	}
}

// A trusted re-signal vouches again only for the text the person saw: the
// issue was not updated more than the skew after the transition.
func TestIntakeReSignalWithinTheSkewReStampsAndBeyondDoesNot(t *testing.T) {
	later := "2026-10-23T12:00:00Z"
	h := newTextHarness(t)
	h.signal(signalTime, signalTime, "t1", "d1")
	h.check("t1", "d2")
	// An edit between the transition and the poll: the text is not vouched.
	if got := h.signal(later, "2026-10-23T12:00:30Z", "t1", "d3"); got != "already_queued" {
		t.Fatal(got)
	}
	if h.reason() != "text_changed" || h.status()["changed"] != true || h.check("t1", "d1")["changed"] != true {
		t.Fatal("an edit after the transition vouched for itself:", h.reason())
	}
	if h.stored() != "t1|d1" {
		t.Fatal("re-stamped:", h.stored())
	}
	// Within the skew it is what they saw.
	if got := h.signal("2026-10-23T13:00:00Z", "2026-10-23T13:00:03Z", "t1", "d3"); got != "already_queued" {
		t.Fatal(got)
	}
	if h.reason() != "" || h.stored() != "t1|d3" || h.status()["changed"] != false {
		t.Fatal("not re-stamped:", h.reason(), h.stored())
	}
}

func (h *textHarness) stored() string {
	h.t.Helper()
	var title, description string
	if err := h.s.db.QueryRow(`SELECT queued_title_sha256,queued_description_sha256 FROM intake_queue`).Scan(&title, &description); err != nil {
		h.t.Fatal(err)
	}
	return title + "|" + description
}

// A mention is not an act on the issue: it never vouches again.
func TestIntakeAMentionNeverReVouches(t *testing.T) {
	h := newTextHarness(t)
	h.signal(signalTime, signalTime, "t1", "d1")
	h.check("t1", "d2")
	if got := h.signalBy("S3", "2026-10-23T12:00:00Z", "2026-10-23T12:00:01Z", "t1", "d2"); got != "already_queued" {
		t.Fatal(got)
	}
	if h.reason() != "text_changed" || h.stored() != "t1|d1" || h.status()["changed"] != true {
		t.Fatal("a mention vouched:", h.reason(), h.stored())
	}
	// A delegation (S2) does.
	h.signalBy("S2", "2026-10-23T13:00:00Z", "2026-10-23T13:00:01Z", "t1", "d2")
	if h.reason() != "" || h.stored() != "t1|d2" {
		t.Fatal("a delegation did not:", h.reason(), h.stored())
	}
}

// The cap raise follows the same rule: the cap rises either way, the
// hashes and the flag change only within the skew.
func TestIntakeCapRaiseFollowsTheSkewRule(t *testing.T) {
	for name, c := range map[string]struct {
		updated, want, reason string
		changed               bool
	}{
		"within the skew": {"2026-10-23T12:00:02Z", "t1|d2", "", false},
		"beyond the skew": {"2026-10-23T12:00:40Z", "t1|d1", "text_changed", true},
	} {
		t.Run(name, func(t *testing.T) {
			h := newTextHarness(t)
			ev := IntakeEvent{Signal: "S1", At: signalTime, Issue: IntakeIssue{UUID: intakeIssue, Identifier: "FIX-7", TitleSHA256: "t1", DescriptionSHA256: "d1", UpdatedAt: signalTime}}
			if got, err := h.s.IntakeEnqueueSystem(context.Background(), h.pid, ev); err != nil || got != "queued" {
				t.Fatal(got, err)
			}
			h.check("t1", "d2")
			h.signal("2026-10-23T12:00:00Z", c.updated, "t1", "d2")
			var cap string
			if err := h.s.db.QueryRow(`SELECT trigger_cap FROM intake_queue`).Scan(&cap); err != nil || cap != "auto" {
				t.Fatal("the cap did not rise:", cap, err)
			}
			if h.stored() != c.want || h.reason() != c.reason || h.status()["changed"] != c.changed {
				t.Fatal(h.stored(), h.reason())
			}
		})
	}
}

// A trusted signal queued for an issue already edited beyond the skew comes
// in flagged, told once; an untrusted one is not flagged.
func TestIntakeRequeueBeyondTheSkewIsQueuedFlagged(t *testing.T) {
	h := newTextHarness(t)
	h.signal(signalTime, signalTime, "t1", "d1")
	h.close()
	if got := h.signal("2026-10-23T12:00:00Z", "2026-10-23T12:05:00Z", "t1", "d5"); got != "queued" {
		t.Fatal(got)
	}
	maybe := h.maybeNotices()
	if h.reason() != "text_maybe" || h.status()["changed"] != true || h.followUps() != 0 || len(maybe) != 1 || h.notices() != 2 {
		t.Fatal(h.reason(), h.followUps(), maybe, h.notices())
	}
	// The wording is the hedged one, with parsed times, and no hash.
	want := "Intake: FIX-7's text may have changed (or another field changed) since the trusted signal (signal at 2026-10-23T12:00:00Z, issue updated at 2026-10-23T12:05:00Z); check the issue history. Until then the text is NOT vouched for. Ask the person who asked to re-add needs-agent or delegate again, or ask a project lead or owner to run clawbridge intake rearm " + intakeIssue + " in their own terminal, to vouch for it.\n" + intakeLine
	if maybe[0] != want {
		t.Fatalf("notice:\n%s\nwant:\n%s", maybe[0], want)
	}
	// Within the skew: an ordinary requeue.
	h2 := newTextHarness(t)
	h2.signal(signalTime, signalTime, "t1", "d1")
	h2.close()
	h2.signal("2026-10-23T12:00:00Z", "2026-10-23T12:00:02Z", "t1", "d5")
	if h2.reason() != "" || h2.followUps() != 0 {
		t.Fatal(h2.reason())
	}
	// Untrusted (S5-like notify cap) signals are never flagged.
	h3 := newTextHarness(t)
	ev := IntakeEvent{Signal: "S1", At: "2026-10-23T12:00:00Z", Issue: IntakeIssue{UUID: intakeIssue, Identifier: "FIX-7", TitleSHA256: "t1", DescriptionSHA256: "d1", UpdatedAt: "2026-10-23T12:30:00Z"}}
	if got, err := h3.s.IntakeEnqueueSystem(context.Background(), h3.pid, ev); err != nil || got != "queued" || h3.reason() != "" || h3.followUps() != 0 {
		t.Fatal(got, err, h3.reason())
	}
}

func (h *textHarness) close() {
	h.t.Helper()
	if _, err := h.s.db.Exec(`UPDATE intake_queue SET state='closed',closed_at='2026-10-23T10:30:00Z'`); err != nil {
		h.t.Fatal(err)
	}
}

func (h *textHarness) rearm(session string, extra map[string]any, app *AppText) map[string]any {
	h.t.Helper()
	args := map[string]any{"issue_uuid": intakeIssue}
	for k, v := range extra {
		args[k] = v
	}
	ctx := context.Background()
	if app != nil {
		ctx = WithAppText(ctx, *app)
	}
	r, err := h.s.Execute(ctx, h.harness.request("intake_rearm", session, args))
	if err != nil {
		h.t.Fatal(err)
	}
	return r.Data.(map[string]any)
}

func appRead(updated string) *AppText {
	return &AppText{TitleSHA256: appTitle, DescriptionSHA256: appDescription, UpdatedAt: updated}
}

// A coordinator session is an agent: its rearm never lifts the mark, with
// the app's read there or not, and a later check changes nothing and tells
// nobody again.
func TestIntakeSessionRearmDoesNotVouch(t *testing.T) {
	h := newTextHarness(t)
	h.signal(signalTime, signalTime, "t1", "d1")
	h.check("t1", "d2")
	confirmed := map[string]any{"agent_marker": "", "updated_at": laterTime}
	for _, app := range []*AppText{nil, appRead(laterTime)} {
		if out := h.rearm("c", confirmed, app); out["text_vouched"] != false || asInt(out["rearmed"]) != 1 || out["reason"] != "text_changed" {
			t.Fatal(out)
		}
	}
	if out := h.check("t1", "d2"); out["changed"] != true || h.status()["changed"] != true {
		t.Fatal("a session's rearm vouched:", out)
	}
	if h.reason() != "text_changed" || h.followUps() != 1 || h.stored() != "t1|d1" {
		t.Fatal(h.reason(), h.followUps(), h.stored())
	}
}

// A lead in their own terminal vouches, with the app's hashes, when the app
// read the very update they confirmed.
func TestIntakeLeadRearmTakesTheAppsReadOfTheConfirmedUpdate(t *testing.T) {
	h := newTextHarness(t)
	h.signal(signalTime, signalTime, "t1", "d1")
	h.check("t1", "d2")
	out := h.rearm("", map[string]any{"agent_marker": "", "updated_at": laterTime}, appRead("2026-10-23T11:00:00.000Z"))
	if out["text_vouched"] != true || out["reason"] != "" {
		t.Fatal(out)
	}
	if h.stored() != appTitle+"|"+appDescription || h.status()["signal_at"] != "2026-10-23T11:00:00.000Z" {
		t.Fatal("the baseline is not the app's read:", h.stored())
	}
	if got := h.check(appTitle, appDescription); got["changed"] != false {
		t.Fatal(got)
	}
	if got := h.check(appTitle, strings.Repeat("c", 64)); got["changed"] != true || h.followUps() != 2 {
		t.Fatal("a later edit:", got, h.followUps())
	}
}

// The confirmed update must be the app's: a mismatch, no read, an unread
// description, an agent marker, a missing marker or time, or hashes sent by
// the client all leave the mark.
func TestIntakeLeadRearmWithoutProofDoesNotVouch(t *testing.T) {
	good := map[string]any{"agent_marker": "", "updated_at": laterTime}
	with := func(k string, v any) map[string]any {
		m := map[string]any{}
		for key, val := range good {
			m[key] = val
		}
		if v == nil {
			delete(m, k)
		} else {
			m[k] = v
		}
		return m
	}
	cases := map[string]struct {
		args map[string]any
		app  *AppText
	}{
		"the issue changed since": {good, appRead("2026-10-23T11:00:09Z")},
		"no app read":             {good, nil},
		"description not read":    {good, &AppText{TitleSHA256: appTitle, UpdatedAt: laterTime}},
		"bad app title hash":      {good, &AppText{TitleSHA256: "zz", DescriptionSHA256: appDescription, UpdatedAt: laterTime}},
		"agent marker":            {with("agent_marker", "CLAUDECODE"), appRead(laterTime)},
		"marker unsent":           {with("agent_marker", nil), appRead(laterTime)},
		"no confirmed update":     {with("updated_at", nil), appRead(laterTime)},
		"bad confirmed update":    {with("updated_at", "soon"), appRead(laterTime)},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			h := newTextHarness(t)
			h.signal(signalTime, signalTime, "t1", "d1")
			h.check("t1", "d2")
			if out := h.rearm("", c.args, c.app); out["text_vouched"] != false || out["reason"] != "text_changed" {
				t.Fatal(out)
			}
			if h.check("t1", "d2")["changed"] != true || h.followUps() != 1 || h.stored() != "t1|d1" {
				t.Fatal("the mark was lifted or the baseline moved:", h.stored())
			}
		})
	}
	// Hashes in the client's arguments are not arguments.
	h := newTextHarness(t)
	h.signal(signalTime, signalTime, "t1", "d1")
	h.check("t1", "d2")
	if e := h.errorOf("intake_rearm", "", map[string]any{"issue_uuid": intakeIssue, "agent_marker": "", "updated_at": laterTime, "title_sha256": appTitle, "description_sha256": appDescription}); e == "" || h.reason() != "text_changed" {
		t.Fatal("client hashes accepted:", e)
	}
}

func TestIntakeTextCandidatesAndSystemCheck(t *testing.T) {
	h := newTextHarness(t)
	ctx := context.Background()
	if got, err := h.s.IntakeTextCandidates(ctx, h.pid); err != nil || len(got) != 0 {
		t.Fatal(got, err)
	}
	h.signal(signalTime, signalTime, "t1", "d1")
	if got, err := h.s.IntakeTextCandidates(ctx, h.pid); err != nil || len(got) != 1 || got[0] != intakeIssue {
		t.Fatal(got, err)
	}
	st, err := h.s.IntakeTextCheck(ctx, h.pid, intakeIssue, "t2", "d1")
	if err != nil || !st.Tracked || !st.Changed || st.SignalAt != signalTime {
		t.Fatal(st, err)
	}
	if got, _ := h.s.IntakeTextCandidates(ctx, h.pid); len(got) != 0 || h.followUps() != 1 {
		t.Fatal("a marked row is still a candidate:", got)
	}
}

// The confirmed update and the app's are compared as instants, not strings:
// the profile and the app may format the sub-second part differently.
func TestIntakeRearmComparesUpdateTimesAsInstants(t *testing.T) {
	for name, c := range map[string]struct {
		confirmed, app string
		want           bool
	}{
		"no fraction vs milliseconds":    {"2026-10-23T10:00:00Z", "2026-10-23T10:00:00.000Z", true},
		"milliseconds vs no fraction":    {"2026-10-23T10:00:00.000Z", "2026-10-23T10:00:00Z", true},
		"short vs long fraction":         {"2026-10-23T10:00:00.5Z", "2026-10-23T10:00:00.500000000Z", true},
		"offset form of the same time":   {"2026-10-23T12:00:00+02:00", "2026-10-23T10:00:00Z", true},
		"a different millisecond":        {"2026-10-23T10:00:00.001Z", "2026-10-23T10:00:00Z", false},
		"a different second":             {"2026-10-23T10:00:01Z", "2026-10-23T10:00:00.000Z", false},
		"the app's time does not parse":  {"2026-10-23T10:00:00Z", "yesterday", false},
		"the confirmed time is not time": {"yesterday", "2026-10-23T10:00:00Z", false},
	} {
		t.Run(name, func(t *testing.T) {
			h := newTextHarness(t)
			h.signal(signalTime, signalTime, "t1", "d1")
			h.check("t1", "d2")
			out := h.rearm("", map[string]any{"agent_marker": "", "updated_at": c.confirmed}, appRead(c.app))
			if out["text_vouched"] != c.want || (out["reason"] == "") != c.want {
				t.Fatal(out)
			}
		})
	}
}

// A flag set at queueing, with nobody to tell, keeps its hedged wording for
// the retry: the signal's time is no longer known there.
func TestIntakeQueueingFlagWithNoCoordinatorIsToldHedgedLater(t *testing.T) {
	h := newTextHarness(t)
	if _, err := h.s.db.Exec(`UPDATE projects SET coordinator_session=NULL`); err != nil {
		t.Fatal(err)
	}
	h.signal(signalTime, signalTime, "t1", "d1")
	h.close()
	h.signal("2026-10-23T12:00:00Z", "2026-10-23T12:05:00Z", "t1", "d5")
	if h.reason() != "text_maybe_unsent" || h.status()["changed"] != true {
		t.Fatal(h.reason())
	}
	h.call("coordinator_set", "c", map[string]any{"target_session": "c"})
	for i := 0; i < 2; i++ {
		if err := h.s.IntakeTextRetry(context.Background(), h.pid); err != nil {
			t.Fatal(err)
		}
	}
	maybe := h.maybeNotices()
	if h.reason() != "text_maybe" || len(maybe) != 1 || h.followUps() != 0 || !strings.Contains(maybe[0], "(signal at unknown, issue updated at 2026-10-23T12:05:00Z)") || h.notices() != 1 {
		t.Fatal(h.reason(), maybe, h.notices())
	}
}
