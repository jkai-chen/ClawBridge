package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http/httptest"
	"testing"
	"time"

	"clawbridge/internal/bus"
)

// FR57: a lead's own-terminal intake rearm crosses the real handler, is read
// as the app, and vouches for the app's text only if the issue is still at
// the update the person confirmed.

func rearmBody(workspace string, extra map[string]any) string {
	args := map[string]any{"issue_uuid": awIssue, "agent_marker": ""}
	for k, v := range extra {
		args[k] = v
	}
	raw, _ := json.Marshal(map[string]any{"operation": "intake_rearm", "workspace": workspace, "project": "p", "args": args})
	return string(raw)
}

func TestPostgresIntakeRearmThroughTheServer(t *testing.T) {
	f := newAWFixture(t, awOrg)
	ctx := context.Background()
	pid := f.projectID()
	const seen = "2026-10-31T09:05:12.340Z"
	f.mcp.texts = map[string]string{awIssue: "the text the lead saw"}
	f.mcp.updated = map[string]string{awIssue: seen}
	appTitle := sha256.Sum256([]byte("synthetic"))
	appDescription := sha256.Sum256([]byte("the text the lead saw"))

	device := func(role string) *deviceFixture {
		t.Helper()
		who := awUnique(role) + "@device-test.example"
		invite := f.must(bus.Request{Operation: "invite_create", Workspace: f.workspace, Args: map[string]any{"email": who, "grants": map[string]any{"p": role}}})
		d := newPGDeviceClient(t, f.store)
		d.handler = NewHandlerWithOptions(f.store, nil, HandlerOptions{Writer: f.writer, Auth: AuthOptions{Methods: []string{AuthDevice}, PublicOrigin: testOrigin, Devices: f.store}, Now: time.Now})
		joinFloor = 40 * time.Millisecond
		t.Cleanup(func() { joinFloor = 250 * time.Millisecond })
		if w := d.postJoin(d.joinBody(who, invite.Data.(map[string]any)["code"].(string), d.now.Unix(), d.priv), "198.51.100.20"); w.Code != 200 {
			t.Fatal(role, "join:", w.Code, w.Body.String())
		}
		return d
	}
	lead, member := device("lead"), device("member")
	rearm := func(d *deviceFixture, extra map[string]any) (*httptest.ResponseRecorder, map[string]any) {
		t.Helper()
		w := d.do("/v1/execute", rearmBody(f.workspace, extra))
		var out struct{ Data map[string]any }
		_ = json.Unmarshal(w.Body.Bytes(), &out)
		return w, out.Data
	}
	status := func() map[string]any {
		t.Helper()
		return f.must(bus.Request{Operation: "intake_text_status", Workspace: f.workspace, Project: "p", Args: map[string]any{"issue_uuid": awIssue}}).Data.(map[string]any)
	}

	// A trusted signal queued the issue; its text has since changed.
	ev := bus.IntakeEvent{Signal: "S1", Actor: "e7000000-0000-4000-8000-0000000000e1", Trusted: true, At: "2026-10-31T09:00:00.000Z", Issue: bus.IntakeIssue{UUID: awIssue, Identifier: "SYN-6", TitleSHA256: "t0", DescriptionSHA256: "d0", UpdatedAt: "2026-10-31T09:00:01.000Z"}}
	if got, err := f.store.IntakeEnqueueSystem(ctx, pid, ev); err != nil || got != "queued" {
		t.Fatal(got, err)
	}
	if st, err := f.store.IntakeTextCheck(ctx, pid, awIssue, "t0", "d9"); err != nil || !st.Changed {
		t.Fatal(st, err)
	}
	if status()["changed"] != true {
		t.Fatal(status())
	}
	reads := func() int { return f.mcp.count("get_issue") }

	// (b) The issue moved on since the lead's read: the app's read is of
	// another update, so nothing is vouched.
	before := reads()
	if w, out := rearm(lead, map[string]any{"updated_at": "2026-10-31T09:05:12.999Z"}); w.Code != 200 || out["text_vouched"] != false {
		t.Fatal("mismatch:", w.Code, w.Body.String())
	}
	if reads() == before || status()["changed"] != true {
		t.Fatal("no app read, or the flag was lifted:", reads()-before)
	}

	// (d) Hashes in a client's request are not arguments: refused or
	// ignored, never taken.
	if w, out := rearm(lead, map[string]any{"updated_at": seen, "title_sha256": hex.EncodeToString(appTitle[:]), "description_sha256": "00"}); (w.Code == 200 && out["text_vouched"] != false) || status()["changed"] != true {
		t.Fatal("client hashes:", w.Code, w.Body.String())
	}

	// (c) A member who is not a lead: no app read, not vouched.
	before = reads()
	if w, out := rearm(member, map[string]any{"updated_at": seen}); (w.Code == 200 && out["text_vouched"] != false) || status()["changed"] != true {
		t.Fatal("member:", w.Code, w.Body.String())
	}
	if reads() != before {
		t.Fatal("the app read the issue for a member:", reads()-before)
	}

	// (a) The lead confirmed the update the app reads: vouched (the mark is
	// gone; the fixture has no coordinator, so the original notice is still
	// owed), with the
	// app's hashes.
	if w, out := rearm(lead, map[string]any{"updated_at": "2026-10-31T09:05:12.34Z"}); w.Code != 200 || out["text_vouched"] != true || out["reason"] != "no_recipient" {
		t.Fatal("match:", w.Code, w.Body.String())
	}
	if status()["changed"] != false {
		t.Fatal(status())
	}
	if st, err := f.store.IntakeTextCheck(ctx, pid, awIssue, hex.EncodeToString(appTitle[:]), hex.EncodeToString(appDescription[:])); err != nil || !st.Tracked || st.Changed {
		t.Fatal("the baseline is not the app's read:", st, err)
	}
	if st, err := f.store.IntakeTextCheck(ctx, pid, awIssue, hex.EncodeToString(appTitle[:]), "ffffffff"); err != nil || !st.Changed {
		t.Fatal("a later edit was not found:", st, err)
	}
}
