package bus

import (
	"context"
	"strings"
	"testing"
	"time"
)

func (f *pgIdentityFixture) events(principal, device string, all bool) []map[string]any {
	f.t.Helper()
	out, err := f.asDevice(principal, device, Request{Operation: "device_events", Workspace: f.workspace, Args: map[string]any{"all": all}})
	if err != nil {
		f.t.Fatal(err)
	}
	rows := []map[string]any{}
	for _, e := range out.Data.(map[string]any)["events"].([]any) {
		rows = append(rows, e.(map[string]any))
	}
	return rows
}

func (f *pgIdentityFixture) pending(device string) int {
	f.t.Helper()
	n, err := f.s.PendingDeviceEvents(context.Background(), device)
	if err != nil {
		f.t.Fatal(err)
	}
	return n
}

// deviceInvite adds another device to principal from one of theirs.
func (f *pgIdentityFixture) deviceInvite(principal, from string) string {
	f.t.Helper()
	out, err := f.asDevice(principal, from, Request{Operation: "invite_create", Workspace: f.workspace, Args: map[string]any{"email": principal, "purpose": "device"}})
	if err != nil {
		f.t.Fatal(err)
	}
	key := newKey(f.t)
	if _, err = f.join(principal, out.Data.(map[string]any)["code"].(string), key); err != nil {
		f.t.Fatal(err)
	}
	return keyID(f.t, key)
}

func kinds(events []map[string]any) string {
	out := []string{}
	for _, e := range events {
		out = append(out, e["kind"].(string)+":"+e["route"].(string)+":"+e["device_id"].(string))
	}
	return strings.Join(out, ",")
}

// §17.3: every device added to or revoked from a user reaches the user's
// other devices and the main owner's, until each acknowledges it; never the
// device it is about, the device that caused it, a CI device, or another
// member.
func TestPostgresDeviceEvents(t *testing.T) {
	f := newPGIdentityFixture(t)
	ownerKey := newKey(t)
	if _, err := f.as(f.owner, "sub-owner", Request{Operation: "device_enroll", Workspace: f.workspace, Args: map[string]any{"public_key": ownerKey, "label": "owner laptop", "key_store": "keyring"}}); err != nil {
		t.Fatal(err)
	}
	o := keyID(t, ownerKey)
	if n := f.pending(o); n != 0 {
		t.Fatal("a device was alerted about its own enrolment:", n)
	}
	ada := newID("ada") + "@identity-test.example"
	key := newKey(t)
	if _, err := f.join(ada, f.invite(f.owner, ada, "member")["code"].(string), key); err != nil {
		t.Fatal(err)
	}
	a1 := keyID(t, key)
	a2 := f.deviceInvite(ada, a1)
	// a1 made a2's invite, so it isn't told (§17.2); a2 hears nothing of
	// itself nor of a1, which came first. The owner hears of both, below.
	if got := kinds(f.events(ada, a1, false)); got != "" {
		t.Fatal(got)
	}
	if n := f.pending(a2); n != 0 {
		t.Fatal("the new device was alerted:", n)
	}
	if got := kinds(f.events(f.owner, o, false)); got != "added:device_add:"+a2+",added:invite:"+a1 {
		t.Fatal("the owner's device:", got)
	}
	e := f.events(f.owner, o, false)[0]
	if e["label"] != "laptop" || e["actor"] != ada || e["principal"] != ada || e["actor_device"] != a1 {
		t.Fatal(e)
	}
	// A revoke reaches the user's remaining devices and the owner, not the
	// device that did it (R6).
	a3 := f.deviceInvite(ada, a1)
	if _, err := f.asDevice(ada, a2, Request{Operation: "device_revoke", Workspace: f.workspace, Args: map[string]any{"device_id": a3}}); err != nil {
		t.Fatal(err)
	}
	if got := kinds(f.events(ada, a1, false)); got != "revoked:revoke:"+a3 {
		t.Fatal(got)
	}
	if got := kinds(f.events(ada, a2, false)); got != "added:device_add:"+a3 {
		t.Fatal("the revoking device heard its own revoke:", got)
	}
	if f.events(ada, a1, false)[0]["actor_device"] != a2 {
		t.Fatal("the revoke doesn't name the device that did it")
	}
	// Another member's device hears nothing of ada's.
	bob := newID("bob") + "@identity-test.example"
	bobKey := newKey(t)
	if _, err := f.join(bob, f.invite(f.owner, bob, "member")["code"].(string), bobKey); err != nil {
		t.Fatal(err)
	}
	if n := f.pending(keyID(t, bobKey)); n != 0 {
		t.Fatal("bob heard of ada's devices:", n)
	}
	// A CI device is no event, and is never alerted.
	ciKey := newKey(t)
	before := f.pending(o)
	if _, err := f.auth(f.owner, "device_ci_create", map[string]any{"name": "deploy", "grants": map[string]any{"p": "reader"}, "expires_days": 30, "public_key": ciKey}); err != nil {
		t.Fatal(err)
	}
	if n := f.pending(o); n != before {
		t.Fatal("a CI device raised an event")
	}
	if n := f.pending(keyID(t, ciKey)); n != 0 {
		t.Fatal("a CI device was alerted")
	}
	if _, err := f.asDevice("ci:deploy@"+strings.ToLower(f.workspace), keyID(t, ciKey), Request{Operation: "device_events", Workspace: f.workspace, Args: map[string]any{}}); err != nil {
		t.Fatal(err)
	}
	// Acknowledging is per device: a1's ack leaves the owner's alert.
	pending := f.pending(a1)
	first := f.events(ada, a1, false)[0]["event_id"].(string)
	ack := func(device string, args map[string]any) (Result, error) {
		return f.asDevice(ada, device, Request{Operation: "device_ack", Workspace: f.workspace, Args: args})
	}
	if out, err := ack(a1, map[string]any{"event_ids": []any{first, first}}); err != nil || out.Data.(map[string]any)["acknowledged"] != 1 {
		t.Fatal(out, err)
	}
	if n := f.pending(a1); n != pending-1 {
		t.Fatal(n)
	}
	if got := f.events(f.owner, o, false); len(got) != before {
		t.Fatal("a1's ack cleared the owner's alert")
	}
	// An event a device can't see is not found, whether it's about the
	// device itself or another member's.
	own := f.events(f.owner, o, true)
	for _, ev := range own {
		if ev["device_id"] == a2 {
			if _, err := ack(a2, map[string]any{"event_ids": []any{ev["event_id"]}}); err == nil || !strings.Contains(err.Error(), "not found") {
				t.Fatal("acked an event about itself:", err)
			}
		}
	}
	for _, bad := range []map[string]any{{}, {"all": true, "event_ids": []any{first}}, {"event_ids": []any{"nope"}}} {
		if _, err := ack(a1, bad); err == nil {
			t.Fatal("accepted", bad)
		}
	}
	if _, err := ack(a1, map[string]any{"all": true}); err != nil {
		t.Fatal(err)
	}
	if n := f.pending(a1); n != 0 {
		t.Fatal(n)
	}
	all := f.events(ada, a1, true)
	if len(all) != pending || all[0]["acked"] != true {
		t.Fatal(all)
	}
	// Events are per device: a Google sign-in has none.
	if _, err := f.as(f.owner, "sub-owner", Request{Operation: "device_events", Workspace: f.workspace, Args: map[string]any{}}); !refusedWith(err, "per device") {
		t.Fatal(err)
	}
	// Recovery that revokes the other devices raises one event each.
	rec, err := f.auth(f.owner, "invite_create", map[string]any{"email": ada, "purpose": "recovery", "revoke_others": true})
	if err != nil {
		t.Fatal(err)
	}
	f.lastJoined = newKey(t)
	if _, err = f.join(ada, rec["code"].(string), f.lastJoined); err != nil {
		t.Fatal(err)
	}
	got := kinds(f.events(f.owner, o, false))
	if !strings.Contains(got, "added:recovery:") || !strings.Contains(got, "revoked:recovery:"+a1) || !strings.Contains(got, "revoked:recovery:"+a2) || strings.Contains(got, "revoked:recovery:"+a3) {
		t.Fatal(got)
	}
	// The recovered device isn't told of the revokes its own join made (L2).
	recovered := keyID(t, f.lastJoined)
	if n := f.pending(recovered); n != 0 {
		t.Fatal("the recovery device heard its own revokes:", kinds(f.events(ada, recovered, false)))
	}
	// The owner removing a member is an event for each of their devices, on
	// the owner's other devices: a stolen owner device can't do it quietly
	// (M1).
	second := newKey(t)
	if _, err = f.as(f.owner, "sub-owner", Request{Operation: "device_enroll", Workspace: f.workspace, Args: map[string]any{"public_key": second, "label": "owner desk", "key_store": "keyring"}}); err != nil {
		t.Fatal(err)
	}
	bobDevice := keyID(t, bobKey)
	if _, err = f.asDevice(f.owner, o, Request{Operation: "user_revoke", Workspace: f.workspace, Args: map[string]any{"principal": bob}}); err != nil {
		t.Fatal(err)
	}
	if got := kinds(f.events(f.owner, keyID(t, second), false)); got != "revoked:user_revoked:"+bobDevice {
		t.Fatal(got)
	}
	if got := kinds(f.events(f.owner, o, false)); strings.Contains(got, "user_revoked") {
		t.Fatal("the revoking device heard its own revoke:", got)
	}
}

// Events last 90 days: an older one isn't shown, and the next event in the
// workspace deletes it and its acks.
func TestPostgresDeviceEventsExpire(t *testing.T) {
	f := newPGIdentityFixture(t)
	ada := newID("ada") + "@identity-test.example"
	key := newKey(t)
	if _, err := f.join(ada, f.invite(f.owner, ada, "member")["code"].(string), key); err != nil {
		t.Fatal(err)
	}
	a1 := keyID(t, key)
	wid := f.workspaceID(f.workspace)
	old := time.Now().UTC().Add(-91 * 24 * time.Hour).Format(timestampLayout)
	f.exec(`UPDATE devices SET created_at=$1 WHERE device_id=$2`, old, a1)
	f.exec(`INSERT INTO device_events(event_id,workspace_id,principal,device_id,kind,label,route,actor,created_at) VALUES('dve_old',$1,$2,'dev_gone','added','old','invite',$3,$4)`, wid, ada, f.owner, old)
	f.exec(`INSERT INTO device_event_acks(event_id,device_id,acked_at) VALUES('dve_old','dev_other',$1)`, old)
	if n := f.pending(a1); n != 0 {
		t.Fatal("an expired event was shown:", n)
	}
	f.deviceInvite(ada, a1)
	for _, q := range []string{`SELECT COUNT(*) FROM device_events WHERE event_id=$1`, `SELECT COUNT(*) FROM device_event_acks WHERE event_id=$1`} {
		if n := f.count(q, "dve_old"); n != 0 {
			t.Fatal("not purged:", q)
		}
	}
	// a1 made the invite, so the new event isn't a1's to see either.
	if n := f.pending(a1); n != 0 {
		t.Fatal(n)
	}
}

// A device label is shown on the victim's other terminals (R1): every write
// path refuses controls, escapes, bidi and other format characters, and
// more than 64 bytes.
func TestPostgresDeviceLabelIsPrintableText(t *testing.T) {
	f := newPGIdentityFixture(t)
	ada := newID("ada") + "@identity-test.example"
	key := newKey(t)
	if _, err := f.join(ada, f.invite(f.owner, ada, "member")["code"].(string), key); err != nil {
		t.Fatal(err)
	}
	a1 := keyID(t, key)
	bad := []string{"\x1b[1A\x1b[2Kyou", "ok\x9b2K", "tab\there", "del\x7f", "rtl‮exe", "zw​sp", "\xff", strings.Repeat("x", 65)}
	for _, label := range bad {
		out, err := f.asDevice(ada, a1, Request{Operation: "invite_create", Workspace: f.workspace, Args: map[string]any{"email": ada, "purpose": "device"}})
		if err != nil {
			t.Fatal(err)
		}
		code := out.Data.(map[string]any)["code"].(string)
		if _, err = f.s.Join(context.Background(), JoinRequest{Email: ada, Code: code, PublicKey: newKey(t), Label: label, KeyStore: "keyring"}); err == nil {
			t.Fatalf("joined with label %q", label)
		}
		if _, err = f.asDevice(ada, a1, Request{Operation: "device_label", Workspace: f.workspace, Args: map[string]any{"device_id": a1, "label": label}}); err == nil {
			t.Fatalf("relabelled to %q", label)
		}
		if _, err = f.as(f.owner, "sub-owner", Request{Operation: "device_enroll", Workspace: f.workspace, Args: map[string]any{"public_key": newKey(t), "label": label, "key_store": "keyring"}}); err == nil {
			t.Fatalf("enrolled with label %q", label)
		}
		f.exec(`UPDATE invites SET revoked_at=$1 WHERE email=$2 AND used_at IS NULL AND revoked_at IS NULL`, stamp(), ada)
	}
	for _, label := range []string{"Ada's desktop", "café · 東京", strings.Repeat("x", 64), strings.Repeat("東", 64)} {
		if _, err := f.asDevice(ada, a1, Request{Operation: "device_label", Workspace: f.workspace, Args: map[string]any{"device_id": a1, "label": label}}); err != nil {
			t.Fatalf("%q: %v", label, err)
		}
	}
}

// §17.2: a device invite lasts 15 minutes unless asked, at most 24 hours;
// it works only while the device that made it is live; and the device that
// made it isn't alerted when it's used (L1).
func TestPostgresDeviceInviteLifeAndIssuer(t *testing.T) {
	f := newPGIdentityFixture(t)
	ada := newID("ada") + "@identity-test.example"
	key := newKey(t)
	if _, err := f.join(ada, f.invite(f.owner, ada, "member")["code"].(string), key); err != nil {
		t.Fatal(err)
	}
	a1 := keyID(t, key)
	add := func(device string, args map[string]any) (map[string]any, error) {
		a := map[string]any{"email": ada, "purpose": "device"}
		for k, v := range args {
			a[k] = v
		}
		out, err := f.asDevice(ada, device, Request{Operation: "invite_create", Workspace: f.workspace, Args: a})
		if err != nil {
			return nil, err
		}
		return out.Data.(map[string]any), nil
	}
	out, err := add(a1, nil)
	if err != nil {
		t.Fatal(err)
	}
	expires, _ := time.Parse(timestampLayout, out["expires_at"].(string))
	if left := time.Until(expires); left < 14*time.Minute || left > 15*time.Minute {
		t.Fatal("default life", left)
	}
	if f.inviteRow(out["invite_id"].(string))["issued_device"] != a1 {
		t.Fatal("the issuing device wasn't recorded")
	}
	for _, bad := range []map[string]any{{"expires_minutes": 24*60 + 1}, {"expires_hours": 25}, {"expires_minutes": 0}, {"expires_hours": 1, "expires_minutes": 60}} {
		if _, err = add(a1, bad); err == nil {
			t.Fatal("accepted", bad)
		}
	}
	if _, err = add(a1, map[string]any{"expires_minutes": 24 * 60}); err != nil {
		t.Fatal(err)
	}
	f.exec(`UPDATE invites SET revoked_at=$1 WHERE email=$2 AND used_at IS NULL AND revoked_at IS NULL`, stamp(), ada)
	// Used: the issuing device isn't told; another device of ada's is.
	a2 := f.deviceInvite(ada, a1)
	if n := f.pending(a1); n != 0 {
		t.Fatal("the issuing device was alerted to its own invite:", kinds(f.events(ada, a1, false)))
	}
	// A device invite dies with the device that made it.
	pending, err := add(a2, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.asDevice(ada, a1, Request{Operation: "device_revoke", Workspace: f.workspace, Args: map[string]any{"device_id": a2}}); err != nil {
		t.Fatal(err)
	}
	if _, err = f.join(ada, pending["code"].(string), newKey(t)); err == nil {
		t.Fatal("joined with an invite whose issuing device is revoked")
	}
	if reason := f.failReason(pending["invite_id"].(string)); reason != "issuer_device_gone" {
		t.Fatal(reason)
	}
	// A device invite made without a device (a Google sign-in) has no
	// issuing device to outlive.
	google, err := f.as(ada, "sub-"+ada, Request{Operation: "invite_create", Workspace: f.workspace, Args: map[string]any{"email": ada, "purpose": "device"}})
	if err == nil {
		if _, err = f.join(ada, google.Data.(map[string]any)["code"].(string), newKey(t)); err != nil {
			t.Fatal(err)
		}
	}
}

// securityNotices is the security messages addressed to session name in
// project p.
func (f *pgIdentityFixture) securityNotices(name string) []string {
	f.t.Helper()
	rows, err := f.s.db.Query(`SELECT m.body FROM messages m JOIN projects p ON p.id=m.project_id JOIN workspaces w ON w.id=p.workspace_id WHERE w.name=$1 AND p.slug='p' AND m.kind='security' AND m.address=$2 ORDER BY m.sequence`, f.workspace, name)
	if err != nil {
		f.t.Fatal(err)
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var body string
		if err = rows.Scan(&body); err != nil {
			f.t.Fatal(err)
		}
		out = append(out, body)
	}
	return out
}

// A1-D4: a device event is also one security notice to each project's
// coordinator session, when the main owner registered it; never to one run
// by anyone else; once per event; with the user, label, device and what
// happened, nothing more.
func TestPostgresOwnerCoordinatorHearsOfDevices(t *testing.T) {
	f := newPGIdentityFixture(t)
	f.must(f.owner, "sub-owner", Request{Operation: "register_session", Workspace: f.workspace, Project: "p", Session: "coord", Args: map[string]any{"instance": "coord-i"}})
	f.exec(`UPDATE projects SET coordinator_session=(SELECT id FROM sessions WHERE name='coord' AND project_id=projects.id) WHERE slug='p' AND workspace_id=$1`, f.workspaceID(f.workspace))
	ada := newID("ada") + "@identity-test.example"
	key := newKey(t)
	if _, err := f.join(ada, f.invite(f.owner, ada, "member")["code"].(string), key); err != nil {
		t.Fatal(err)
	}
	got := f.securityNotices("coord")
	if len(got) != 1 || !strings.Contains(got[0], `new device `+keyID(t, key)+` added to `+ada) || !strings.Contains(got[0], "via invite") || strings.Contains(got[0], key) || strings.Contains(got[0], "laptop") {
		t.Fatal(got)
	}
	// Once per event, however often it's sent.
	var event string
	if err := f.s.db.QueryRow(`SELECT event_id FROM device_events WHERE device_id=$1`, keyID(t, key)).Scan(&event); err != nil {
		t.Fatal(err)
	}
	raw, err := f.s.db.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	tx := &transaction{Tx: raw, postgres: true}
	if err = f.s.ownerDeviceNotices(context.Background(), tx, f.workspaceID(f.workspace), event, ada, keyID(t, key), "added", "invite", f.owner, stamp()); err != nil {
		t.Fatal(err)
	}
	if err = raw.Commit(); err != nil {
		t.Fatal(err)
	}
	if got = f.securityNotices("coord"); len(got) != 1 {
		t.Fatal("sent twice:", got)
	}
	// A revoke too.
	if _, err = f.auth(f.owner, "device_revoke", map[string]any{"device_id": keyID(t, key)}); err != nil {
		t.Fatal(err)
	}
	if got = f.securityNotices("coord"); len(got) != 2 || !strings.Contains(got[1], "revoked") || !strings.Contains(got[1], "by "+f.owner) {
		t.Fatal(got)
	}
	// A coordinator someone else registered hears nothing.
	f.role(ada, "lead")
	f.must(ada, "sub-ada", Request{Operation: "register_session", Workspace: f.workspace, Project: "p", Session: "ada-coord", Args: map[string]any{"instance": "ada-coord-i"}})
	f.exec(`UPDATE projects SET coordinator_session=(SELECT id FROM sessions WHERE name='ada-coord' AND project_id=projects.id) WHERE slug='p' AND workspace_id=$1`, f.workspaceID(f.workspace))
	bob := newID("bob") + "@identity-test.example"
	if _, err = f.join(bob, f.invite(f.owner, bob, "member")["code"].(string), newKey(t)); err != nil {
		t.Fatal(err)
	}
	if got = f.securityNotices("ada-coord"); len(got) != 0 {
		t.Fatal("a coordinator another principal runs was told:", got)
	}
	if got = f.securityNotices("coord"); len(got) != 2 {
		t.Fatal("the old coordinator was told:", got)
	}
	// Nor does the owner's coordinator once it's retired.
	f.exec(`UPDATE projects SET coordinator_session=(SELECT id FROM sessions WHERE name='coord' AND project_id=projects.id) WHERE slug='p' AND workspace_id=$1`, f.workspaceID(f.workspace))
	f.exec(`UPDATE sessions SET status='retired' WHERE name='coord'`)
	cy := newID("cy") + "@identity-test.example"
	if _, err = f.join(cy, f.invite(f.owner, cy, "member")["code"].(string), newKey(t)); err != nil {
		t.Fatal(err)
	}
	if got = f.securityNotices("coord"); len(got) != 2 {
		t.Fatal("a retired coordinator was told:", got)
	}
}
