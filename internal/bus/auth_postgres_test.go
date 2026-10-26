package bus

import (
	"context"
	"crypto/ed25519"
	"errors"
	"strings"
	"sync"
	"testing"

	"clawbridge/internal/devicekey"
)

func newKey(t *testing.T) string {
	t.Helper()
	pub, _, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	return devicekey.EncodePublicKey(pub)
}

func keyID(t *testing.T, key string) string {
	t.Helper()
	pub, err := devicekey.ParsePublicKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return devicekey.ID(pub)
}

func (f *pgIdentityFixture) role(principal, role string) {
	f.t.Helper()
	f.must(f.owner, "sub-owner", Request{Operation: "project_role_set", Workspace: f.workspace, Project: "p", Args: map[string]any{"principal": principal, "role": role}})
}

func (f *pgIdentityFixture) auth(principal, op string, a map[string]any) (map[string]any, error) {
	subject := "sub-" + principal
	if principal == f.owner {
		subject = "sub-owner"
	}
	out, err := f.as(principal, subject, Request{Operation: op, Workspace: f.workspace, Args: a})
	if err != nil {
		return nil, err
	}
	return out.Data.(map[string]any), nil
}

func (f *pgIdentityFixture) invite(by, email, role string) map[string]any {
	f.t.Helper()
	out, err := f.auth(by, "invite_create", map[string]any{"email": email, "grants": map[string]any{"p": role}})
	if err != nil {
		f.t.Fatal(err)
	}
	return out
}

func (f *pgIdentityFixture) asDevice(principal, device string, r Request) (Result, error) {
	return f.s.ExecuteAsIdentity(context.Background(), Identity{Principal: principal, Issuer: DeviceIssuer, Subject: device}, r)
}

func (f *pgIdentityFixture) join(email, code, key string) (map[string]any, error) {
	return f.s.Join(context.Background(), JoinRequest{Email: email, Code: code, PublicKey: key, Label: "laptop", KeyStore: "keyring"})
}

func (f *pgIdentityFixture) inviteRow(id string) map[string]any {
	f.t.Helper()
	tx, err := f.s.db.BeginTx(context.Background(), nil)
	if err != nil {
		f.t.Fatal(err)
	}
	defer tx.Rollback()
	row, err := one(context.Background(), &transaction{Tx: tx, postgres: true}, `SELECT * FROM invites WHERE invite_id=?`, id)
	if err != nil {
		f.t.Fatal(err)
	}
	return row
}

func (f *pgIdentityFixture) exec(q string, a ...any) {
	f.t.Helper()
	if _, err := f.s.db.ExecContext(context.Background(), (&transaction{postgres: true}).bind(q), a...); err != nil {
		f.t.Fatal(err)
	}
}

func (f *pgIdentityFixture) audited(action, key string) int {
	f.t.Helper()
	var n int
	if err := f.s.db.QueryRowContext(context.Background(), `SELECT COUNT(*) FROM history WHERE action=$1 AND entity_key=$2`, action, key).Scan(&n); err != nil {
		f.t.Fatal(err)
	}
	return n
}

// Who may invite whom (D1): the workspace owner anyone; a project owner up
// to lead in their project; a lead nobody. The code is shown once and only
// its salted hash is stored.
func TestPostgresInviteRules(t *testing.T) {
	f := newPGIdentityFixture(t)
	lead, sub := newID("lead")+"@identity-test.example", newID("sub")+"@identity-test.example"
	f.role(lead, "lead")
	f.role(sub, "owner")
	ada := newID("ada") + "@identity-test.example"
	out := f.invite(f.owner, ada, "member")
	code, id := out["code"].(string), out["invite_id"].(string)
	normal, _ := devicekey.NormalCode(code)
	row := f.inviteRow(id)
	if row["code_sha256"] != devicekey.CodeHash(id, normal) || strings.Contains(asJSON(row), normal) || f.audited("invite_created", id) != 1 {
		t.Fatal("stored", row)
	}
	var history string
	if err := f.s.db.QueryRow(`SELECT details FROM history WHERE entity_key=$1`, id).Scan(&history); err != nil || strings.Contains(history, normal) {
		t.Fatal("the audit holds the code", history, err)
	}
	if _, err := f.auth(lead, "invite_create", map[string]any{"email": newID("x") + "@identity-test.example", "grants": map[string]any{"p": "member"}}); !refusedWith(err, "owner") {
		t.Fatal("a lead invited:", err)
	}
	f.invite(sub, newID("y")+"@identity-test.example", "lead")
	if _, err := f.auth(sub, "invite_create", map[string]any{"email": newID("z") + "@identity-test.example", "grants": map[string]any{"p": "owner"}}); !refusedWith(err, "up to lead") {
		t.Fatal("a project owner granted owner:", err)
	}
	for _, bad := range []map[string]any{
		{"email": "ci:deploy@" + f.workspace, "grants": map[string]any{"p": "member"}},
		{"email": newID("d") + "@identity-test.example", "purpose": "device"},
		{"email": ada, "purpose": "recovery"},
		{"email": newID("n") + "@identity-test.example"},
	} {
		if _, err := f.auth(lead, "invite_create", bad); err == nil {
			t.Fatal("accepted", bad)
		}
	}
	// At most three pending invites per email.
	f.invite(f.owner, ada, "member")
	f.invite(f.owner, ada, "reader")
	if _, err := f.auth(f.owner, "invite_create", map[string]any{"email": ada, "grants": map[string]any{"p": "member"}}); !refusedWith(err, "3 pending") {
		t.Fatal("a fourth invite:", err)
	}
	all, _ := f.auth(f.owner, "invite_list", nil)
	mine, _ := f.auth(sub, "invite_list", nil)
	if len(all["invites"].([]any)) != 4 || len(mine["invites"].([]any)) != 1 {
		t.Fatal(all, mine)
	}
	if _, err := f.auth(sub, "invite_revoke", map[string]any{"invite_id": id}); !refusedWith(err, "issuer") {
		t.Fatal("someone else's invite revoked:", err)
	}
	f.must(f.owner, "sub-owner", Request{Operation: "invite_revoke", Workspace: f.workspace, Args: map[string]any{"invite_id": id}})
	if f.inviteRow(id)["revoked_at"] == nil || f.audited("invite_revoked", id) != 1 {
		t.Fatal("not revoked")
	}
	// The domain allow-list applies at invite time.
	if _, err := f.asHD(f.owner, "identity-test.example", Request{Operation: "workspace_set_domains", Workspace: f.workspace, Args: map[string]any{"domains": []any{"identity-test.example"}}}); err != nil {
		t.Fatal(err)
	}
	invite := func(email string) error {
		_, err := f.asHD(f.owner, "identity-test.example", Request{Operation: "invite_create", Workspace: f.workspace, Args: map[string]any{"email": email, "grants": map[string]any{"p": "member"}}})
		return err
	}
	if err := invite("bob@other.example"); !refusedWith(err, "allow-list") {
		t.Fatal("an outside domain was invited:", err)
	}
	in := newID("in") + "@identity-test.example"
	if err := invite(in); err != nil {
		t.Fatal(err)
	}
	// ...and again at join: a list narrowed since refuses the invite.
	res, err := f.asHD(f.owner, "identity-test.example", Request{Operation: "invite_create", Workspace: f.workspace, Args: map[string]any{"email": newID("late") + "@identity-test.example", "grants": map[string]any{"p": "member"}}})
	if err != nil {
		t.Fatal(err)
	}
	late := res.Data.(map[string]any)
	f.exec(`UPDATE workspaces SET allowed_domains=? WHERE name=?`, `["elsewhere.example"]`, f.workspace)
	if _, err = f.join(late["email"].(string), late["code"].(string), newKey(t)); err != ErrInvalidInvite {
		t.Fatal("a join outside the allow-list:", err)
	}
}

// A join binds the key and applies the grants once; every failure answers
// the same, and the fifth burns the invite.
func TestPostgresJoin(t *testing.T) {
	f := newPGIdentityFixture(t)
	ada := newID("ada") + "@identity-test.example"
	out := f.invite(f.owner, ada, "member")
	code, id := out["code"].(string), out["invite_id"].(string)
	if _, err := f.join(newID("eve")+"@identity-test.example", code, newKey(t)); err != ErrInvalidInvite {
		t.Fatal("another email joined:", err)
	}
	key := newKey(t)
	joined, err := f.join(strings.ToUpper(ada), strings.ToLower(code), key)
	if err != nil || joined["device_id"] != keyID(t, key) || joined["principal"] != ada {
		t.Fatal(joined, err)
	}
	if f.count(`SELECT COUNT(*) FROM project_roles WHERE principal=$1 AND role='member'`, ada) != 1 || f.inviteRow(id)["used_device"] != keyID(t, key) || f.audited("invite_used", id) != 1 || f.audited("device_added", keyID(t, key)) != 1 {
		t.Fatal("grants or audit missing")
	}
	if _, err = f.join(ada, code, newKey(t)); err != ErrInvalidInvite {
		t.Fatal("a used invite joined again:", err)
	}
	// The device signs in as the email principal.
	if _, err = f.asDevice(ada, keyID(t, key), f.list()); err != nil {
		t.Fatal(err)
	}

	// Expired: refused, and charged to the invite.
	bob := newID("bob") + "@identity-test.example"
	out = f.invite(f.owner, bob, "member")
	f.exec(`UPDATE invites SET expires_at=? WHERE invite_id=?`, "2000-01-01T00:00:00.000000000Z", out["invite_id"])
	if _, err = f.join(bob, out["code"].(string), newKey(t)); err != ErrInvalidInvite || f.inviteRow(out["invite_id"].(string))["failed_attempts"] != int64(1) {
		t.Fatal("expired:", err)
	}

	// Five wrong codes burn it, and the right one then fails too.
	cara := newID("cara") + "@identity-test.example"
	out = f.invite(f.owner, cara, "member")
	cid := out["invite_id"].(string)
	for i := 0; i < 4; i++ {
		if _, err = f.join(cara, devicekey.NewCode(), newKey(t)); err != ErrInvalidInvite {
			t.Fatal(err)
		}
	}
	if f.inviteRow(cid)["revoked_at"] != nil {
		t.Fatal("burnt after four")
	}
	if _, err = f.join(cara, devicekey.NewCode(), newKey(t)); err != ErrInvalidInvite {
		t.Fatal(err)
	}
	if row := f.inviteRow(cid); row["revoke_reason"] != "attempts" || f.audited("invite_burnt", cid) != 1 {
		t.Fatal("not burnt", row)
	}
	if _, err = f.join(cara, out["code"].(string), newKey(t)); err != ErrInvalidInvite {
		t.Fatal("a burnt invite joined:", err)
	}
	// An invite grants only what its issuer may still grant when it's used.
	sub := newID("sub") + "@identity-test.example"
	f.role(sub, "owner")
	dan := newID("dan") + "@identity-test.example"
	danCode := f.invite(sub, dan, "member")["code"].(string)
	f.role(sub, "member")
	if _, err = f.join(dan, danCode, newKey(t)); err != ErrInvalidInvite {
		t.Fatal("a demoted owner's invite granted a role:", err)
	}
	list, _ := f.auth(f.owner, "invite_list", nil)
	for _, v := range list["invites"].([]any) {
		if row := v.(map[string]any); row["invite_id"] == cid && row["status"] != "burnt" {
			t.Fatal(row)
		}
	}
}

// Two joins racing with one code bind exactly one device.
func TestPostgresJoinIsSingleUse(t *testing.T) {
	f := newPGIdentityFixture(t)
	ada := newID("ada") + "@identity-test.example"
	code := f.invite(f.owner, ada, "member")["code"].(string)
	var wg sync.WaitGroup
	errs := make([]error, 6)
	for i := range errs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, errs[i] = f.join(ada, code, newKey(t))
		}(i)
	}
	wg.Wait()
	ok := 0
	for _, err := range errs {
		if err == nil {
			ok++
		} else if err != ErrInvalidInvite {
			t.Fatal(err)
		}
	}
	if ok != 1 || f.count(`SELECT COUNT(*) FROM devices WHERE principal=$1`, ada) != 1 {
		t.Fatal("joins that succeeded:", ok)
	}
}

// A device works until revoked; its owner, or the workspace owner,
// revokes it, and recovery replaces the lost one.
func TestPostgresDevicesAndRecovery(t *testing.T) {
	f := newPGIdentityFixture(t)
	ada := newID("ada") + "@identity-test.example"
	key, other := newKey(t), newKey(t)
	if _, err := f.join(ada, f.invite(f.owner, ada, "member")["code"].(string), key); err != nil {
		t.Fatal(err)
	}
	dev := keyID(t, key)
	r := Request{Operation: "device_list", Workspace: f.workspace, Args: map[string]any{}}
	out, err := f.asDevice(ada, dev, r)
	if err != nil || len(out.Data.(map[string]any)["devices"].([]any)) != 1 {
		t.Fatal(out, err)
	}
	// Another device, by a device invite for one's own email.
	add, err := f.asDevice(ada, dev, Request{Operation: "invite_create", Workspace: f.workspace, Args: map[string]any{"email": ada, "purpose": "device"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.join(ada, add.Data.(map[string]any)["code"].(string), other); err != nil {
		t.Fatal(err)
	}
	if _, err = f.asDevice(ada, dev, Request{Operation: "device_label", Workspace: f.workspace, Args: map[string]any{"device_id": keyID(t, other), "label": "desktop"}}); err != nil {
		t.Fatal(err)
	}
	// A member can't revoke someone else's device; the owner can.
	bob := newID("bob") + "@identity-test.example"
	f.member(bob)
	if _, err = f.auth(bob, "device_revoke", map[string]any{"device_id": dev}); err == nil {
		t.Fatal("a member revoked another's device")
	}
	if _, err = f.auth(f.owner, "device_revoke", map[string]any{"device_id": dev}); err != nil {
		t.Fatal(err)
	}
	if _, err = f.asDevice(ada, dev, f.list()); !refusedWith(err, "revoked") {
		t.Fatal("a revoked device signed in:", err)
	}
	// A device identity must match the device's principal.
	if _, err = f.asDevice(bob, keyID(t, other), f.list()); err == nil {
		t.Fatal("a device signed in as another principal")
	}
	// Recovery with revoke_others replaces every other device.
	rec, err := f.auth(f.owner, "invite_create", map[string]any{"email": ada, "purpose": "recovery", "revoke_others": true})
	if err != nil {
		t.Fatal(err)
	}
	fresh := newKey(t)
	joined, err := f.join(ada, rec["code"].(string), fresh)
	if err != nil || len(joined["revoked_devices"].([]string)) != 1 {
		t.Fatal(joined, err)
	}
	if _, err = f.asDevice(ada, keyID(t, other), f.list()); err == nil {
		t.Fatal("recovery left the other device live")
	}
	if _, err = f.asDevice(ada, keyID(t, fresh), f.list()); err != nil {
		t.Fatal(err)
	}
	// Enrolling needs a Google sign-in, not a device.
	enroll := Request{Operation: "device_enroll", Workspace: f.workspace, Args: map[string]any{"public_key": newKey(t), "label": "work", "key_store": "keyring"}}
	if _, err = f.asDevice(ada, keyID(t, fresh), enroll); !refusedWith(err, "Google") {
		t.Fatal("a device enrolled a device:", err)
	}
	if _, err = f.as(f.owner, "sub-owner", enroll); err != nil {
		t.Fatal(err)
	}
}

// A CI device is a ci: principal (AU-M2), never an email: no invite, role
// grant or Google account can take it, and revoking its creator revokes it.
func TestPostgresCIDevice(t *testing.T) {
	f := newPGIdentityFixture(t)
	sub, lead := newID("sub")+"@identity-test.example", newID("lead")+"@identity-test.example"
	f.role(sub, "owner")
	f.role(lead, "lead")
	key := newKey(t)
	create := map[string]any{"name": "deploy", "grants": map[string]any{"p": "reader"}, "expires_days": 30, "public_key": key}
	if _, err := f.auth(lead, "device_ci_create", create); !refusedWith(err, "owner") {
		t.Fatal("a lead created a CI device:", err)
	}
	for k, v := range map[string]any{"expires_days": 91, "grants": map[string]any{"p": "lead"}, "name": "Deploy"} {
		bad := map[string]any{}
		for kk, vv := range create {
			bad[kk] = vv
		}
		bad[k] = v
		if _, err := f.auth(sub, "device_ci_create", bad); err == nil {
			t.Fatal("accepted", k, v)
		}
	}
	out, err := f.auth(sub, "device_ci_create", create)
	if err != nil {
		t.Fatal(err)
	}
	ci := "ci:deploy@" + strings.ToLower(f.workspace)
	if out["principal"] != ci || out["expires_at"] == nil {
		t.Fatal(out)
	}
	if _, err = f.asDevice(ci, keyID(t, key), f.list()); err != nil {
		t.Fatal(err)
	}
	if _, err = f.asDevice(ci, keyID(t, key), Request{Operation: "register_session", Workspace: f.workspace, Project: "p", Session: "ci", Args: map[string]any{"instance": "ci-i"}}); err == nil {
		t.Fatal("a reader CI device registered a session")
	}
	if _, err = f.s.ExecuteAsIdentity(context.Background(), Identity{Principal: ci, Issuer: GoogleIssuer, Subject: "g"}, f.list()); err == nil {
		t.Fatal("a Google account signed in as a CI principal")
	}
	if _, err = f.as(f.owner, "sub-owner", Request{Operation: "project_role_set", Workspace: f.workspace, Project: "p", Args: map[string]any{"principal": ci, "role": "member"}}); err == nil {
		t.Fatal("a role was granted to a CI principal")
	}
	if _, err = f.auth(f.owner, "invite_create", map[string]any{"email": ci, "grants": map[string]any{"p": "member"}}); err == nil {
		t.Fatal("a CI principal was invited")
	}
	if _, err = f.auth(sub, "device_ci_create", map[string]any{"name": "deploy", "grants": map[string]any{"p": "reader"}, "expires_days": 1, "public_key": newKey(t)}); err == nil {
		t.Fatal("two live CI devices share a name")
	}
	// Revoking the creator revokes their CI devices.
	if _, err = f.auth(f.owner, "user_revoke", map[string]any{"principal": sub}); err != nil {
		t.Fatal(err)
	}
	if _, err = f.asDevice(ci, keyID(t, key), f.list()); err == nil {
		t.Fatal("a revoked creator's CI device signed in")
	}
}

// Revoking a user removes their roles, sessions, membership, devices and
// pending invites; the owner can't revoke themselves.
func TestPostgresUserRevoke(t *testing.T) {
	f := newPGIdentityFixture(t)
	ada := newID("ada") + "@identity-test.example"
	key := newKey(t)
	if _, err := f.join(ada, f.invite(f.owner, ada, "member")["code"].(string), key); err != nil {
		t.Fatal(err)
	}
	f.must(ada, "sub-ada", Request{Operation: "register_session", Workspace: f.workspace, Project: "p", Session: "ada-s", Args: map[string]any{"instance": "ada-i"}})
	pending := f.invite(f.owner, ada, "reader")["invite_id"].(string)
	if _, err := f.auth(f.owner, "user_revoke", map[string]any{"principal": f.owner}); err == nil {
		t.Fatal("the owner revoked themselves")
	}
	out, err := f.auth(f.owner, "user_revoke", map[string]any{"principal": ada})
	if err != nil {
		t.Fatal(err)
	}
	if len(out["revoked_devices"].([]string)) != 1 || out["revoked_invites"] != int64(1) || f.inviteRow(pending)["revoked_at"] == nil || f.audited("user_revoked", ada) != 1 {
		t.Fatal(out)
	}
	if f.count(`SELECT COUNT(*) FROM members WHERE principal=$1`, ada) != 0 || f.count(`SELECT COUNT(*) FROM sessions WHERE owner_principal=$1 AND status='active'`, ada) != 0 {
		t.Fatal("membership or session left")
	}
	if _, err = f.asDevice(ada, keyID(t, key), f.list()); err == nil {
		t.Fatal("a revoked user's device signed in")
	}
}

// The main owner offers, the recipient accepts; nothing changes until
// then, and the old owner keeps an owner role on each project.
func TestPostgresOwnerTransfer(t *testing.T) {
	f := newPGIdentityFixture(t)
	bob, eve := newID("bob")+"@identity-test.example", newID("eve")+"@identity-test.example"
	f.member(bob)
	if _, err := f.auth(f.owner, "owner_transfer_offer", map[string]any{"to": eve}); err == nil {
		t.Fatal("offered to a non-member")
	}
	if _, err := f.auth(bob, "owner_transfer_offer", map[string]any{"to": bob}); !refusedWith(err, "owner") {
		t.Fatal("a member offered ownership:", err)
	}
	if _, err := f.auth(f.owner, "owner_transfer_offer", map[string]any{"to": bob}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.auth(f.owner, "owner_transfer_offer", map[string]any{"to": bob}); err == nil {
		t.Fatal("a second pending offer")
	}
	owner := func() string {
		var p string
		if err := f.s.db.QueryRow(`SELECT owner_principal FROM workspaces WHERE name=$1`, f.workspace).Scan(&p); err != nil {
			t.Fatal(err)
		}
		return p
	}
	if owner() != f.owner {
		t.Fatal("the offer alone moved ownership")
	}
	if _, err := f.auth(f.owner, "owner_transfer_respond", map[string]any{"action": "accept"}); !refusedWith(err, "recipient") {
		t.Fatal("the owner accepted their own offer:", err)
	}
	view, _ := f.auth(bob, "owner_transfer_get", nil)
	if view["pending"] == nil {
		t.Fatal(view)
	}
	// The main owner holds no explicit project role; accepting gives the
	// old owner one on every project.
	f.exec(`DELETE FROM project_roles WHERE principal=?`, f.owner)
	if _, err := f.auth(bob, "owner_transfer_respond", map[string]any{"action": "accept"}); err != nil {
		t.Fatal(err)
	}
	if owner() != bob || f.count(`SELECT COUNT(*) FROM project_roles WHERE principal=$1 AND role='owner'`, f.owner) != 1 {
		t.Fatal("not transferred")
	}
	// Decline, cancel and expiry leave ownership where it is.
	old := f.owner
	if _, err := f.auth(bob, "owner_transfer_offer", map[string]any{"to": old}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.auth(old, "owner_transfer_respond", map[string]any{"action": "decline"}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.auth(bob, "owner_transfer_offer", map[string]any{"to": old}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.auth(bob, "owner_transfer_respond", map[string]any{"action": "cancel"}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.auth(bob, "owner_transfer_offer", map[string]any{"to": old}); err != nil {
		t.Fatal(err)
	}
	f.exec(`UPDATE owner_transfers SET expires_at=? WHERE to_principal=?`, "2000-01-01T00:00:00.000000000Z", old)
	if _, err := f.auth(old, "owner_transfer_respond", map[string]any{"action": "accept"}); err == nil {
		t.Fatal("an expired offer was accepted")
	}
	if owner() != bob {
		t.Fatal("ownership moved")
	}
	var events int
	if err := f.s.db.QueryRow(`SELECT COUNT(*) FROM history WHERE entity='owner_transfer' AND action IN ('owner_transfer_offered','owner_transfer_accepted','owner_transfer_declined','owner_transfer_cancelled','owner_transfer_expired')`).Scan(&events); err != nil || events < 7 {
		t.Fatal("transfer audit", events, err)
	}
}

// A nonce is used once per device; expired ones are swept.
func TestPostgresDeviceNonces(t *testing.T) {
	f := newPGIdentityFixture(t)
	dev := newID("dev_")
	ctx := context.Background()
	if err := f.s.UseNonce(ctx, dev, "n1"); err != nil {
		t.Fatal(err)
	}
	if err := f.s.UseNonce(ctx, dev, "n1"); !errors.Is(err, ErrNonceReused) {
		t.Fatal("a nonce was used twice:", err)
	}
	if err := f.s.UseNonce(ctx, newID("dev_"), "n1"); err != nil {
		t.Fatal("another device's nonce collided:", err)
	}
	f.exec(`UPDATE device_nonces SET expires_at=? WHERE device_id=?`, "2000-01-01T00:00:00.000000000Z", dev)
	if err := f.s.UseNonce(ctx, dev, "n2"); err != nil {
		t.Fatal(err)
	}
	if f.count(`SELECT COUNT(*) FROM device_nonces WHERE device_id=$1`, dev) != 1 {
		t.Fatal("the expired nonce was not swept")
	}
}
