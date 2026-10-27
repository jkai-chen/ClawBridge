package bus

import (
	"context"
	"crypto/ed25519"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"clawbridge/internal/devicekey"
	"clawbridge/internal/principal"
)

// ClawBridge's own sign-in (live/AUTH-INVITE-DESIGN.md): invites, device
// keys, CI devices and the main owner's transfer. The hosted authority
// keeps them; a local store creates the tables for the same schema number
// and never uses them.

// Schema 18. A device belongs to the one workspace that admitted it, and
// signs only there: an invite's issuer sees its code, so a device carries
// no more than its issuer's word, which reaches no further than the
// issuer's workspace. A device's public key is stored in its wire form (unpadded
// base64url), so the same DDL serves SQLite and PostgreSQL.
const authSchemaV18 = `
CREATE TABLE devices(device_id TEXT PRIMARY KEY,workspace_id TEXT NOT NULL REFERENCES workspaces(id),principal TEXT NOT NULL,kind TEXT NOT NULL CHECK(kind IN ('device','ci')),public_key TEXT NOT NULL UNIQUE,label TEXT NOT NULL,key_store TEXT NOT NULL,via TEXT NOT NULL,created_by TEXT NOT NULL,created_at TEXT NOT NULL,expires_at TEXT,last_used_at TEXT,revoked_at TEXT,revoked_by TEXT,revoke_reason TEXT);
CREATE INDEX devices_principal ON devices(workspace_id,principal);
CREATE TABLE invites(invite_id TEXT PRIMARY KEY,workspace_id TEXT NOT NULL REFERENCES workspaces(id),email TEXT NOT NULL,code_sha256 TEXT NOT NULL,purpose TEXT NOT NULL CHECK(purpose IN ('user','device','recovery')),grants TEXT NOT NULL,revoke_others INTEGER NOT NULL DEFAULT 0,created_by TEXT NOT NULL,created_at TEXT NOT NULL,expires_at TEXT NOT NULL,used_at TEXT,used_device TEXT,revoked_at TEXT,revoke_reason TEXT,failed_attempts INTEGER NOT NULL DEFAULT 0);
CREATE INDEX invites_email ON invites(email) WHERE used_at IS NULL AND revoked_at IS NULL;
CREATE TABLE owner_transfers(workspace_id TEXT PRIMARY KEY REFERENCES workspaces(id),from_principal TEXT NOT NULL,to_principal TEXT NOT NULL,created_at TEXT NOT NULL,expires_at TEXT NOT NULL);
CREATE TABLE device_nonces(device_id TEXT NOT NULL,nonce TEXT NOT NULL,expires_at TEXT NOT NULL,PRIMARY KEY(device_id,nonce));
`

// DeviceIssuer is the issuer of an identity proven by a device key's
// signature; its subject is the device ID.
const DeviceIssuer = "clawbridge-device"

const (
	inviteMaxAttempts       = 5
	invitePendingPerSpace   = 20
	invitePendingPerEmail   = 3
	inviteDefaultHours      = 72
	inviteDeviceHours       = 24
	inviteMaxHours          = 168
	ownerTransferHours      = 72
	ciMaxDays               = 90
	maxDeviceLabel          = 64
	NonceLifetime           = 10 * time.Minute
	deviceLastUsedPrecision = time.Minute
)

// ErrInvalidInvite is join's one answer to every failure, so a joiner
// learns nothing about which emails or invites exist. The reason goes to
// the invite's failed attempts and the audit.
var ErrInvalidInvite = &serviceError{code: "forbidden", cause: errors.New("invalid or expired invite"), public: true}

// ErrNonceReused refuses a signed request whose nonce this device used
// within the nonce lifetime.
var ErrNonceReused = &serviceError{code: "forbidden", cause: errors.New("request nonce already used"), public: true}

func refused(message string) error {
	return &serviceError{code: "forbidden", cause: errors.New(message), public: true}
}

func authOperation(op string) bool {
	switch op {
	case "invite_create", "invite_list", "invite_revoke", "device_list", "device_revoke", "device_label", "device_enroll", "device_ci_create", "user_list", "user_revoke", "owner_transfer_offer", "owner_transfer_respond", "owner_transfer_get":
		return true
	}
	return false
}

func future(d time.Duration) string { return hostClock().UTC().Add(d).Format(timestampLayout) }

var roleRank = map[string]int{"reader": 1, "member": 2, "lead": 3, "owner": 4}

// authOp runs a workspace-level sign-in operation for a member the caller
// has already admitted (execute).
func (s *Store) authOp(ctx context.Context, tx *transaction, w map[string]any, r Request) (any, error) {
	a := r.Args
	switch r.Operation {
	case "invite_create":
		return s.createInvite(ctx, tx, w, a)
	case "invite_list":
		return s.listInvites(ctx, tx, w)
	case "invite_revoke":
		return s.revokeInvite(ctx, tx, w, str(a, "invite_id"))
	case "device_list":
		return s.listDevices(ctx, tx, w, str(a, "principal"))
	case "device_revoke":
		return s.revokeDevice(ctx, tx, w, str(a, "device_id"), str(a, "reason"))
	case "device_label":
		return s.labelDevice(ctx, tx, w, str(a, "device_id"), str(a, "label"))
	case "device_enroll":
		return s.enrollDevice(ctx, tx, w, a)
	case "device_ci_create":
		return s.createCIDevice(ctx, tx, w, a)
	case "user_list":
		return s.listUsers(ctx, tx, w)
	case "user_revoke":
		return s.revokeUser(ctx, tx, w, normalPrincipal(str(a, "principal")))
	case "owner_transfer_offer":
		return s.offerOwnerTransfer(ctx, tx, w, normalPrincipal(str(a, "to")))
	case "owner_transfer_respond":
		return s.respondOwnerTransfer(ctx, tx, w, str(a, "action"))
	case "owner_transfer_get":
		return s.ownerTransferView(ctx, tx, w)
	}
	return nil, invalid("unsupported operation")
}

// provenance names how a hosted request's caller was proven.
func (s *Store) provenance() string {
	if s.identity != nil && s.identity.Issuer == DeviceIssuer {
		return "device-key"
	}
	return "google-id-token"
}

func mainOwner(s *Store, w map[string]any) bool { return w["owner_principal"] == s.principal }

// grantsArg reads grants, project (ID or slug) to role, resolved to this
// workspace's project IDs.
func grantsArg(ctx context.Context, tx *transaction, wid string, raw any) (map[string]string, error) {
	out := map[string]string{}
	if raw == nil {
		return out, nil
	}
	m, ok := raw.(map[string]any)
	if !ok {
		return nil, invalid("grants must be an object of project to role")
	}
	for ref, v := range m {
		role, _ := v.(string)
		if roleRank[role] == 0 {
			return nil, invalid("a grant's role must be owner, lead, member or reader")
		}
		p, err := one(ctx, tx, `SELECT id FROM projects WHERE workspace_id=? AND (id=? OR slug=?)`, wid, ref, ref)
		if err != nil {
			return nil, scopedError(err, "grants name a project not in this workspace", "invalid")
		}
		out[p["id"].(string)] = role
	}
	return out, nil
}

// mayGrant is the invite rule (D1): the main owner grants any role; a
// project owner grants up to lead in their own projects; nobody else.
func (s *Store) mayGrant(ctx context.Context, tx *transaction, w map[string]any, grants map[string]string) error {
	if mainOwner(s, w) {
		return nil
	}
	if len(grants) == 0 {
		return refused("only the workspace owner may invite without a project grant")
	}
	for pid, role := range grants {
		mine, err := s.projectRole(ctx, tx, w, pid)
		if err != nil || mine != "owner" {
			return refused("inviting to a project needs its owner")
		}
		if roleRank[role] > roleRank["lead"] {
			return refused("a project owner may invite up to lead; only the workspace owner grants owner")
		}
	}
	return nil
}

// emailAdmitted applies the workspace's domain allow-list to an invited
// email, at invite and again at join (owner's decision (c)).
func emailAdmitted(raw any, email string) bool {
	domains, on, damaged := allowedDomains(raw)
	if !on {
		return true
	}
	if damaged {
		return false
	}
	domain := email[strings.LastIndex(email, "@")+1:]
	for _, d := range domains {
		if d == domain {
			return true
		}
	}
	return false
}

func (s *Store) createInvite(ctx context.Context, tx *transaction, w map[string]any, a map[string]any) (any, error) {
	wid := w["id"].(string)
	email := normalPrincipal(str(a, "email"))
	if strings.HasPrefix(email, principal.CIPrefix) {
		return nil, invalid("a CI principal can't be invited; create a CI device instead")
	}
	if !validPrincipal(email) {
		return nil, invalid("email must be an email address")
	}
	purpose := str(a, "purpose")
	if purpose == "" {
		purpose = "user"
	}
	hours := inviteDefaultHours
	if purpose == "device" {
		hours = inviteDeviceHours
	}
	if v, ok := a["expires_hours"]; ok {
		n, err := integer(v)
		if err != nil || n < 1 || n > inviteMaxHours {
			return nil, invalid("expires_hours must be 1-168")
		}
		hours = n
	}
	grants, err := grantsArg(ctx, tx, wid, a["grants"])
	if err != nil {
		return nil, err
	}
	revokeOthers, _ := a["revoke_others"].(bool)
	switch purpose {
	case "user":
		if len(grants) == 0 {
			return nil, invalid("a user invite needs at least one project grant")
		}
		if err = s.mayGrant(ctx, tx, w, grants); err != nil {
			return nil, err
		}
		member, err := query(ctx, tx, `SELECT principal FROM members WHERE workspace_id=? AND principal=?`, wid, email)
		if err != nil {
			return nil, err
		}
		if len(member) > 0 {
			return nil, invalid("that email is already a member; they add a device with a device invite, or the workspace owner issues a recovery invite")
		}
	case "device":
		// Another device for oneself: no grants, and only one's own email.
		if email != s.principal || len(grants) > 0 {
			return nil, invalid("a device invite is for your own email, with no grants")
		}
	case "recovery":
		if !mainOwner(s, w) {
			return nil, refused("only the workspace owner may issue a recovery invite")
		}
		if len(grants) > 0 {
			return nil, invalid("a recovery invite carries no grants")
		}
		if _, err = one(ctx, tx, `SELECT principal FROM members WHERE workspace_id=? AND principal=?`, wid, email); err != nil {
			return nil, scopedError(err, "a recovery invite is for an existing member", "invalid")
		}
	default:
		return nil, invalid("purpose must be user, device or recovery")
	}
	if revokeOthers && purpose != "recovery" {
		return nil, invalid("revoke_others applies only to a recovery invite")
	}
	if !emailAdmitted(w["allowed_domains"], email) {
		return nil, refused("this workspace's domain allow-list doesn't admit that email")
	}
	now := stamp()
	var space, mine int
	if err = tx.QueryRowContext(ctx, `SELECT COUNT(*),COALESCE(SUM(CASE WHEN email=? THEN 1 ELSE 0 END),0) FROM invites WHERE workspace_id=? AND used_at IS NULL AND revoked_at IS NULL AND expires_at>?`, email, wid, now).Scan(&space, &mine); err != nil {
		return nil, err
	}
	if space >= invitePendingPerSpace {
		return nil, refused("this workspace has 20 pending invites; revoke one first")
	}
	if mine >= invitePendingPerEmail {
		return nil, refused("that email has 3 pending invites; revoke one first")
	}
	id := devicekey.NewInviteID()
	code := devicekey.NewCode()
	normal, _ := devicekey.NormalCode(code)
	expires := future(time.Duration(hours) * time.Hour)
	if _, err = tx.ExecContext(ctx, `INSERT INTO invites(invite_id,workspace_id,email,code_sha256,purpose,grants,revoke_others,created_by,created_at,expires_at) VALUES(?,?,?,?,?,?,?,?,?,?)`, id, wid, email, devicekey.CodeHash(id, normal), purpose, asJSON(grants), boolInt(revokeOthers), s.principal, now, expires); err != nil {
		return nil, err
	}
	shown := map[string]any{"invite_id": id, "email": email, "purpose": purpose, "grants": grants, "expires_at": expires, "revoke_others": revokeOthers}
	if err = s.audit(ctx, tx, wid, "", "", "invite", id, "invite_created", shown); err != nil {
		return nil, err
	}
	out := map[string]any{"code": code}
	for k, v := range shown {
		out[k] = v
	}
	return out, nil
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// inviteStatus names an invite's state; the code is never shown again.
func inviteStatus(row map[string]any, now string) string {
	switch {
	case row["used_at"] != nil:
		return "used"
	case row["revoke_reason"] == "attempts":
		return "burnt"
	case row["revoked_at"] != nil:
		return "revoked"
	case row["expires_at"].(string) <= now:
		return "expired"
	}
	return "pending"
}

func inviteView(row map[string]any, now string) map[string]any {
	var grants map[string]any
	_ = json.Unmarshal([]byte(row["grants"].(string)), &grants)
	out := map[string]any{"status": inviteStatus(row, now), "grants": grants}
	for _, k := range []string{"invite_id", "email", "purpose", "created_by", "created_at", "expires_at", "used_at", "used_device", "revoked_at", "revoke_reason", "failed_attempts"} {
		out[k] = row[k]
	}
	out["revoke_others"] = row["revoke_others"] != int64(0)
	return out
}

// listInvites shows the workspace owner every invite, and anyone else the
// invites they issued.
func (s *Store) listInvites(ctx context.Context, tx *transaction, w map[string]any) (any, error) {
	q := `SELECT * FROM invites WHERE workspace_id=? ORDER BY created_at DESC LIMIT 200`
	args := []any{w["id"]}
	if !mainOwner(s, w) {
		q = `SELECT * FROM invites WHERE workspace_id=? AND created_by=? ORDER BY created_at DESC LIMIT 200`
		args = append(args, s.principal)
	}
	rows, err := query(ctx, tx, q, args...)
	if err != nil {
		return nil, err
	}
	now := stamp()
	out := []any{}
	for _, row := range rows {
		out = append(out, inviteView(row, now))
	}
	return map[string]any{"invites": out}, nil
}

func (s *Store) revokeInvite(ctx context.Context, tx *transaction, w map[string]any, id string) (any, error) {
	wid := w["id"].(string)
	row, err := one(ctx, tx, `SELECT * FROM invites WHERE workspace_id=? AND invite_id=?`, wid, id)
	if err != nil {
		return nil, scopedError(err, "invite not found", "not_found")
	}
	if !mainOwner(s, w) && row["created_by"] != s.principal {
		return nil, refused("only the invite's issuer or the workspace owner may revoke it")
	}
	if status := inviteStatus(row, stamp()); status != "pending" {
		return nil, invalid("the invite is already " + status)
	}
	if _, err = tx.ExecContext(ctx, `UPDATE invites SET revoked_at=?,revoke_reason='revoked' WHERE invite_id=?`, stamp(), id); err != nil {
		return nil, err
	}
	if err = s.audit(ctx, tx, wid, "", "", "invite", id, "invite_revoked", map[string]any{"invite_id": id, "email": row["email"]}); err != nil {
		return nil, err
	}
	return map[string]any{"invite_id": id, "status": "revoked"}, nil
}

// JoinRequest is a join whose proof of key possession the server has
// already checked (§4): the bus binds the key and applies the invite.
type JoinRequest struct {
	Email     string
	Code      string
	PublicKey string
	Label     string
	KeyStore  string
}

// cleanLabel trims a device label; a label is display only.
func cleanLabel(label string) (string, error) {
	label = strings.TrimSpace(label)
	if label == "" || len(label) > maxDeviceLabel || strings.ContainsAny(label, "\r\n\x00") {
		return "", invalid("a device label must be 1-64 characters on one line")
	}
	return label, nil
}

func keyStoreOK(v string) bool { return v == "keyring" || v == "file" || v == "env" }

// Join consumes an invite for its email with the code, and binds the
// device key, in one transaction: single use, bound to the email, burnt
// after five failures, under the workspace's allow-list (§3, §4). Every
// failure is ErrInvalidInvite; the failed attempts it charges are kept.
func (s *Store) Join(ctx context.Context, in JoinRequest) (map[string]any, error) {
	if !s.postgres {
		return nil, errors.New("join is only available on the hosted authority")
	}
	email := normalPrincipal(in.Email)
	code, codeOK := devicekey.NormalCode(in.Code)
	pub, keyErr := devicekey.ParsePublicKey(in.PublicKey)
	label, labelErr := cleanLabel(in.Label)
	if !validPrincipal(email) || !codeOK || keyErr != nil || labelErr != nil || !keyStoreOK(in.KeyStore) || in.KeyStore == "env" {
		return nil, ErrInvalidInvite
	}
	raw, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, errors.New("authority unavailable; no write queued")
	}
	tx := &transaction{Tx: raw, postgres: s.postgres}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `SET LOCAL statement_timeout='12s'`); err != nil {
		return nil, err
	}
	if _, err = tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtextextended(?,0))`, "join:"+email); err != nil {
		return nil, err
	}
	pending, err := query(ctx, tx, `SELECT * FROM invites WHERE email=? AND used_at IS NULL AND revoked_at IS NULL ORDER BY created_at`, email)
	if err != nil {
		return nil, err
	}
	var match map[string]any
	for _, row := range pending {
		if devicekey.CodeMatches(row["code_sha256"].(string), row["invite_id"].(string), code) {
			match = row
		}
	}
	js := *s
	js.principal = email
	fail := func(charged []map[string]any, reason string) (map[string]any, error) {
		for _, row := range charged {
			if err := js.chargeInvite(ctx, tx, row, reason); err != nil {
				return nil, err
			}
		}
		if err := tx.Commit(); err != nil {
			return nil, errors.New("authority commit failed; no write queued")
		}
		return nil, ErrInvalidInvite
	}
	now := stamp()
	if match == nil {
		// A wrong code charges each live invite for the email, so guessing
		// burns them rather than trying forever (AU-L2).
		live := []map[string]any{}
		for _, row := range pending {
			if row["expires_at"].(string) > now {
				live = append(live, row)
			}
		}
		return fail(live, "wrong_code")
	}
	charged := []map[string]any{match}
	if match["expires_at"].(string) <= now {
		return fail(charged, "expired")
	}
	wid := match["workspace_id"].(string)
	if _, err = tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtextextended(?,0))`, "workspace:"+wid); err != nil {
		return nil, err
	}
	w, err := one(ctx, tx, `SELECT * FROM workspaces WHERE id=?`, wid)
	if err != nil {
		return nil, err
	}
	if !emailAdmitted(w["allowed_domains"], email) {
		return fail(charged, "domain_refused")
	}
	deviceID := devicekey.ID(pub)
	var taken int
	if err = tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM devices WHERE device_id=? OR public_key=?`, deviceID, devicekey.EncodePublicKey(pub)).Scan(&taken); err != nil {
		return nil, err
	}
	if taken > 0 {
		return fail(charged, "key_in_use")
	}
	purpose := match["purpose"].(string)
	member, err := query(ctx, tx, `SELECT principal FROM members WHERE workspace_id=? AND principal=?`, wid, email)
	if err != nil {
		return nil, err
	}
	if purpose != "user" && len(member) == 0 {
		return fail(charged, "not_a_member")
	}
	if purpose == "user" && len(member) > 0 {
		// An existing member adds a device with a device invite, or the
		// owner recovers them; a user invite would let its issuer, who
		// knows the code, act as them.
		return fail(charged, "already_member")
	}
	var grants map[string]string
	if err = json.Unmarshal([]byte(match["grants"].(string)), &grants); err != nil {
		return nil, err
	}
	// The issuer's authority is read again at use, so an invite from an
	// owner who has since lost the role grants nothing.
	issuer := *s
	issuer.principal = match["created_by"].(string)
	switch purpose {
	case "user":
		if err = issuer.mayGrant(ctx, tx, w, grants); err != nil {
			return fail(charged, "issuer_lost_authority")
		}
	case "device":
		// Only the person themselves may add a device to their email.
		if issuer.principal != email {
			return fail(charged, "issuer_lost_authority")
		}
	case "recovery":
		// Only whoever owns the workspace now may recover a member, so a
		// recovery invite dies with its issuer's ownership. The host
		// operator's bootstrap invite recovers only the owner themselves.
		if !mainOwner(&issuer, w) && (issuer.principal != OperatorPrincipal || w["owner_principal"] != email) {
			return fail(charged, "issuer_lost_authority")
		}
	}
	res, err := tx.ExecContext(ctx, `UPDATE invites SET used_at=?,used_device=? WHERE invite_id=? AND used_at IS NULL AND revoked_at IS NULL`, now, deviceID, match["invite_id"])
	if err != nil {
		return nil, err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return fail(nil, "")
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO devices(device_id,workspace_id,principal,kind,public_key,label,key_store,via,created_by,created_at) VALUES(?,?,?,'device',?,?,?,?,?,?)`, deviceID, wid, email, devicekey.EncodePublicKey(pub), label, in.KeyStore, match["invite_id"], match["created_by"], now); err != nil {
		return nil, err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO members(workspace_id,principal,added_by,added_at) VALUES(?,?,?,?) ON CONFLICT(workspace_id,principal) DO NOTHING`, wid, email, match["created_by"], now); err != nil {
		return nil, err
	}
	for pid, role := range grants {
		if _, err = tx.ExecContext(ctx, `INSERT INTO project_roles(project_id,principal,role,granted_by,granted_at) VALUES(?,?,?,?,?) ON CONFLICT(project_id,principal) DO UPDATE SET role=excluded.role,granted_by=excluded.granted_by,granted_at=excluded.granted_at`, pid, email, role, match["created_by"], now); err != nil {
			return nil, err
		}
		if err = js.audit(ctx, tx, wid, pid, "", "project_role", email, "set", map[string]any{"role": role, "principal": email, "via": match["invite_id"]}); err != nil {
			return nil, err
		}
	}
	revoked := []string{}
	if purpose == "recovery" && match["revoke_others"] != int64(0) {
		if revoked, err = revokePrincipalDevices(ctx, tx, wid, email, deviceID, match["created_by"].(string), "recovery"); err != nil {
			return nil, err
		}
	}
	if err = js.audit(ctx, tx, wid, "", "", "invite", match["invite_id"].(string), "invite_used", map[string]any{"invite_id": match["invite_id"], "device_id": deviceID, "purpose": purpose, "invited_by": match["created_by"]}); err != nil {
		return nil, err
	}
	if err = js.audit(ctx, tx, wid, "", "", "device", deviceID, "device_added", map[string]any{"device_id": deviceID, "principal": email, "kind": "device", "via": match["invite_id"], "key_store": in.KeyStore, "revoked_devices": revoked}); err != nil {
		return nil, err
	}
	if err = tx.Commit(); err != nil {
		return nil, errors.New("authority commit failed; no write queued")
	}
	return map[string]any{"device_id": deviceID, "principal": email, "workspace": map[string]any{"id": wid, "name": w["name"]}, "purpose": purpose, "grants": grants, "revoked_devices": revoked}, nil
}

// chargeInvite records one failed join against an invite, and burns it at
// the fifth (§3).
func (s *Store) chargeInvite(ctx context.Context, tx *transaction, row map[string]any, reason string) error {
	id := row["invite_id"].(string)
	var attempts int
	if err := tx.QueryRowContext(ctx, `UPDATE invites SET failed_attempts=failed_attempts+1 WHERE invite_id=? RETURNING failed_attempts`, id).Scan(&attempts); err != nil {
		return err
	}
	wid := row["workspace_id"].(string)
	if err := s.audit(ctx, tx, wid, "", "", "invite", id, "invite_join_failed", map[string]any{"invite_id": id, "reason": reason, "failed_attempts": attempts}); err != nil {
		return err
	}
	if attempts < inviteMaxAttempts {
		return nil
	}
	if _, err := tx.ExecContext(ctx, `UPDATE invites SET revoked_at=?,revoke_reason='attempts' WHERE invite_id=? AND revoked_at IS NULL`, stamp(), id); err != nil {
		return err
	}
	return s.audit(ctx, tx, wid, "", "", "invite", id, "invite_burnt", map[string]any{"invite_id": id, "failed_attempts": attempts})
}

// revokePrincipalDevices revokes every live device of principal in the
// workspace except keep, and returns their IDs.
func revokePrincipalDevices(ctx context.Context, tx *transaction, wid, principal, keep, by, reason string) ([]string, error) {
	rows, err := query(ctx, tx, `SELECT device_id FROM devices WHERE workspace_id=? AND principal=? AND device_id<>? AND revoked_at IS NULL ORDER BY device_id`, wid, principal, keep)
	if err != nil {
		return nil, err
	}
	out := []string{}
	for _, row := range rows {
		id := row["device_id"].(string)
		if _, err = tx.ExecContext(ctx, `UPDATE devices SET revoked_at=?,revoked_by=?,revoke_reason=? WHERE device_id=?`, stamp(), by, reason, id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, nil
}

func deviceView(row map[string]any, now string) map[string]any {
	out := map[string]any{}
	for _, k := range []string{"device_id", "principal", "kind", "label", "key_store", "via", "created_by", "created_at", "expires_at", "last_used_at", "revoked_at", "revoked_by", "revoke_reason"} {
		out[k] = row[k]
	}
	status := "active"
	if row["revoked_at"] != nil {
		status = "revoked"
	} else if e, ok := row["expires_at"].(string); ok && e <= now {
		status = "expired"
	}
	out["status"] = status
	return out
}

// listDevices shows a caller their own devices in the workspace and the CI
// devices they created there; the workspace owner may name any member.
func (s *Store) listDevices(ctx context.Context, tx *transaction, w map[string]any, who string) (any, error) {
	who = normalPrincipal(who)
	var rows []map[string]any
	var err error
	switch {
	case who == "" || who == s.principal:
		rows, err = query(ctx, tx, `SELECT * FROM devices WHERE workspace_id=? AND (principal=? OR (kind='ci' AND created_by=?)) ORDER BY created_at`, w["id"], s.principal, s.principal)
	case mainOwner(s, w):
		if _, err = one(ctx, tx, `SELECT principal FROM members WHERE workspace_id=? AND principal=?`, w["id"], who); err != nil {
			return nil, scopedError(err, "not a member of this workspace", "not_found")
		}
		rows, err = query(ctx, tx, `SELECT * FROM devices WHERE workspace_id=? AND principal=? ORDER BY created_at`, w["id"], who)
	default:
		return nil, refused("only the workspace owner may list another member's devices")
	}
	if err != nil {
		return nil, err
	}
	now := stamp()
	out := []any{}
	for _, row := range rows {
		out = append(out, deviceView(row, now))
	}
	return map[string]any{"devices": out}, nil
}

// deviceInWorkspace reads a device of this workspace the caller may manage:
// their own, a CI device they created, or, for the workspace owner, any.
func (s *Store) deviceInWorkspace(ctx context.Context, tx *transaction, w map[string]any, id string, ownerMay bool) (map[string]any, error) {
	row, err := one(ctx, tx, `SELECT * FROM devices WHERE device_id=? AND workspace_id=?`, id, w["id"])
	if err != nil {
		return nil, scopedError(err, "device not found", "not_found")
	}
	if row["principal"] == s.principal || (row["kind"] == "ci" && row["created_by"] == s.principal) || (ownerMay && mainOwner(s, w)) {
		return row, nil
	}
	return nil, &serviceError{code: "not_found", cause: errors.New("device not found"), public: true}
}

func (s *Store) revokeDevice(ctx context.Context, tx *transaction, w map[string]any, id, reason string) (any, error) {
	row, err := s.deviceInWorkspace(ctx, tx, w, id, true)
	if err != nil {
		return nil, err
	}
	if row["revoked_at"] != nil {
		return nil, invalid("the device is already revoked")
	}
	if reason == "" {
		reason = "revoked"
	}
	if len(reason) > 200 {
		return nil, invalid("reason must be at most 200 characters")
	}
	if _, err = tx.ExecContext(ctx, `UPDATE devices SET revoked_at=?,revoked_by=?,revoke_reason=? WHERE device_id=?`, stamp(), s.principal, reason, id); err != nil {
		return nil, err
	}
	if err = s.audit(ctx, tx, w["id"].(string), "", "", "device", id, "device_revoked", map[string]any{"device_id": id, "principal": row["principal"], "reason": reason}); err != nil {
		return nil, err
	}
	return map[string]any{"device_id": id, "status": "revoked"}, nil
}

func (s *Store) labelDevice(ctx context.Context, tx *transaction, w map[string]any, id, label string) (any, error) {
	label, err := cleanLabel(label)
	if err != nil {
		return nil, err
	}
	row, err := s.deviceInWorkspace(ctx, tx, w, id, false)
	if err != nil {
		return nil, err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE devices SET label=? WHERE device_id=?`, label, id); err != nil {
		return nil, err
	}
	row["label"] = label
	return deviceView(row, stamp()), nil
}

// enrollDevice adds a device key for a caller signed in with Google, with no
// invite: the verified Google account is the proof (§10).
func (s *Store) enrollDevice(ctx context.Context, tx *transaction, w map[string]any, a map[string]any) (any, error) {
	if s.identity == nil || s.identity.Issuer != GoogleIssuer {
		return nil, refused("enrolling a device needs a Google sign-in; ask an owner for an invite")
	}
	pub, err := devicekey.ParsePublicKey(str(a, "public_key"))
	if err != nil {
		return nil, invalid(err.Error())
	}
	label, err := cleanLabel(str(a, "label"))
	if err != nil {
		return nil, err
	}
	store := str(a, "key_store")
	if !keyStoreOK(store) || store == "env" {
		return nil, invalid("key_store must be keyring or file")
	}
	return s.addDevice(ctx, tx, w, s.principal, "device", pub, label, store, "google_enroll", "")
}

func (s *Store) addDevice(ctx context.Context, tx *transaction, w map[string]any, owner, kind string, pub ed25519.PublicKey, label, store, via, expires string) (map[string]any, error) {
	id := devicekey.ID(pub)
	var taken int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM devices WHERE device_id=? OR public_key=?`, id, devicekey.EncodePublicKey(pub)).Scan(&taken); err != nil {
		return nil, err
	}
	if taken > 0 {
		return nil, invalid("this key is already registered")
	}
	var exp any
	if expires != "" {
		exp = expires
	}
	now := stamp()
	if _, err := tx.ExecContext(ctx, `INSERT INTO devices(device_id,workspace_id,principal,kind,public_key,label,key_store,via,created_by,created_at,expires_at) VALUES(?,?,?,?,?,?,?,?,?,?,?)`, id, w["id"], owner, kind, devicekey.EncodePublicKey(pub), label, store, via, s.principal, now, exp); err != nil {
		return nil, err
	}
	out := map[string]any{"device_id": id, "principal": owner, "kind": kind, "label": label, "key_store": store, "via": via, "expires_at": exp}
	if err := s.audit(ctx, tx, w["id"].(string), "", "", "device", id, "device_added", out); err != nil {
		return nil, err
	}
	return out, nil
}

// createCIDevice registers a CI device's public key under its own ci:
// principal, with only the roles listed and a required expiry (D4, AU-M2).
// Its private key never reaches the server.
func (s *Store) createCIDevice(ctx context.Context, tx *transaction, w map[string]any, a map[string]any) (any, error) {
	wid := w["id"].(string)
	p := principal.CIPrincipal(str(a, "name"), w["name"].(string))
	if p == "" {
		return nil, invalid("a CI device name must be 1-40 lowercase letters, digits or hyphens")
	}
	days, err := integer(a["expires_days"])
	if err != nil || days < 1 || days > ciMaxDays {
		return nil, invalid("expires_days is required, 1-90")
	}
	grants, err := grantsArg(ctx, tx, wid, a["grants"])
	if err != nil {
		return nil, err
	}
	if len(grants) == 0 {
		return nil, invalid("a CI device needs at least one project grant")
	}
	for _, role := range grants {
		if role != "reader" && role != "member" {
			return nil, invalid("a CI device's role must be reader or member")
		}
	}
	if err = s.mayGrant(ctx, tx, w, grants); err != nil {
		return nil, err
	}
	pub, err := devicekey.ParsePublicKey(str(a, "public_key"))
	if err != nil {
		return nil, invalid(err.Error())
	}
	label := str(a, "label")
	if label == "" {
		label = p
	}
	if label, err = cleanLabel(label); err != nil {
		return nil, err
	}
	var live int
	if err = tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM devices WHERE principal=? AND revoked_at IS NULL AND (expires_at IS NULL OR expires_at>?)`, p, stamp()).Scan(&live); err != nil {
		return nil, err
	}
	if live > 0 {
		return nil, invalid("a live CI device already has that name; revoke it first")
	}
	now := stamp()
	if _, err = tx.ExecContext(ctx, `INSERT INTO members(workspace_id,principal,added_by,added_at) VALUES(?,?,?,?) ON CONFLICT(workspace_id,principal) DO NOTHING`, wid, p, s.principal, now); err != nil {
		return nil, err
	}
	// A reused name holds exactly the roles listed now.
	if _, err = tx.ExecContext(ctx, `DELETE FROM project_roles WHERE principal=? AND project_id IN (SELECT id FROM projects WHERE workspace_id=?)`, p, wid); err != nil {
		return nil, err
	}
	for pid, role := range grants {
		if _, err = tx.ExecContext(ctx, `INSERT INTO project_roles(project_id,principal,role,granted_by,granted_at) VALUES(?,?,?,?,?)`, pid, p, role, s.principal, now); err != nil {
			return nil, err
		}
	}
	out, err := s.addDevice(ctx, tx, w, p, "ci", pub, label, "env", "ci_create", future(time.Duration(days)*24*time.Hour))
	if err != nil {
		return nil, err
	}
	out["grants"] = grants
	return out, nil
}

// listUsers shows the workspace owner every member, who added them, and
// their devices here.
func (s *Store) listUsers(ctx context.Context, tx *transaction, w map[string]any) (any, error) {
	if !mainOwner(s, w) {
		return nil, refused("only the workspace owner may list users")
	}
	members, err := query(ctx, tx, `SELECT principal,added_by,added_at FROM members WHERE workspace_id=? ORDER BY principal`, w["id"])
	if err != nil {
		return nil, err
	}
	now := stamp()
	out := []any{}
	for _, m := range members {
		p := m["principal"].(string)
		roles, err := query(ctx, tx, `SELECT p.slug,pr.role FROM project_roles pr JOIN projects p ON p.id=pr.project_id WHERE p.workspace_id=? AND pr.principal=? ORDER BY p.slug`, w["id"], p)
		if err != nil {
			return nil, err
		}
		devices, err := query(ctx, tx, `SELECT * FROM devices WHERE workspace_id=? AND principal=? ORDER BY created_at`, w["id"], p)
		if err != nil {
			return nil, err
		}
		shown := []any{}
		for _, d := range devices {
			shown = append(shown, deviceView(d, now))
		}
		out = append(out, map[string]any{"principal": p, "invited_by": m["added_by"], "added_at": m["added_at"], "owner": p == w["owner_principal"], "roles": roles, "devices": shown})
	}
	return map[string]any{"users": out}, nil
}

// revokeUser removes a member from the workspace (§6): every project role
// (retiring their sessions), the membership, pending invites, their devices
// here and the CI devices they created here.
func (s *Store) revokeUser(ctx context.Context, tx *transaction, w map[string]any, who string) (any, error) {
	wid := w["id"].(string)
	if !mainOwner(s, w) {
		return nil, refused("only the workspace owner may revoke a user")
	}
	if who == s.principal {
		return nil, invalid("the workspace owner can't revoke themselves; transfer ownership first")
	}
	if _, err := one(ctx, tx, `SELECT principal FROM members WHERE workspace_id=? AND principal=?`, wid, who); err != nil {
		return nil, scopedError(err, "not a member of this workspace", "not_found")
	}
	projects, err := query(ctx, tx, `SELECT pr.project_id FROM project_roles pr JOIN projects p ON p.id=pr.project_id WHERE p.workspace_id=? AND pr.principal=? ORDER BY pr.project_id`, wid, who)
	if err != nil {
		return nil, err
	}
	retired := []any{}
	for _, row := range projects {
		out, err := s.revokeProjectRole(ctx, tx, w, wid, row["project_id"].(string), map[string]any{"principal": who})
		if err != nil {
			return nil, err
		}
		retired = append(retired, out.(map[string]any)["retired_sessions"].([]string))
	}
	if _, err = tx.ExecContext(ctx, `DELETE FROM members WHERE workspace_id=? AND principal=?`, wid, who); err != nil {
		return nil, err
	}
	invites, err := tx.ExecContext(ctx, `UPDATE invites SET revoked_at=?,revoke_reason='user_revoked' WHERE workspace_id=? AND email=? AND used_at IS NULL AND revoked_at IS NULL`, stamp(), wid, who)
	if err != nil {
		return nil, err
	}
	revokedInvites, _ := invites.RowsAffected()
	devices, err := revokePrincipalDevices(ctx, tx, wid, who, "", s.principal, "user_revoked")
	if err != nil {
		return nil, err
	}
	ci, err := query(ctx, tx, `SELECT device_id FROM devices WHERE workspace_id=? AND kind='ci' AND created_by=? AND revoked_at IS NULL ORDER BY device_id`, wid, who)
	if err != nil {
		return nil, err
	}
	for _, row := range ci {
		id := row["device_id"].(string)
		if _, err = tx.ExecContext(ctx, `UPDATE devices SET revoked_at=?,revoked_by=?,revoke_reason='creator_revoked' WHERE device_id=?`, stamp(), s.principal, id); err != nil {
			return nil, err
		}
		devices = append(devices, id)
	}
	result := map[string]any{"principal": who, "retired_sessions": retired, "revoked_devices": devices, "revoked_invites": revokedInvites}
	if err = s.audit(ctx, tx, wid, "", "", "user", who, "user_revoked", result); err != nil {
		return nil, err
	}
	return result, nil
}

// pendingTransfer reads the workspace's offer, dropping (and auditing) one
// that expired unaccepted.
func (s *Store) pendingTransfer(ctx context.Context, tx *transaction, wid string) (map[string]any, error) {
	rows, err := query(ctx, tx, `SELECT * FROM owner_transfers WHERE workspace_id=?`, wid)
	if err != nil || len(rows) == 0 {
		return nil, err
	}
	row := rows[0]
	if row["expires_at"].(string) > stamp() {
		return row, nil
	}
	if _, err = tx.ExecContext(ctx, `DELETE FROM owner_transfers WHERE workspace_id=?`, wid); err != nil {
		return nil, err
	}
	return nil, s.audit(ctx, tx, wid, "", "", "owner_transfer", wid, "owner_transfer_expired", transferView(row))
}

func transferView(row map[string]any) map[string]any {
	return map[string]any{"from": row["from_principal"], "to": row["to_principal"], "created_at": row["created_at"], "expires_at": row["expires_at"]}
}

func (s *Store) offerOwnerTransfer(ctx context.Context, tx *transaction, w map[string]any, to string) (any, error) {
	wid := w["id"].(string)
	if !mainOwner(s, w) {
		return nil, refused("only the workspace owner may offer ownership")
	}
	if !validPrincipal(to) || to == s.principal {
		return nil, invalid("offer ownership to another member's email")
	}
	if _, err := one(ctx, tx, `SELECT principal FROM members WHERE workspace_id=? AND principal=?`, wid, to); err != nil {
		return nil, scopedError(err, "ownership can be offered only to a member", "invalid")
	}
	pending, err := s.pendingTransfer(ctx, tx, wid)
	if err != nil {
		return nil, err
	}
	if pending != nil {
		return nil, invalid("an ownership offer is already pending; cancel it first")
	}
	row := map[string]any{"from_principal": s.principal, "to_principal": to, "created_at": stamp(), "expires_at": future(ownerTransferHours * time.Hour)}
	if _, err = tx.ExecContext(ctx, `INSERT INTO owner_transfers(workspace_id,from_principal,to_principal,created_at,expires_at) VALUES(?,?,?,?,?)`, wid, row["from_principal"], to, row["created_at"], row["expires_at"]); err != nil {
		return nil, err
	}
	view := transferView(row)
	if err = s.audit(ctx, tx, wid, "", "", "owner_transfer", wid, "owner_transfer_offered", view); err != nil {
		return nil, err
	}
	return view, nil
}

// respondOwnerTransfer settles the offer: the recipient accepts or
// declines, the owner cancels. On accept the recipient becomes the main
// owner and the old owner keeps an explicit owner role on every project.
func (s *Store) respondOwnerTransfer(ctx context.Context, tx *transaction, w map[string]any, action string) (any, error) {
	wid := w["id"].(string)
	row, err := s.pendingTransfer(ctx, tx, wid)
	if err != nil {
		return nil, err
	}
	if row == nil {
		return nil, &serviceError{code: "not_found", cause: errors.New("no ownership offer is pending"), public: true}
	}
	from, to := row["from_principal"].(string), row["to_principal"].(string)
	switch action {
	case "accept", "decline":
		if s.principal != to {
			return nil, refused("only the offer's recipient may " + action + " it")
		}
	case "cancel":
		if s.principal != from {
			return nil, refused("only the owner who made the offer may cancel it")
		}
	default:
		return nil, invalid("action must be accept, decline or cancel")
	}
	if _, err = tx.ExecContext(ctx, `DELETE FROM owner_transfers WHERE workspace_id=?`, wid); err != nil {
		return nil, err
	}
	view := transferView(row)
	if action == "accept" {
		if w["owner_principal"] != from {
			return nil, invalid("the workspace owner changed since the offer")
		}
		now := stamp()
		if _, err = tx.ExecContext(ctx, `INSERT INTO project_roles(project_id,principal,role,granted_by,granted_at) SELECT id,?,'owner',?,? FROM projects WHERE workspace_id=? ON CONFLICT(project_id,principal) DO UPDATE SET role='owner',granted_by=excluded.granted_by,granted_at=excluded.granted_at`, from, to, now, wid); err != nil {
			return nil, err
		}
		if _, err = tx.ExecContext(ctx, `UPDATE workspaces SET owner_principal=? WHERE id=? AND owner_principal=?`, to, wid, from); err != nil {
			return nil, err
		}
		// Recovery is the owner's alone, so the old owner's pending
		// recovery invites go with the role.
		res, err := tx.ExecContext(ctx, `UPDATE invites SET revoked_at=?,revoke_reason='owner_transferred' WHERE workspace_id=? AND purpose='recovery' AND used_at IS NULL AND revoked_at IS NULL`, now, wid)
		if err != nil {
			return nil, err
		}
		n, _ := res.RowsAffected()
		view["revoked_recovery_invites"] = n
	}
	verb := map[string]string{"accept": "accepted", "decline": "declined", "cancel": "cancelled"}[action]
	if err = s.audit(ctx, tx, wid, "", "", "owner_transfer", wid, "owner_transfer_"+verb, view); err != nil {
		return nil, err
	}
	view["status"] = verb
	return view, nil
}

// ownerTransferView shows a pending offer to the owner and the recipient.
func (s *Store) ownerTransferView(ctx context.Context, tx *transaction, w map[string]any) (any, error) {
	rows, err := query(ctx, tx, `SELECT * FROM owner_transfers WHERE workspace_id=? AND expires_at>? AND (from_principal=? OR to_principal=?)`, w["id"], stamp(), s.principal, s.principal)
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return map[string]any{"pending": nil}, nil
	}
	return map[string]any{"pending": transferView(rows[0])}, nil
}

// Device is what the server needs to verify a device's signature.
type Device struct {
	ID string
	// WorkspaceID is the one workspace the device signs in.
	WorkspaceID string
	Principal   string
	Kind        string
	PublicKey   ed25519.PublicKey
	// Live is false for a revoked or expired device.
	Live bool
}

// DeviceByID reads a device for signature verification.
func (s *Store) DeviceByID(ctx context.Context, id string) (Device, error) {
	var d Device
	var key string
	var expires, revoked sql.NullString
	err := s.db.QueryRowContext(ctx, (&transaction{postgres: s.postgres}).bind(`SELECT device_id,workspace_id,principal,kind,public_key,expires_at,revoked_at FROM devices WHERE device_id=?`), id).Scan(&d.ID, &d.WorkspaceID, &d.Principal, &d.Kind, &key, &expires, &revoked)
	if errors.Is(err, sql.ErrNoRows) {
		return Device{}, errNotFound
	}
	if err != nil {
		return Device{}, err
	}
	if d.PublicKey, err = devicekey.ParsePublicKey(key); err != nil {
		return Device{}, err
	}
	d.Live = !revoked.Valid && (!expires.Valid || expires.String > stamp())
	return d, nil
}

// UseNonce records a verified request's nonce for its device, refusing one
// already used (ErrNonceReused), and stamps the device's last use at most
// once a minute. The nonce must already be checked (16-64 bytes, AU-L4).
func (s *Store) UseNonce(ctx context.Context, deviceID, nonce string) error {
	raw, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return errors.New("authority unavailable")
	}
	tx := &transaction{Tx: raw, postgres: s.postgres}
	defer tx.Rollback()
	res, err := tx.ExecContext(ctx, `INSERT INTO device_nonces(device_id,nonce,expires_at) VALUES(?,?,?) ON CONFLICT(device_id,nonce) DO NOTHING`, deviceID, nonce, future(NonceLifetime))
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return ErrNonceReused
	}
	now := hostClock().UTC()
	// purge_expired runs only on request, so each insert also sweeps its
	// own device's expired nonces: a device keeps at most its last ten
	// minutes' worth.
	if _, err = tx.ExecContext(ctx, `DELETE FROM device_nonces WHERE device_id=? AND expires_at<=?`, deviceID, now.Format(timestampLayout)); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE devices SET last_used_at=? WHERE device_id=? AND (last_used_at IS NULL OR last_used_at<?)`, now.Format(timestampLayout), deviceID, now.Add(-deviceLastUsedPrecision).Format(timestampLayout)); err != nil {
		return err
	}
	if err = tx.Commit(); err != nil {
		return errors.New("authority commit failed")
	}
	return nil
}

// purgeNonces deletes nonces past their lifetime (purge_expired).
func purgeNonces(ctx context.Context, tx *transaction) (int64, error) {
	res, err := tx.ExecContext(ctx, `DELETE FROM device_nonces WHERE expires_at<=?`, stamp())
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// liveDevice checks, inside a request, that a device identity's device is
// still live and still the principal's.
func liveDevice(ctx context.Context, tx *transaction, id Identity, workspace string) error {
	if workspace == "" {
		return refused("a device key signs only within its workspace")
	}
	var n int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM devices WHERE device_id=? AND principal=? AND revoked_at IS NULL AND (expires_at IS NULL OR expires_at>?)`, id.Subject, id.Principal, stamp()).Scan(&n); err != nil {
		return err
	}
	if n != 1 {
		return refused("this device was revoked or has expired; ask an owner for an invite")
	}
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM devices WHERE device_id=? AND workspace_id=?`, id.Subject, workspace).Scan(&n); err != nil {
		return err
	}
	if n != 1 {
		return refused("this device belongs to another workspace; join this one with its own invite")
	}
	return nil
}

// OperatorBootstrapInvite is the host operator's way in when no device can
// sign in yet: a fresh server with device sign-in only, or an owner who has
// lost every device. It creates the workspace for owner if it doesn't exist
// (as workspace_init would for the bootstrap owner), then issues a 24 h
// recovery invite for owner alone, from operator:local. Its code is shown
// once, to the caller. A caller that made the code itself passes it as code
// (cloud bootstrap): only its salted hash is stored, and it is not returned.
func (s *Store) OperatorBootstrapInvite(ctx context.Context, workspace, owner, code string) (map[string]any, error) {
	if !s.postgres {
		return nil, errors.New("bootstrap-invite needs the hosted authority")
	}
	given := code != ""
	if given {
		if _, ok := devicekey.NormalCode(code); !ok {
			return nil, errors.New("the given code is not an invite code (CBJ- and 16 letters and digits)")
		}
	}
	owner = normalPrincipal(owner)
	if !validPrincipal(owner) {
		return nil, errors.New("the bootstrap owner must be an email address")
	}
	raw, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, errors.New("authority unavailable")
	}
	tx := &transaction{Tx: raw, postgres: true}
	defer tx.Rollback()
	for _, q := range []string{`SET LOCAL statement_timeout='12s'`, `SET LOCAL lock_timeout='5s'`, `SELECT pg_advisory_xact_lock(882712311)`} {
		if _, err = tx.ExecContext(ctx, q); err != nil {
			return nil, errors.New("authority unavailable; nothing changed")
		}
	}
	existing, err := query(ctx, tx, `SELECT * FROM workspaces WHERE name=?`, workspace)
	if err != nil {
		return nil, err
	}
	var w map[string]any
	created := false
	if len(existing) == 1 {
		w = existing[0]
		if w["owner_principal"] != owner || w["mode"] != "server" {
			return nil, errors.New("that workspace's owner is not the bootstrap owner; the owner recovers with their own device, or by ownership transfer")
		}
	} else {
		init := *s
		init.principal, init.bootstrapOwner = owner, owner
		if _, err = init.initWorkspace(ctx, tx, Request{Operation: "workspace_init", Workspace: workspace, Args: map[string]any{"mode": "server"}}); err != nil {
			return nil, err
		}
		if w, err = one(ctx, tx, `SELECT * FROM workspaces WHERE name=?`, workspace); err != nil {
			return nil, err
		}
		created = true
	}
	wid := w["id"].(string)
	if _, err = tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtextextended(?,0))`, "workspace:"+wid); err != nil {
		return nil, errors.New("workspace is busy or the authority is unavailable; nothing changed")
	}
	var mine int
	if err = tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM invites WHERE workspace_id=? AND email=? AND used_at IS NULL AND revoked_at IS NULL AND expires_at>?`, wid, owner, stamp()).Scan(&mine); err != nil {
		return nil, err
	}
	if mine >= invitePendingPerEmail {
		return nil, errors.New("the owner has 3 pending invites; revoke one first")
	}
	id := devicekey.NewInviteID()
	if !given {
		code = devicekey.NewCode()
	}
	normal, _ := devicekey.NormalCode(code)
	expires := future(inviteDeviceHours * time.Hour)
	if _, err = tx.ExecContext(ctx, `INSERT INTO invites(invite_id,workspace_id,email,code_sha256,purpose,grants,revoke_others,created_by,created_at,expires_at) VALUES(?,?,?,?,'recovery','{}',0,?,?,?)`, id, wid, owner, devicekey.CodeHash(id, normal), OperatorPrincipal, stamp(), expires); err != nil {
		return nil, err
	}
	op := *s
	op.principal = OperatorPrincipal
	shown := map[string]any{"invite_id": id, "email": owner, "purpose": "recovery", "expires_at": expires, "workspace": w["name"], "workspace_created": created, "via": "operator bootstrap-invite"}
	if err = op.audit(ctx, tx, wid, "", "", "invite", id, "invite_created", shown); err != nil {
		return nil, err
	}
	if err = tx.Commit(); err != nil {
		return nil, errors.New("authority commit failed")
	}
	out := map[string]any{}
	if !given {
		out["code"] = code
	}
	for k, v := range shown {
		out[k] = v
	}
	return out, nil
}
