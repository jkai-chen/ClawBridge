package server

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"clawbridge/internal/linearapp"
)

const (
	probeIssue = "00000000-0000-4000-8000-0000000000e1"
	probeTeam  = "00000000-0000-4000-8000-0000000000aa"
	probeUser  = "00000000-0000-4000-8000-0000000000d1"
	probeOwner = "owner@probe.example"
)

// fakeSubscribeApp is the fixture as the app reads it, and Linear's answer
// to each subscribe, which adds the person to the read-back when it says
// subscribed.
type fakeSubscribeApp struct {
	title, team string
	comments    int
	subscribers []map[string]any
	classes     map[string]string
	sent        []string
	// ghost answers subscribed without the person appearing; appear adds
	// them whatever the answer; stranger adds someone else after a call.
	ghost, appear map[string]bool
	stranger      bool
}

func (a *fakeSubscribeApp) GraphQL(_ context.Context, _ string, _ map[string]any, out any) error {
	comments := []any{}
	for i := 0; i < a.comments; i++ {
		comments = append(comments, map[string]any{"id": "c"})
	}
	nodes := []any{}
	for _, s := range a.subscribers {
		nodes = append(nodes, s)
	}
	raw, _ := json.Marshal(map[string]any{"issue": map[string]any{"id": probeIssue, "title": a.title, "team": map[string]any{"id": a.team}, "comments": map[string]any{"nodes": comments}, "subscribers": map[string]any{"nodes": nodes}}})
	return json.Unmarshal(raw, out)
}

func (a *fakeSubscribeApp) SubscribeIssue(_ context.Context, issue, email, user string) (string, error) {
	who := email + user
	a.sent = append(a.sent, who)
	class := "subscribed"
	if c, ok := a.classes[who]; ok {
		class = c
	}
	if a.stranger {
		a.subscribers = append(a.subscribers, map[string]any{"id": "00000000-0000-4000-8000-0000000000f9", "email": "someone@probe.example"})
	}
	if (class == "subscribed" && !a.ghost[who]) || a.appear[who] {
		a.subscribers = append(a.subscribers, map[string]any{"id": map[bool]string{true: "00000000-0000-4000-8000-0000000000f1", false: user}[email != ""], "email": email})
	}
	return class, nil
}

func runSubscribeProbe(t *testing.T, app *fakeSubscribeApp, user string, dryRun bool) (map[string]any, string, error) {
	t.Helper()
	prev := newSubscribeProbeApp
	newSubscribeProbeApp = func(linearapp.Store) (subscribeProbeApp, error) { return app, nil }
	t.Cleanup(func() { newSubscribeProbeApp = prev })
	var out bytes.Buffer
	err := linearAppSubscribeProbe(context.Background(), nil, strings.ToUpper(probeIssue), probeTeam, probeOwner, user, dryRun, &out)
	if strings.Contains(out.String(), probeOwner) {
		t.Fatal("the owner's email was printed:", out.String())
	}
	var got map[string]map[string]any
	_ = json.Unmarshal(out.Bytes(), &got)
	return got["subscribe"], out.String(), err
}

func fixture() *fakeSubscribeApp {
	return &fakeSubscribeApp{title: subscribeFixtureTitle, team: probeTeam}
}

// The fixture guard refuses, sending nothing: another issue, another team
// (printed, L3), a comment, or any subscriber already there (L1: the
// creator unsubscribes first, or the email call proves nothing).
func TestSubscribeProbeGuard(t *testing.T) {
	for name, app := range map[string]*fakeSubscribeApp{
		"title":      {title: "Real work", team: probeTeam},
		"team":       {title: subscribeFixtureTitle, team: "00000000-0000-4000-8000-0000000000bb"},
		"comment":    {title: subscribeFixtureTitle, team: probeTeam, comments: 1},
		"subscribed": {title: subscribeFixtureTitle, team: probeTeam, subscribers: []map[string]any{{"id": "00000000-0000-4000-8000-0000000000f1", "email": probeOwner}}},
	} {
		got, _, err := runSubscribeProbe(t, app, probeUser, false)
		if err == nil || !strings.Contains(err.Error(), "nothing was sent") || len(app.sent) != 0 {
			t.Fatal(name, err, app.sent)
		}
		if name == "team" && got["issue_team_id"] != "00000000-0000-4000-8000-0000000000bb" {
			t.Fatal("the fixture's team isn't printed:", got)
		}
		if name == "subscribed" && !strings.Contains(err.Error(), "inconclusive") {
			t.Fatal(err)
		}
	}
	// A dry run stops after the guard.
	app := fixture()
	if got, _, err := runSubscribeProbe(t, app, probeUser, true); err != nil || got["dry_run"] != true || got["team_id"] != probeTeam || len(app.sent) != 0 {
		t.Fatal(got, err, app.sent)
	}
}

// A pass is both calls subscribed and both people in the read-back; any
// refusal, or a read-back without them, is no pass. The cleanup names the
// fixture's ID.
func TestSubscribeProbeRun(t *testing.T) {
	app := fixture()
	got, _, err := runSubscribeProbe(t, app, probeUser, false)
	back, _ := got["read_back"].(map[string]any)
	if err != nil || got["pass"] != true || back["owner_subscribed"] != true || back["user_subscribed"] != true || !strings.Contains(got["cleanup"].(string), probeIssue) {
		t.Fatal(got, err)
	}
	if strings.Join(app.sent, ",") != probeOwner+","+probeUser {
		t.Fatal(app.sent)
	}
	// Linear refusing the app subscribing someone else is recorded, not a pass.
	app = fixture()
	app.classes = map[string]string{probeOwner: "scope"}
	if got, _, _ = runSubscribeProbe(t, app, "", false); got["pass"] != false || got["calls"].(map[string]any)["email"] != "scope" {
		t.Fatal(got)
	}
	// Mixed: by ID works, by email doesn't.
	app = fixture()
	app.classes = map[string]string{probeOwner: "refused"}
	if got, _, _ = runSubscribeProbe(t, app, probeUser, false); got["pass"] != false || got["calls"].(map[string]any)["user"] != "subscribed" {
		t.Fatal(got)
	}
	// A refused email call is no pass, even if the owner shows up anyway.
	app = fixture()
	app.classes, app.appear = map[string]string{probeOwner: "refused"}, map[string]bool{probeOwner: true}
	if got, _, _ = runSubscribeProbe(t, app, "", false); got["pass"] != false {
		t.Fatal(got)
	}
	// The second person answered subscribed but isn't there: no pass.
	app = fixture()
	app.ghost = map[string]bool{probeUser: true}
	if got, _, _ = runSubscribeProbe(t, app, probeUser, false); got["pass"] != false || got["read_back"].(map[string]any)["user_subscribed"] != false {
		t.Fatal(got)
	}
	// Someone else, not the owner, in the read-back: no pass.
	app = fixture()
	app.ghost, app.stranger = map[string]bool{probeOwner: true}, true
	if got, _, _ = runSubscribeProbe(t, app, "", false); got["pass"] != false || got["read_back"].(map[string]any)["owner_subscribed"] != false {
		t.Fatal(got)
	}
	// Linear says subscribed but the read-back hasn't the person: no pass.
	app = fixture()
	app.classes = map[string]string{probeOwner: "subscribed"}
	quiet := &quietApp{app}
	prev := newSubscribeProbeApp
	newSubscribeProbeApp = func(linearapp.Store) (subscribeProbeApp, error) { return quiet, nil }
	t.Cleanup(func() { newSubscribeProbeApp = prev })
	var out bytes.Buffer
	if err := linearAppSubscribeProbe(context.Background(), nil, probeIssue, probeTeam, probeOwner, "", false, &out); err != nil || !strings.Contains(out.String(), `"pass":false`) {
		t.Fatal(out.String(), err)
	}
}

// quietApp answers subscribed without the person ever appearing.
type quietApp struct{ *fakeSubscribeApp }

func (q *quietApp) SubscribeIssue(_ context.Context, _, email, user string) (string, error) {
	q.sent = append(q.sent, email+user)
	return "subscribed", nil
}

// Linear subscribes the fixture's creator: someone already there who is
// neither target is fine, counted and never named. A target already there
// is inconclusive, sending nothing; someone else added (or gone) by the
// time of the read-back is no pass.
func TestSubscribeProbeTargetsOnly(t *testing.T) {
	const creator = "00000000-0000-4000-8000-0000000000c7"
	creatorRow := map[string]any{"id": creator, "email": "creator@probe.example"}
	app := fixture()
	app.subscribers = []map[string]any{creatorRow}
	got, out, err := runSubscribeProbe(t, app, probeUser, false)
	back, _ := got["read_back"].(map[string]any)
	if err != nil || got["pass"] != true || got["subscribers_before"] != float64(1) || got["others_before"] != float64(1) || back["others"] != float64(1) || back["subscribers"] != float64(3) {
		t.Fatal(got, err)
	}
	if strings.Contains(out, "creator@probe.example") || strings.Contains(out, creator) {
		t.Fatal("another subscriber named:", out)
	}
	// --user already subscribed: inconclusive, nothing sent.
	app = fixture()
	app.subscribers = []map[string]any{creatorRow, {"id": probeUser, "email": "second@probe.example"}}
	if _, _, err = runSubscribeProbe(t, app, probeUser, false); err == nil || !strings.Contains(err.Error(), "inconclusive") || len(app.sent) != 0 {
		t.Fatal(err, app.sent)
	}
	// Someone else added after: no pass.
	app = fixture()
	app.subscribers = []map[string]any{creatorRow}
	app.stranger = true
	if got, _, _ = runSubscribeProbe(t, app, "", false); got["pass"] != false || got["read_back"].(map[string]any)["others_changed"] != true {
		t.Fatal(got)
	}
	// Someone gone by the read-back: no pass either.
	vanish := &vanishingApp{fakeSubscribeApp: fixture()}
	vanish.subscribers = []map[string]any{creatorRow}
	prev := newSubscribeProbeApp
	newSubscribeProbeApp = func(linearapp.Store) (subscribeProbeApp, error) { return vanish, nil }
	t.Cleanup(func() { newSubscribeProbeApp = prev })
	var buf bytes.Buffer
	if err = linearAppSubscribeProbe(context.Background(), nil, probeIssue, probeTeam, probeOwner, "", false, &buf); err != nil || !strings.Contains(buf.String(), `"pass":false`) || !strings.Contains(buf.String(), `"others_changed":true`) {
		t.Fatal(buf.String(), err)
	}
}

// vanishingApp drops everyone else on the first subscribe.
type vanishingApp struct{ *fakeSubscribeApp }

func (v *vanishingApp) SubscribeIssue(ctx context.Context, issue, email, user string) (string, error) {
	kept := []map[string]any{}
	for _, s := range v.subscribers {
		if s["email"] == email || s["id"] == user {
			kept = append(kept, s)
		}
	}
	v.subscribers = kept
	return v.fakeSubscribeApp.SubscribeIssue(ctx, issue, email, user)
}

// The arguments are checked before anything is opened.
func TestSubscribeProbeArguments(t *testing.T) {
	for _, a := range [][4]string{{"x", probeTeam, probeOwner, ""}, {probeIssue, "x", probeOwner, ""}, {probeIssue, probeTeam, "", ""}, {probeIssue, probeTeam, probeOwner, "x"}} {
		if err := linearAppSubscribeProbe(context.Background(), nil, a[0], a[1], a[2], a[3], false, &bytes.Buffer{}); err == nil || !strings.Contains(err.Error(), "--subscribe needs") {
			t.Fatal(a, err)
		}
	}
}

// The operator's --subscribe takes its own flags only, and without
// --confirm says what it would do, opening nothing.
func TestOperatorSubscribeFlags(t *testing.T) {
	for want, args := range map[string][]string{
		"not --project":                               {"linear-app-probe", "--confirm", "--subscribe", "--project", probeTeam},
		"--project-status or":                         {"linear-app-probe", "--confirm", "--subscribe", "--project-status"},
		"or --allow-member":                           {"linear-app-probe", "--confirm", "--subscribe", "--allow-member", probeUser},
		"--health-order or --subscribe":               {"linear-app-probe", "--confirm", "--rate-headers", "--subscribe"},
		"never unsubscribes":                          {"linear-app-probe", "--subscribe", "--issue", probeIssue, "--team", probeTeam, "--email", probeOwner},
		"needs --p8, --project-status or --subscribe": {"linear-app-probe", "--confirm", "--dry-run"},
	} {
		if err := RunOperator(context.Background(), args, &bytes.Buffer{}); err == nil || !strings.Contains(err.Error(), want) {
			t.Fatal(args, err)
		}
	}
}

// --email-stdin reads the owner's email from one line of stdin, so it is
// never on the command line: the address alone, trimmed, or a refusal
// that never echoes it. It goes with --subscribe, in place of --email.
func TestSubscribeProbeEmailStdin(t *testing.T) {
	for in, want := range map[string]string{probeOwner + "\n": probeOwner, "  " + probeOwner + " \r\n": probeOwner, probeOwner: probeOwner} {
		if got, err := readProbeEmail(strings.NewReader(in)); err != nil || got != want {
			t.Fatal(in, got, err)
		}
	}
	for _, in := range []string{"", "\n", "not-an-email", "Owner <" + probeOwner + ">", "a@b@c", strings.Repeat("a", 320) + "@x.example"} {
		if _, err := readProbeEmail(strings.NewReader(in)); err == nil || (in != "" && strings.Contains(err.Error(), in)) {
			t.Fatal(in, err)
		}
	}
	prev := operatorStdin
	t.Cleanup(func() { operatorStdin = prev })
	args := []string{"linear-app-probe", "--subscribe", "--issue", probeIssue, "--team", probeTeam, "--email-stdin", "--database-url-env", "CLAWBRIDGE_TEST_NO_SUCH_DSN"}
	for want, extra := range map[string][]string{
		"in place of --email": {"--confirm", "--email", probeOwner},
		"never unsubscribes":  nil,
	} {
		stdin := strings.NewReader(probeOwner + "\n")
		operatorStdin = stdin
		if err := RunOperator(context.Background(), append(append([]string{}, args...), extra...), &bytes.Buffer{}); err == nil || !strings.Contains(err.Error(), want) || stdin.Len() == 0 {
			t.Fatal(want, err, stdin.Len())
		}
	}
	if err := RunOperator(context.Background(), []string{"linear-app-probe", "--confirm", "--email-stdin", "--p8", "--project", probeTeam}, &bytes.Buffer{}); err == nil || !strings.Contains(err.Error(), "goes with --subscribe") {
		t.Fatal(err)
	}
	// A bad line is refused before anything is opened; a good one is read
	// and the run goes on to the database, the email in no output.
	operatorStdin = strings.NewReader("nobody\n")
	if err := RunOperator(context.Background(), append(append([]string{}, args...), "--confirm"), &bytes.Buffer{}); err == nil || !strings.Contains(err.Error(), "stdin holds no email") {
		t.Fatal(err)
	}
	operatorStdin = strings.NewReader(probeOwner + "\n")
	var out bytes.Buffer
	err := RunOperator(context.Background(), append(append([]string{}, args...), "--confirm"), &out)
	if err == nil || !strings.Contains(err.Error(), "Postgres connection") || strings.Contains(err.Error()+out.String(), probeOwner) {
		t.Fatal(err, out.String())
	}
}
