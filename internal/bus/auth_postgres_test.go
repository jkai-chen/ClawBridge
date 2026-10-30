package bus

import (
	"context"
	"crypto/ed25519"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

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

func (f *pgIdentityFixture) workspaceExists(name string) bool {
	f.t.Helper()
	var n int
	if err := f.s.db.QueryRow(`SELECT COUNT(*) FROM workspaces WHERE name=$1`, name).Scan(&n); err != nil {
		f.t.Fatal(err)
	}
	return n != 0
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
	if _, err = f.auth(f.owner, "invite_create", map[string]any{"email": "ci:other@" + strings.ToLower(f.workspace), "grants": map[string]any{"p": "member"}}); err == nil || !strings.Contains(err.Error(), "CI principal") {
		t.Fatal("a CI principal was invited:", err)
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
	pending, err := f.auth(f.owner, "invite_create", map[string]any{"email": ada, "purpose": "recovery"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.auth(f.owner, "user_revoke", map[string]any{"principal": f.owner}); err == nil {
		t.Fatal("the owner revoked themselves")
	}
	out, err := f.auth(f.owner, "user_revoke", map[string]any{"principal": ada})
	if err != nil {
		t.Fatal(err)
	}
	if len(out["revoked_devices"].([]string)) != 1 || out["revoked_invites"] != int64(1) || f.inviteRow(pending["invite_id"].(string))["revoked_at"] == nil || f.audited("user_revoked", ada) != 1 {
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
	// The old owner's pending recovery invites go with the role; other
	// invites stay.
	cara := newID("cara") + "@identity-test.example"
	f.member(cara)
	recovery, err := f.auth(f.owner, "invite_create", map[string]any{"email": cara, "purpose": "recovery"})
	if err != nil {
		t.Fatal(err)
	}
	user := f.invite(f.owner, newID("dora")+"@identity-test.example", "member")
	accepted, err := f.auth(bob, "owner_transfer_respond", map[string]any{"action": "accept"})
	if err != nil {
		t.Fatal(err)
	}
	if owner() != bob || f.count(`SELECT COUNT(*) FROM project_roles WHERE principal=$1 AND role='owner'`, f.owner) != 1 {
		t.Fatal("not transferred")
	}
	if accepted["revoked_recovery_invites"] != int64(1) || f.inviteRow(recovery["invite_id"].(string))["revoke_reason"] != "owner_transferred" || f.inviteRow(user["invite_id"].(string))["revoked_at"] != nil {
		t.Fatal("recovery invites after the transfer", accepted)
	}
	if _, err = f.join(cara, recovery["code"].(string), newKey(t)); err != ErrInvalidInvite {
		t.Fatal("the old owner's recovery invite joined:", err)
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
	var coded interface{ Code() string }
	if _, err := f.auth(old, "owner_transfer_respond", map[string]any{"action": "accept"}); !errors.As(err, &coded) || coded.Code() != "not_found" || !strings.Contains(err.Error(), "no ownership offer is pending") {
		t.Fatal("an expired offer was accepted:", err)
	}
	if owner() != bob {
		t.Fatal("ownership moved")
	}
	var events int
	if err := f.s.db.QueryRow(`SELECT COUNT(*) FROM history WHERE entity='owner_transfer' AND action IN ('owner_transfer_offered','owner_transfer_accepted','owner_transfer_declined','owner_transfer_cancelled','owner_transfer_expired')`).Scan(&events); err != nil || events < 7 {
		t.Fatal("transfer audit", events, err)
	}
}

// joinedDevices are n devices of a new member, joined with an invite and
// then device invites.
func (f *pgIdentityFixture) joinedDevices(n int) (string, []string) {
	f.t.Helper()
	who := newID("dev") + "@identity-test.example"
	key := newKey(f.t)
	if _, err := f.join(who, f.invite(f.owner, who, "member")["code"].(string), key); err != nil {
		f.t.Fatal(err)
	}
	devices := []string{keyID(f.t, key)}
	for len(devices) < n {
		devices = append(devices, f.deviceInvite(who, devices[0]))
	}
	return who, devices
}

// A nonce is used once per device; expired ones are swept.
func TestPostgresDeviceNonces(t *testing.T) {
	f := newPGIdentityFixture(t)
	who, devices := f.joinedDevices(2)
	dev := devices[0]
	ctx := context.Background()
	if err := f.s.UseNonce(ctx, dev, who, "n1"); err != nil {
		t.Fatal(err)
	}
	if err := f.s.UseNonce(ctx, dev, who, "n1"); !errors.Is(err, ErrNonceReused) {
		t.Fatal("a nonce was used twice:", err)
	}
	if err := f.s.UseNonce(ctx, devices[1], who, "n1"); err != nil {
		t.Fatal("another device's nonce collided:", err)
	}
	f.exec(`UPDATE device_nonces SET expires_at=? WHERE device_id=?`, "2000-01-01T00:00:00.000000000Z", dev)
	if err := f.s.UseNonce(ctx, dev, who, "n2"); err != nil {
		t.Fatal(err)
	}
	if f.count(`SELECT COUNT(*) FROM device_nonces WHERE device_id=$1`, dev) != 1 {
		t.Fatal("the expired nonce was not swept")
	}
}

// UseNonce and a request's own transaction judge a device by one test:
// a live device passes both, and an expired one, a revoked one or one
// signing as another principal fails both. A device that fails writes
// nothing (no nonce, no use stamped), and is told it is revoked before it
// is told it is dormant or that its nonce was used.
func TestPostgresUseNonceRefusesADeviceNoLongerLive(t *testing.T) {
	f := newPGIdentityFixture(t)
	who, devices := f.joinedDevices(3)
	live, expired, revoked := devices[0], devices[1], devices[2]
	ctx := context.Background()
	whoami := Request{Operation: "list_projects", Workspace: f.workspace}
	nonces := func(device string) int {
		return f.count(`SELECT COUNT(*) FROM device_nonces WHERE device_id=$1`, device)
	}
	same := func(device, principal string, want error) {
		t.Helper()
		err := f.s.UseNonce(ctx, device, principal, newID("nonce-"))
		if !errors.Is(err, want) && !(want == nil && err == nil) {
			t.Fatalf("UseNonce(%s): %v, want %v", device, err, want)
		}
		_, err = f.asDevice(principal, device, whoami)
		if (err == nil) != (want == nil) || (err != nil && !strings.Contains(err.Error(), "revoked or has expired")) {
			t.Fatalf("the request's own check on %s: %v, want refused: %v", device, err, want != nil)
		}
	}
	same(live, who, nil)
	if err := f.s.UseNonce(ctx, revoked, who, "replayed"); err != nil {
		t.Fatal(err)
	}
	f.exec(`UPDATE devices SET expires_at=? WHERE device_id=?`, "2000-01-01T00:00:00.000000000Z", expired)
	same(expired, who, ErrDeviceRevoked)
	same(live, f.owner, ErrDeviceRevoked)

	// Revoked, and dormant too, its last use long ago: revoked comes first,
	// a replayed nonce is answered revoked, and nothing is written.
	if _, err := f.asDevice(who, live, Request{Operation: "device_revoke", Workspace: f.workspace, Args: map[string]any{"device_id": revoked}}); err != nil {
		t.Fatal(err)
	}
	f.exec(`UPDATE devices SET created_at=?,last_used_at=? WHERE device_id=?`, "2000-01-01T00:00:00.000000000Z", "2000-01-01T00:00:00.000000000Z", revoked)
	before := nonces(revoked)
	for _, nonce := range []string{"replayed", newID("nonce-")} {
		if err := f.s.UseNonce(ctx, revoked, who, nonce); !errors.Is(err, ErrDeviceRevoked) {
			t.Fatalf("a revoked, dormant device's nonce %q: %v", nonce, err)
		}
	}
	if used, _ := f.deviceUse(revoked); nonces(revoked) != before || used != "2000-01-01T00:00:00.000000000Z" {
		t.Fatalf("a revoked device wrote: %d nonces (was %d), last used %s", nonces(revoked), before, used)
	}
	same(revoked, who, ErrDeviceRevoked)
}

// other makes a second workspace on the same server, owned by f.owner, with
// its own project p.
func (f *pgIdentityFixture) other() string {
	f.t.Helper()
	ws := newID("idws")
	f.must(f.owner, "sub-owner", Request{Operation: "workspace_init", Workspace: ws, Args: map[string]any{"mode": "server"}})
	f.must(f.owner, "sub-owner", Request{Operation: "project_create", Workspace: ws, Args: map[string]any{"slug": "p"}})
	return ws
}

func (f *pgIdentityFixture) workspaceID(name string) string {
	f.t.Helper()
	var id string
	if err := f.s.db.QueryRow(`SELECT id FROM workspaces WHERE name=$1`, name).Scan(&id); err != nil {
		f.t.Fatal(err)
	}
	return id
}

// failReason is the reason the latest failed join of an invite recorded.
func (f *pgIdentityFixture) failReason(id string) string {
	f.t.Helper()
	var reason string
	if err := f.s.db.QueryRow(`SELECT details::jsonb->>'reason' FROM history WHERE action='invite_join_failed' AND entity_key=$1 ORDER BY id DESC LIMIT 1`, id).Scan(&reason); err != nil {
		f.t.Fatal(err)
	}
	return reason
}

// A device signs only in the workspace that admitted it, even for a person
// who is a member of another; every request outside a workspace refuses
// it, and an owner's revocations reach only their own workspace's devices.
func TestPostgresDeviceSignsOnlyInItsWorkspace(t *testing.T) {
	f := newPGIdentityFixture(t)
	b := f.other()
	inB := func(op string, a map[string]any) Request {
		return Request{Operation: op, Workspace: b, Project: "p", Args: a}
	}
	ada := newID("ada") + "@identity-test.example"
	key := newKey(t)
	if _, err := f.join(ada, f.invite(f.owner, ada, "member")["code"].(string), key); err != nil {
		t.Fatal(err)
	}
	dev := keyID(t, key)
	f.must(f.owner, "sub-owner", inB("project_role_set", map[string]any{"principal": ada, "role": "member"}))
	if _, err := f.asDevice(ada, dev, f.list()); err != nil {
		t.Fatal(err)
	}
	if _, err := f.asDevice(ada, dev, Request{Operation: "list_projects", Workspace: b}); !refusedWith(err, "another workspace") {
		t.Fatal("a device signed in to another workspace:", err)
	}

	// resolve_project with no workspace: a Google account sees both p's; a
	// device sees only its own workspace's, so B's projects make it neither
	// ambiguous nor refused.
	resolve := Request{Operation: "resolve_project", Project: "p"}
	if _, err := f.as(ada, "sub-"+ada, resolve); err == nil || !strings.Contains(err.Error(), "ambiguous") {
		t.Fatal("the fixture should be ambiguous for a Google account:", err)
	}
	f.must(f.owner, "sub-owner", Request{Operation: "project_create", Workspace: b, Args: map[string]any{"slug": "q"}})
	f.must(f.owner, "sub-owner", Request{Operation: "project_role_set", Workspace: b, Project: "q", Args: map[string]any{"principal": ada, "role": "member"}})
	f.exec(`UPDATE workspaces SET allowed_domains=? WHERE name=?`, `["elsewhere.example"]`, b)
	out, err := f.asDevice(ada, dev, resolve)
	if err != nil || out.Data.(map[string]any)["workspace_id"] != f.workspaceID(f.workspace) {
		t.Fatal(out, err)
	}
	if _, err = f.asDevice(ada, dev, Request{Operation: "resolve_project", Project: "q"}); err == nil || !strings.Contains(err.Error(), "not found") || refusedWith(err, "domain") {
		t.Fatal("a device resolved, or learned of, another workspace's project:", err)
	}
	f.exec(`UPDATE workspaces SET allowed_domains='[]' WHERE name=?`, b)

	// workspace_init and principal_unbind run outside any workspace, so a
	// device, even the server owner's, never runs them.
	enrolled := f.must(f.owner, "sub-owner", Request{Operation: "device_enroll", Workspace: f.workspace, Args: map[string]any{"public_key": newKey(t), "label": "owner", "key_store": "keyring"}})
	ownerDev := enrolled.Data.(map[string]any)["device_id"].(string)
	if _, err = f.asDevice(f.owner, ownerDev, Request{Operation: "workspace_init", Workspace: newID("idws"), Args: map[string]any{"mode": "server"}}); !refusedWith(err, "within its workspace") {
		t.Fatal("a device made a workspace:", err)
	}
	f.must(ada, "sub-"+ada, f.list())
	if _, err = f.asDevice(f.owner, ownerDev, Request{Operation: "principal_unbind", Args: map[string]any{"principal": ada}}); !refusedWith(err, "within its workspace") {
		t.Fatal("a device unbound a principal:", err)
	}
	if f.count(`SELECT COUNT(*) FROM principal_subjects WHERE principal=$1`, ada) != 1 {
		t.Fatal("the refused unbind unbound")
	}

	// A device invite in B makes a device of B: the join takes the
	// workspace from the invite.
	add := f.must(ada, "sub-"+ada, inB("invite_create", map[string]any{"email": ada, "purpose": "device"}))
	keyB := newKey(t)
	if _, err = f.join(ada, add.Data.(map[string]any)["code"].(string), keyB); err != nil {
		t.Fatal(err)
	}
	devB := keyID(t, keyB)
	if f.count(`SELECT COUNT(*) FROM devices WHERE device_id=$1 AND workspace_id=(SELECT id FROM workspaces WHERE name='`+b+`')`, devB) != 1 {
		t.Fatal("the device isn't B's")
	}
	if _, err = f.asDevice(ada, devB, Request{Operation: "list_projects", Workspace: b}); err != nil {
		t.Fatal(err)
	}
	listed, err := f.asDevice(ada, dev, Request{Operation: "device_list", Workspace: f.workspace, Args: map[string]any{}})
	if err != nil || len(listed.Data.(map[string]any)["devices"].([]any)) != 1 {
		t.Fatal("A's list shows B's device", listed, err)
	}
	users, _ := f.auth(f.owner, "user_list", nil)
	for _, u := range users["users"].([]any) {
		if u := u.(map[string]any); u["principal"] == ada && len(u["devices"].([]any)) != 1 {
			t.Fatal("A's user list shows B's device", u)
		}
	}
	// A's owner manages only A's devices.
	var coded interface{ Code() string }
	if _, err = f.auth(f.owner, "device_revoke", map[string]any{"device_id": devB}); !errors.As(err, &coded) || coded.Code() != "not_found" {
		t.Fatal("A's owner revoked B's device:", err)
	}
	if _, err = f.auth(f.owner, "device_label", map[string]any{"device_id": devB, "label": "x"}); err == nil {
		t.Fatal("A's owner relabelled B's device")
	}

	// Recovery in A replaces A's devices only.
	rec, err := f.auth(f.owner, "invite_create", map[string]any{"email": ada, "purpose": "recovery", "revoke_others": true})
	if err != nil {
		t.Fatal(err)
	}
	fresh := newKey(t)
	joined, err := f.join(ada, rec["code"].(string), fresh)
	if err != nil || len(joined["revoked_devices"].([]string)) != 1 || joined["revoked_devices"].([]string)[0] != dev {
		t.Fatal(joined, err)
	}
	if _, err = f.asDevice(ada, devB, Request{Operation: "list_projects", Workspace: b}); err != nil {
		t.Fatal("recovery in A revoked B's device:", err)
	}

	// CI devices are their workspace's too, and revoking their creator in
	// one workspace revokes only that workspace's.
	sub := newID("sub") + "@identity-test.example"
	f.role(sub, "owner")
	f.must(f.owner, "sub-owner", inB("project_role_set", map[string]any{"principal": sub, "role": "owner"}))
	ciKeyA, ciKeyB := newKey(t), newKey(t)
	ciA, err := f.auth(sub, "device_ci_create", map[string]any{"name": "deploy", "grants": map[string]any{"p": "reader"}, "expires_days": 30, "public_key": ciKeyA})
	if err != nil {
		t.Fatal(err)
	}
	ciB := f.must(sub, "sub-"+sub, inB("device_ci_create", map[string]any{"name": "deploy", "grants": map[string]any{"p": "reader"}, "expires_days": 30, "public_key": ciKeyB})).Data.(map[string]any)
	if _, err = f.asDevice(ciA["principal"].(string), keyID(t, ciKeyA), Request{Operation: "list_projects", Workspace: b}); err == nil {
		t.Fatal("A's CI device signed in to B")
	}

	// user_revoke in A takes A's devices and A's CI devices, and leaves B's.
	gone, err := f.auth(f.owner, "user_revoke", map[string]any{"principal": ada})
	if err != nil || len(gone["revoked_devices"].([]string)) != 1 || gone["revoked_devices"].([]string)[0] != keyID(t, fresh) {
		t.Fatal(gone, err)
	}
	if _, err = f.asDevice(ada, devB, Request{Operation: "list_projects", Workspace: b}); err != nil {
		t.Fatal("revoking the user in A revoked their device in B:", err)
	}
	gone, err = f.auth(f.owner, "user_revoke", map[string]any{"principal": sub})
	if err != nil || len(gone["revoked_devices"].([]string)) != 1 || gone["revoked_devices"].([]string)[0] != keyID(t, ciKeyA) {
		t.Fatal(gone, err)
	}
	if _, err = f.asDevice(ciB["principal"].(string), keyID(t, ciKeyB), Request{Operation: "list_projects", Workspace: b}); err != nil {
		t.Fatal("revoking the creator in A revoked their CI device in B:", err)
	}
}

// An invite's code is seen by its issuer, so it carries only the issuer's
// word: a user invite never names a member (at create or at join), a device
// invite works only for its issuer's own email, a recovery invite only
// while its issuer owns the workspace, and every member records who
// invited them.
func TestPostgresInviteCarriesOnlyItsIssuersWord(t *testing.T) {
	f := newPGIdentityFixture(t)
	ada := newID("ada") + "@identity-test.example"
	f.member(ada)
	if _, err := f.auth(f.owner, "invite_create", map[string]any{"email": ada, "grants": map[string]any{"p": "member"}}); err == nil || !strings.Contains(err.Error(), "already a member") {
		t.Fatal("a user invite named a member:", err)
	}
	carl := newID("carl") + "@identity-test.example"
	late := f.invite(f.owner, carl, "member")
	f.member(carl)
	if _, err := f.join(carl, late["code"].(string), newKey(t)); err != ErrInvalidInvite || f.failReason(late["invite_id"].(string)) != "already_member" {
		t.Fatal("a user invite joined as a member:", err)
	}

	// A device invite whose issuer isn't the email.
	add, err := f.auth(ada, "invite_create", map[string]any{"email": ada, "purpose": "device"})
	if err != nil {
		t.Fatal(err)
	}
	f.exec(`UPDATE invites SET created_by=? WHERE invite_id=?`, f.owner, add["invite_id"])
	if _, err = f.join(ada, add["code"].(string), newKey(t)); err != ErrInvalidInvite || f.failReason(add["invite_id"].(string)) != "issuer_lost_authority" {
		t.Fatal("a device invite from someone else joined:", err)
	}

	// A recovery invite from an owner who no longer owns the workspace.
	rec, err := f.auth(f.owner, "invite_create", map[string]any{"email": ada, "purpose": "recovery"})
	if err != nil {
		t.Fatal(err)
	}
	f.exec(`UPDATE workspaces SET owner_principal=? WHERE name=?`, carl, f.workspace)
	if _, err = f.join(ada, rec["code"].(string), newKey(t)); err != ErrInvalidInvite || f.failReason(rec["invite_id"].(string)) != "issuer_lost_authority" {
		t.Fatal("a former owner's recovery invite joined:", err)
	}
	f.exec(`UPDATE workspaces SET owner_principal=? WHERE name=?`, f.owner, f.workspace)

	// invited_by: in the user list, the project's members and the audit.
	sub := newID("sub") + "@identity-test.example"
	f.role(sub, "owner")
	dan := newID("dan") + "@identity-test.example"
	invite := f.invite(sub, dan, "member")
	if _, err = f.join(dan, invite["code"].(string), newKey(t)); err != nil {
		t.Fatal(err)
	}
	users, _ := f.auth(f.owner, "user_list", nil)
	found := false
	for _, u := range users["users"].([]any) {
		if u := u.(map[string]any); u["principal"] == dan {
			found = u["invited_by"] == sub
		}
	}
	members := f.must(f.owner, "sub-owner", Request{Operation: "list_members", Workspace: f.workspace, Project: "p"}).Data.(map[string]any)["members"].([]map[string]any)
	for _, m := range members {
		if m["principal"] == dan && m["invited_by"] != sub {
			found = false
		}
	}
	var by string
	if err = f.s.db.QueryRow(`SELECT details::jsonb->>'invited_by' FROM history WHERE action='invite_used' AND entity_key=$1`, invite["invite_id"]).Scan(&by); err != nil || by != sub || !found {
		t.Fatal("invited_by", users, members, by, err)
	}
}

// The heartbeat after a device's read re-checks the device: one revoked
// while the read ran skips the beat.
func TestPostgresHeartbeatRechecksTheDevice(t *testing.T) {
	ctx := context.Background()
	t.Cleanup(func() { beforeBeat.Store(nil) })
	for _, revoke := range []bool{false, true} {
		f := newPGIdentityFixture(t)
		ada := newID("ada") + "@identity-test.example"
		key := newKey(t)
		if _, err := f.join(ada, f.invite(f.owner, ada, "member")["code"].(string), key); err != nil {
			t.Fatal(err)
		}
		dev := keyID(t, key)
		reg, err := f.asDevice(ada, dev, Request{Operation: "register_session", Workspace: f.workspace, Project: "p", Session: "hb", Args: map[string]any{}})
		if err != nil {
			t.Fatal(err)
		}
		lastSeen := func() string {
			var at string
			if err := f.s.db.QueryRowContext(ctx, `SELECT last_seen FROM sessions WHERE owner_principal=$1`, ada).Scan(&at); err != nil {
				t.Fatal(err)
			}
			return at
		}
		before := lastSeen()
		hook := func() {
			if revoke {
				if _, err := f.s.db.ExecContext(ctx, `UPDATE devices SET revoked_at='2000-01-01T00:00:00.000000000Z' WHERE device_id=$1`, dev); err != nil {
					t.Error(err)
				}
			}
		}
		beforeBeat.Store(&hook)
		_, err = f.asDevice(ada, dev, Request{Operation: "whoami", Workspace: f.workspace, Project: "p", Session: "hb", Args: map[string]any{"instance": reg.Data.(map[string]any)["instance"]}})
		beforeBeat.Store(nil)
		if err != nil {
			t.Fatal("the read itself was admitted and must succeed", err)
		}
		if moved := lastSeen() != before; moved == revoke {
			t.Fatalf("beat wrote=%v with the device revoked=%v", moved, revoke)
		}
	}
}

// The host operator's bootstrap invite lets the bootstrap owner in with no
// device and no Google: it creates the workspace, and its code joins only
// the workspace's owner.
func TestPostgresOperatorBootstrapInvite(t *testing.T) {
	f := newPGIdentityFixture(t)
	owner := newID("boot") + "@identity-test.example"
	ws := newID("bootws")
	out, err := f.s.OperatorBootstrapInvite(context.Background(), ws, owner, "")
	if err != nil || out["workspace_created"] != true || out["purpose"] != "recovery" {
		t.Fatal(out, err)
	}
	id := out["invite_id"].(string)
	if row := f.inviteRow(id); row["created_by"] != OperatorPrincipal || row["email"] != owner || f.audited("invite_created", id) != 1 {
		t.Fatal(row)
	}
	key := newKey(t)
	if _, err = f.join(owner, out["code"].(string), key); err != nil {
		t.Fatal(err)
	}
	if _, err = f.asDevice(owner, keyID(t, key), Request{Operation: "project_create", Workspace: ws, Args: map[string]any{"slug": "p"}}); err != nil {
		t.Fatal("the owner's device can't act as the owner:", err)
	}
	// Run again for an owner who lost every device: the workspace stays.
	again, err := f.s.OperatorBootstrapInvite(context.Background(), ws, owner, "")
	if err != nil || again["workspace_created"] != false {
		t.Fatal(again, err)
	}
	// It never serves a workspace someone else owns...
	if _, err = f.s.OperatorBootstrapInvite(context.Background(), f.workspace, owner, ""); err == nil {
		t.Fatal("a bootstrap invite for another owner's workspace")
	}
	// ...and its code joins only while its email owns the workspace.
	bob := newID("bob") + "@identity-test.example"
	f.exec(`INSERT INTO members(workspace_id,principal,added_by,added_at) SELECT id,?,?,? FROM workspaces WHERE name=?`, bob, owner, "2026-01-01T00:00:00.000000000Z", ws)
	f.exec(`UPDATE workspaces SET owner_principal=? WHERE name=?`, bob, ws)
	if _, err = f.join(owner, again["code"].(string), newKey(t)); err != ErrInvalidInvite || f.failReason(again["invite_id"].(string)) != "issuer_lost_authority" {
		t.Fatal("an operator invite joined a former owner:", err)
	}
	if _, err = f.s.OperatorBootstrapInvite(context.Background(), ws, "ci:x@"+ws, ""); err == nil {
		t.Fatal("a CI principal as the bootstrap owner")
	}
	// cloud bootstrap makes the code itself: only its hash is stored, and it
	// is never returned; a malformed one is refused before anything exists.
	ws2 := newID("bootws")
	if _, err = f.s.OperatorBootstrapInvite(context.Background(), ws2, owner, "CBJ-not-a-code"); err == nil || f.workspaceExists(ws2) {
		t.Fatal("took a malformed code:", err)
	}
	mine := devicekey.NewCode()
	given, err := f.s.OperatorBootstrapInvite(context.Background(), ws2, owner, strings.ToLower(mine))
	if err != nil || given["code"] != nil || given["workspace_created"] != true {
		t.Fatal(given, err)
	}
	normal, _ := devicekey.NormalCode(mine)
	if row := f.inviteRow(given["invite_id"].(string)); row["code_sha256"] != devicekey.CodeHash(given["invite_id"].(string), normal) {
		t.Fatal("the given code's hash:", row)
	}
	if _, err = f.join(owner, mine, newKey(t)); err != nil {
		t.Fatal("the given code doesn't join:", err)
	}
}

// An invite's grants may name a project by slug, as the TUI's checklist
// does (R183 L3): it's stored by the project's ID; an unknown slug is
// refused.
func TestPostgresInviteGrantsBySlug(t *testing.T) {
	f := newPGIdentityFixture(t)
	email := newID("slug") + "@identity-test.example"
	out := f.invite(f.owner, email, "member")
	var pid string
	if err := f.s.db.QueryRow(`SELECT p.id FROM projects p JOIN workspaces w ON w.id=p.workspace_id WHERE w.name=$1 AND p.slug='p'`, f.workspace).Scan(&pid); err != nil {
		t.Fatal(err)
	}
	var grants string
	if err := f.s.db.QueryRow(`SELECT grants FROM invites WHERE invite_id=$1`, out["invite_id"]).Scan(&grants); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(grants, pid) || strings.Contains(grants, `"p"`) {
		t.Fatal("the grant isn't by project ID:", grants)
	}
	if _, err := f.auth(f.owner, "invite_create", map[string]any{"email": "x" + email, "grants": map[string]any{"no-such-project": "member"}}); err == nil || !strings.Contains(err.Error(), "not in this workspace") {
		t.Fatal("an unknown slug:", err)
	}
}

// IdentityLive holds for a device only while it is live and its
// principal's: a queued write sent later is sent only then (M2).
func TestPostgresIdentityLiveFollowsTheDevice(t *testing.T) {
	ctx := context.Background()
	f := newPGIdentityFixture(t)
	ada := newID("ada") + "@identity-test.example"
	key := newKey(t)
	if _, err := f.join(ada, f.invite(f.owner, ada, "member")["code"].(string), key); err != nil {
		t.Fatal(err)
	}
	dev := keyID(t, key)
	id := Identity{Principal: ada, Issuer: DeviceIssuer, Subject: dev}
	if live, err := f.s.IdentityLive(ctx, id); err != nil || !live {
		t.Fatal("a live device is not live:", live, err)
	}
	if live, _ := f.s.IdentityLive(ctx, Identity{Principal: f.owner, Issuer: DeviceIssuer, Subject: dev}); live {
		t.Fatal("a device is live for another principal")
	}
	if _, err := f.s.db.ExecContext(ctx, `UPDATE devices SET revoked_at='2000-01-01T00:00:00.000000000Z' WHERE device_id=$1`, dev); err != nil {
		t.Fatal(err)
	}
	if live, err := f.s.IdentityLive(ctx, id); err != nil || live {
		t.Fatal("a revoked device is live:", live, err)
	}
}

// A new owner bootstrap invite revokes the workspace's earlier pending ones,
// and only those: another workspace's, a member's and a used one stay.
func TestPostgresBootstrapInviteSupersedesEarlierOwnerCodes(t *testing.T) {
	f := newPGIdentityFixture(t)
	ctx := context.Background()
	owner := newID("boot") + "@identity-test.example"
	ws := newID("bootws")
	first, err := f.s.OperatorBootstrapInvite(ctx, ws, owner, "")
	if err != nil || first["revoked_earlier"] != 0 {
		t.Fatal(first, err)
	}
	// Another workspace with the same owner email keeps its pending code.
	otherWS := newID("bootws")
	elsewhere, err := f.s.OperatorBootstrapInvite(ctx, otherWS, owner, "")
	if err != nil {
		t.Fatal(err)
	}
	// A member invite in the first workspace is not an owner code.
	memberInvite := "inv_member_" + newID("m")
	f.exec(`INSERT INTO invites(invite_id,workspace_id,email,code_sha256,purpose,grants,revoke_others,created_by,created_at,expires_at) SELECT ?,id,?,?,'device','{}',0,?,?,? FROM workspaces WHERE name=?`,
		memberInvite, "member@identity-test.example", "x", owner, stamp(), future(24*time.Hour), ws)
	second, err := f.s.OperatorBootstrapInvite(ctx, ws, owner, "")
	if err != nil || second["revoked_earlier"] != 1 {
		t.Fatal(second, err)
	}
	if row := f.inviteRow(first["invite_id"].(string)); row["revoked_at"] == nil || f.audited("invite_revoked", first["invite_id"].(string)) != 1 {
		t.Fatal("the earlier owner code is still live:", row)
	}
	if _, err = f.join(owner, first["code"].(string), newKey(t)); err == nil {
		t.Fatal("the superseded owner code still joined")
	}
	if row := f.inviteRow(elsewhere["invite_id"].(string)); row["revoked_at"] != nil {
		t.Fatal("another workspace's owner code was revoked")
	}
	if f.inviteRow(memberInvite)["revoked_at"] != nil {
		t.Fatal("a member invite was revoked")
	}
	if _, err = f.join(owner, second["code"].(string), newKey(t)); err != nil {
		t.Fatal("the newest owner code does not join:", err)
	}
}
