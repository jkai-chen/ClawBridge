package bus

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// dormancyClock moves the host clock by whole offsets from now.
type dormancyClock struct{ base, now time.Time }

func newDormancyClock(t *testing.T) *dormancyClock {
	c := &dormancyClock{base: time.Now().UTC()}
	c.now = c.base
	hostClock = func() time.Time { return c.now }
	t.Cleanup(func() { hostClock = time.Now })
	return c
}

func (c *dormancyClock) at(d time.Duration) { c.now = c.base.Add(d) }

// use is one signed request's nonce: what the server records for it.
func (f *pgIdentityFixture) use(device string) error {
	return f.s.UseNonce(context.Background(), device, newID("nonce-"))
}

func (f *pgIdentityFixture) dormancyEvents(principal, device, about, kind string) []map[string]any {
	f.t.Helper()
	out := []map[string]any{}
	for _, e := range f.events(principal, device, true) {
		if e["device_id"] == about && e["kind"] == kind {
			out = append(out, e)
		}
	}
	return out
}

// deviceUse is a device's last use and revocation, "" for none.
func (f *pgIdentityFixture) deviceUse(id string) (used, revoked string) {
	f.t.Helper()
	if err := f.s.db.QueryRow(`SELECT COALESCE(last_used_at,''),COALESCE(revoked_at,'') FROM devices WHERE device_id=$1`, id).Scan(&used, &revoked); err != nil {
		f.t.Fatal(err)
	}
	return used, revoked
}

func (f *pgIdentityFixture) listed(principal, device, about string) map[string]any {
	f.t.Helper()
	out, err := f.asDevice(principal, device, Request{Operation: "device_list", Workspace: f.workspace, Args: map[string]any{"principal": f.principalOf(about)}})
	if err != nil {
		f.t.Fatal(err)
	}
	for _, d := range out.Data.(map[string]any)["devices"].([]any) {
		if d.(map[string]any)["device_id"] == about {
			return d.(map[string]any)
		}
	}
	f.t.Fatal("not listed:", about)
	return nil
}

func (f *pgIdentityFixture) principalOf(device string) string {
	f.t.Helper()
	var p string
	if err := f.s.db.QueryRow(`SELECT principal FROM devices WHERE device_id=$1`, device).Scan(&p); err != nil {
		f.t.Fatal(err)
	}
	return p
}

// §17.4: a human device unused for the idle limit is refused, and nothing
// it holds goes; before that its user's other devices and the main owner's
// are warned from day 80 (day 60 for the owner's own); another device of
// the user, or the main owner alone, confirms it; CI never goes dormant.
func TestPostgresDeviceDormancy(t *testing.T) {
	f := newPGIdentityFixture(t)
	clock := newDormancyClock(t)
	day := 24 * time.Hour
	ownerKey, spareKey := newKey(t), newKey(t)
	for _, k := range []string{ownerKey, spareKey} {
		if _, err := f.as(f.owner, "sub-owner", Request{Operation: "device_enroll", Workspace: f.workspace, Args: map[string]any{"public_key": k, "label": "owner laptop", "key_store": "keyring"}}); err != nil {
			t.Fatal(err)
		}
	}
	o, spare := keyID(t, ownerKey), keyID(t, spareKey)
	ada := newID("ada") + "@identity-test.example"
	key := newKey(t)
	if _, err := f.join(ada, f.invite(f.owner, ada, "member")["code"].(string), key); err != nil {
		t.Fatal(err)
	}
	a1 := keyID(t, key)
	a2 := f.deviceInvite(ada, a1)
	// The owner's coordinator session, from here on.
	f.must(f.owner, "sub-owner", Request{Operation: "register_session", Workspace: f.workspace, Project: "p", Session: "coord", Args: map[string]any{"instance": "coord-i"}})
	f.exec(`UPDATE projects SET coordinator_session=(SELECT id FROM sessions WHERE name='coord' AND project_id=projects.id) WHERE slug='p' AND workspace_id=$1`, f.workspaceID(f.workspace))

	// Day 60 warns of the owner's own unused device, there and in the
	// owner's coordinator inbox; not yet of a member's.
	clock.at(60*day + time.Minute)
	if err := f.use(o); err != nil {
		t.Fatal(err)
	}
	if got := f.dormancyEvents(f.owner, o, spare, "dormant_soon"); len(got) != 1 || got[0]["due_at"] != layout(clock.base.Add(90*day)) {
		t.Fatal("the owner's spare device:", got)
	}
	if got := f.dormancyEvents(f.owner, o, a2, "dormant_soon"); len(got) != 0 {
		t.Fatal("a member's device warned at day 60:", got)
	}
	notices := f.securityNotices("coord")
	if len(notices) != 1 || !strings.Contains(notices[0], "your device "+spare+" goes dormant") || strings.Contains(notices[0], "laptop") {
		t.Fatal(notices)
	}
	// Day 80 for a member's: shown on the user's other device and the
	// owner's, not in the coordinator inbox; not before.
	clock.at(80*day - time.Minute)
	if err := f.use(o); err != nil {
		t.Fatal(err)
	}
	if got := f.dormancyEvents(f.owner, o, a2, "dormant_soon"); len(got) != 0 {
		t.Fatal("warned before day 80:", got)
	}
	clock.at(80*day + time.Minute)
	for i := 0; i < 2; i++ {
		clock.now = clock.now.Add(2 * time.Minute)
		if err := f.use(o); err != nil {
			t.Fatal(err)
		}
	}
	if got := f.dormancyEvents(ada, a1, a2, "dormant_soon"); len(got) != 1 {
		t.Fatal("the user's other device, once:", got)
	}
	if got := f.dormancyEvents(f.owner, o, a2, "dormant_soon"); len(got) != 1 {
		t.Fatal("the owner's device:", got)
	}
	if got := f.securityNotices("coord"); len(got) != 1 {
		t.Fatal("a member's warning reached the coordinator:", got)
	}
	// The warning doesn't count as the device's use (R2).
	if used, _ := f.deviceUse(a2); used != "" {
		t.Fatal("the warning touched the device:", used)
	}
	if d := f.listed(ada, a1, a2); d["status"] != "active" || d["dormant_soon"] != true || d["dormant_at"] == nil {
		t.Fatal("device list:", d)
	}

	// Day 90: refused, and nothing changes; not a minute before.
	clock.at(90*day - time.Minute)
	if err := f.use(a1); err != nil {
		t.Fatal("dormant before the limit:", err)
	}
	clock.at(90*day + time.Minute)
	var dormant *DormantError
	if err := f.use(a2); !errors.As(err, &dormant) || dormant.Days != 90 || dormant.Device != a2 {
		t.Fatal("a dormant device was let in:", err)
	}
	if used, revoked := f.deviceUse(a2); used != "" || revoked != "" {
		t.Fatal("the refusal changed the device:", used, revoked)
	}
	if n := f.count(`SELECT COUNT(*) FROM device_nonces WHERE device_id=$1`, a2); n != 0 {
		t.Fatal("the refusal kept a nonce:", n)
	}
	if d := f.listed(f.owner, o, a2); d["status"] != "dormant" {
		t.Fatal("device list:", d)
	}
	clock.now = clock.now.Add(2 * time.Minute)
	if err := f.use(a1); err != nil {
		t.Fatal(err)
	}
	if got := f.dormancyEvents(f.owner, o, a2, "dormant"); len(got) != 1 {
		t.Fatal("no dormant event for the owner:", got)
	}

	// Confirming: never by a project owner, nor by the device itself; the
	// main owner names the member; then it works, and its user's other
	// devices hear of it.
	bob := newID("bob") + "@identity-test.example"
	bobKey := newKey(t)
	if _, err := f.join(bob, f.invite(f.owner, bob, "owner")["code"].(string), bobKey); err != nil {
		t.Fatal(err)
	}
	if _, err := f.asDevice(bob, keyID(t, bobKey), Request{Operation: "device_confirm", Workspace: f.workspace, Args: map[string]any{"device_id": a2, "principal": ada}}); err == nil {
		t.Fatal("a project owner confirmed another member's device")
	}
	if _, err := f.asDevice(f.owner, o, Request{Operation: "device_confirm", Workspace: f.workspace, Args: map[string]any{"device_id": a2}}); err == nil {
		t.Fatal("confirmed another member's device without naming them")
	}
	if _, err := f.asDevice(ada, a2, Request{Operation: "device_confirm", Workspace: f.workspace, Args: map[string]any{"device_id": a2}}); err == nil {
		t.Fatal("a device confirmed itself")
	}
	out, err := f.asDevice(f.owner, o, Request{Operation: "device_confirm", Workspace: f.workspace, Args: map[string]any{"device_id": a2, "principal": ada}})
	if err != nil || out.Data.(map[string]any)["was_dormant"] != true || out.Data.(map[string]any)["status"] != "active" {
		t.Fatal(out, err)
	}
	if err = f.use(a2); err != nil {
		t.Fatal("a confirmed device is still refused:", err)
	}
	if f.audited("device_confirmed", a2) != 1 {
		t.Fatal("no audit")
	}
	if got := f.dormancyEvents(ada, a1, a2, "reconfirmed"); len(got) != 1 || got[0]["actor"] != f.owner {
		t.Fatal("the user's other device:", got)
	}
	got := f.securityNotices("coord")
	if !strings.Contains(got[len(got)-1], "device "+a2+" of "+ada+" re-confirmed") || strings.Contains(strings.Join(got, "\n"), "your device "+a2) {
		t.Fatal(got)
	}
	// Its own user confirms from another device, with no --member.
	if _, err = f.asDevice(ada, a1, Request{Operation: "device_confirm", Workspace: f.workspace, Args: map[string]any{"device_id": a2}}); err != nil {
		t.Fatal(err)
	}

	// CI never goes dormant; a human device does at a lower limit.
	ciKey := newKey(t)
	ci, err := f.auth(f.owner, "device_ci_create", map[string]any{"name": "deploy", "grants": map[string]any{"p": "reader"}, "expires_days": 90, "public_key": ciKey})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.asDevice(f.owner, o, Request{Operation: "device_confirm", Workspace: f.workspace, Args: map[string]any{"device_id": ci["device_id"], "principal": f.principalOf(ci["device_id"].(string))}}); err == nil || !strings.Contains(err.Error(), "CI device never goes dormant") {
		t.Fatal("confirmed a CI device:", err)
	}
	set, err := f.asDevice(f.owner, o, Request{Operation: "workspace_settings", Workspace: f.workspace, Args: map[string]any{"device_idle_days": 30}})
	if err != nil || asInt(set.Data.(map[string]any)["device_idle_days"]) != 30 {
		t.Fatal(set, err)
	}
	clock.now = clock.now.Add(31 * day)
	if err = f.use(ci["device_id"].(string)); err != nil {
		t.Fatal("a CI device went dormant:", err)
	}
	if err = f.use(a1); !errors.As(err, &dormant) || dormant.Days != 30 {
		t.Fatal("not dormant at the lower limit:", err)
	}

	// Lowering the limit past devices' idle time says how many go dormant
	// at once, and takes --confirm (R181 L1); raising never asks.
	if _, err = f.asDevice(f.owner, o, Request{Operation: "workspace_settings", Workspace: f.workspace, Args: map[string]any{"device_idle_days": 365}}); err != nil {
		t.Fatal("raising asked:", err)
	}
	if _, err = f.asDevice(f.owner, o, Request{Operation: "workspace_settings", Workspace: f.workspace, Args: map[string]any{"device_idle_days": 30}}); err == nil || !strings.Contains(err.Error(), "would go dormant at once") || !strings.Contains(err.Error(), "--confirm") {
		t.Fatal("lowered without a confirm:", err)
	}
	set, err = f.asDevice(f.owner, o, Request{Operation: "workspace_settings", Workspace: f.workspace, Args: map[string]any{"device_idle_days": 30, "confirm": true}})
	if err != nil || asInt(set.Data.(map[string]any)["device_idle_days"]) != 30 || asInt(set.Data.(map[string]any)["dormant_devices"]) == 0 {
		t.Fatal(set, err)
	}
}

// Confirming is vouched for by a live human device of the person: never a
// Google sign-in, a CI key, or a key the server doesn't know (R181 M1).
func TestPostgresDeviceConfirmNeedsALiveHumanDevice(t *testing.T) {
	f := newPGIdentityFixture(t)
	ownerKey := newKey(t)
	if _, err := f.as(f.owner, "sub-owner", Request{Operation: "device_enroll", Workspace: f.workspace, Args: map[string]any{"public_key": ownerKey, "label": "owner laptop", "key_store": "keyring"}}); err != nil {
		t.Fatal(err)
	}
	o := keyID(t, ownerKey)
	ada := newID("ada") + "@identity-test.example"
	adaKey := newKey(t)
	if _, err := f.join(ada, f.invite(f.owner, ada, "member")["code"].(string), adaKey); err != nil {
		t.Fatal(err)
	}
	a := keyID(t, adaKey)
	confirm := Request{Operation: "device_confirm", Workspace: f.workspace, Args: map[string]any{"device_id": a, "principal": ada}}
	if _, err := f.as(f.owner, "sub-owner", confirm); err == nil || !strings.Contains(err.Error(), "not a sign-in") {
		t.Fatal("a Google sign-in confirmed:", err)
	}
	ci, err := f.auth(f.owner, "device_ci_create", map[string]any{"name": "deploy", "grants": map[string]any{"p": "reader"}, "expires_days": 90, "public_key": newKey(t)})
	if err != nil {
		t.Fatal(err)
	}
	oldKey := newKey(t)
	if _, err = f.as(f.owner, "sub-owner", Request{Operation: "device_enroll", Workspace: f.workspace, Args: map[string]any{"public_key": oldKey, "label": "old laptop", "key_store": "keyring"}}); err != nil {
		t.Fatal(err)
	}
	if _, err = f.auth(f.owner, "device_revoke", map[string]any{"device_id": keyID(t, oldKey)}); err != nil {
		t.Fatal(err)
	}
	for name, signer := range map[string]string{"a CI key": ci["device_id"].(string), "an unknown key": newID("dev_"), "a revoked device": keyID(t, oldKey), "the target itself": a} {
		if _, err = f.asDevice(f.owner, signer, confirm); err == nil {
			t.Fatal(name, "confirmed")
		}
	}
	// Even a CI-kind key holding the person's own principal can't.
	ciKey := newKey(t)
	if _, err = f.as(f.owner, "sub-owner", Request{Operation: "device_enroll", Workspace: f.workspace, Args: map[string]any{"public_key": ciKey, "label": "build box", "key_store": "keyring"}}); err != nil {
		t.Fatal(err)
	}
	if _, err = f.s.db.Exec(`UPDATE devices SET kind='ci' WHERE device_id=$1`, keyID(t, ciKey)); err != nil {
		t.Fatal(err)
	}
	if _, err = f.asDevice(f.owner, keyID(t, ciKey), confirm); err == nil || !strings.Contains(err.Error(), "a CI key can't confirm") {
		t.Fatal("a CI-kind key confirmed:", err)
	}
	if _, err = f.asDevice(f.owner, o, confirm); err != nil {
		t.Fatal("the main owner's own device:", err)
	}
}

// The setting: the main owner sets 30-365 days; members read it.
func TestPostgresDeviceIdleSetting(t *testing.T) {
	f := newPGIdentityFixture(t)
	ada := newID("ada") + "@identity-test.example"
	if _, err := f.join(ada, f.invite(f.owner, ada, "owner")["code"].(string), newKey(t)); err != nil {
		t.Fatal(err)
	}
	got, err := f.auth(ada, "workspace_settings", map[string]any{})
	if err != nil || asInt(got["device_idle_days"]) != 90 || got["dormant_devices"] != nil {
		t.Fatal(got, err)
	}
	if _, err = f.auth(ada, "workspace_settings", map[string]any{"device_idle_days": 120}); err == nil {
		t.Fatal("a project owner changed the setting")
	}
	for _, bad := range []int{29, 366} {
		// Refused by the bus, before the schema's own CHECK.
		if _, err = f.auth(f.owner, "workspace_settings", map[string]any{"device_idle_days": bad}); err == nil || !strings.Contains(err.Error(), "device_idle_days must be 30 to 365") {
			t.Fatal("took", bad, err)
		}
	}
	for _, good := range []int{30, 365} {
		if got, err = f.auth(f.owner, "workspace_settings", map[string]any{"device_idle_days": good}); err != nil || asInt(got["device_idle_days"]) != good || got["dormant_devices"] == nil {
			t.Fatal(good, got, err)
		}
	}
	if f.audited("device_idle_days_set", f.workspaceID(f.workspace)) != 2 {
		t.Fatal("not audited")
	}
}

// R3: with every one of the main owner's devices dormant, the host
// operator's bootstrap invite lets the owner in on a new device, which
// confirms an old one.
func TestPostgresOwnerDevicesAllDormant(t *testing.T) {
	f := newPGIdentityFixture(t)
	clock := newDormancyClock(t)
	owner := newID("boot") + "@identity-test.example"
	ws := newID("bootws")
	out, err := f.s.OperatorBootstrapInvite(context.Background(), ws, owner, "")
	if err != nil {
		t.Fatal(err)
	}
	k1 := newKey(t)
	if _, err = f.join(owner, out["code"].(string), k1); err != nil {
		t.Fatal(err)
	}
	old := keyID(t, k1)
	clock.at(91 * 24 * time.Hour)
	var dormant *DormantError
	if err = f.use(old); !errors.As(err, &dormant) {
		t.Fatal("not dormant:", err)
	}
	again, err := f.s.OperatorBootstrapInvite(context.Background(), ws, owner, "")
	if err != nil {
		t.Fatal(err)
	}
	k2 := newKey(t)
	if _, err = f.join(owner, again["code"].(string), k2); err != nil {
		t.Fatal(err)
	}
	if err = f.use(keyID(t, k2)); err != nil {
		t.Fatal(err)
	}
	if _, err = f.s.ExecuteAsIdentity(context.Background(), Identity{Principal: owner, Issuer: DeviceIssuer, Subject: keyID(t, k2)}, Request{Operation: "device_confirm", Workspace: ws, Args: map[string]any{"device_id": old}}); err != nil {
		t.Fatal(err)
	}
	if err = f.use(old); err != nil {
		t.Fatal("the confirmed device is still refused:", err)
	}
}
