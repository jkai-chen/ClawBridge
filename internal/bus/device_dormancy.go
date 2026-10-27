package bus

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"time"
)

// Dormant devices (live/AUTH-INVITE-DESIGN.md §17.4). A human device unused
// for the workspace's idle limit is dormant: its requests are refused until
// another device of its user, or the main owner, confirms it. Nothing it
// holds is deleted. CI devices keep their own expiry and never go dormant.
// Before that, the user's other devices and the main owner's are warned
// (R2): any use resets a device's clock, so a warning on the device itself
// would never be seen.

// Schema 32: the workspace's idle limit, and the date a dormancy event is
// about.
const deviceDormancySchemaV32 = `ALTER TABLE workspaces ADD COLUMN device_idle_days INTEGER NOT NULL DEFAULT 90 CHECK(device_idle_days BETWEEN 30 AND 365);
ALTER TABLE device_events ADD COLUMN due_at TEXT;
`

const (
	defaultDeviceIdleDays = 90
	minDeviceIdleDays     = 30
	maxDeviceIdleDays     = 365
	// The warning starts this many days before a device goes dormant
	// (day 80 of 90), or, for the main owner's own devices, which only the
	// host operator can recover (R3), deviceWarnOwnerDays (day 60). Neither
	// is more than half the limit.
	deviceWarnDays      = 10
	deviceWarnOwnerDays = 30
)

// DormantError refuses a request from a dormant device.
type DormantError struct {
	Device string
	Days   int
}

func (e *DormantError) Error() string {
	return fmt.Sprintf("device_dormant: this device hasn't been used for %d days; confirm it from another of your devices (clawbridge device confirm %s) or ask the workspace owner", e.Days, e.Device)
}

// deviceIdleDays is a workspace's idle limit.
func deviceIdleDays(w map[string]any) int {
	if n := asInt(w["device_idle_days"]); n >= minDeviceIdleDays && n <= maxDeviceIdleDays {
		return n
	}
	return defaultDeviceIdleDays
}

// deviceSeen is when a device was last used, or added if never.
func deviceSeen(row map[string]any) string {
	if v, ok := row["last_used_at"].(string); ok && v != "" {
		return v
	}
	v, _ := row["created_at"].(string)
	return v
}

func days(n int) time.Duration { return time.Duration(n) * 24 * time.Hour }

// deviceDormantAt is when a device last seen at seen goes dormant.
func deviceDormantAt(seen string, idle int) time.Time { return at(seen).Add(days(idle)) }

// deviceWarnAt is when the warning about it starts.
func deviceWarnAt(seen string, idle int, owner bool) time.Time {
	before := deviceWarnDays
	if owner {
		before = deviceWarnOwnerDays
	}
	if before > idle/2 {
		before = idle / 2
	}
	return deviceDormantAt(seen, idle).Add(-days(before))
}

// dormancyView adds when a live human device goes dormant, and marks it
// dormant once it has.
func dormancyView(out, row, w map[string]any, now string) {
	if row["kind"] != "device" || out["status"] != "active" {
		return
	}
	idle := deviceIdleDays(w)
	due := deviceDormantAt(deviceSeen(row), idle)
	out["dormant_at"] = layout(due)
	if !at(now).Before(due) {
		out["status"] = "dormant"
	} else if !at(now).Before(deviceWarnAt(deviceSeen(row), idle, row["principal"] == w["owner_principal"])) {
		out["dormant_soon"] = true
	}
}

// checkDormant refuses a dormant human device, before its use is stamped.
func checkDormant(ctx context.Context, tx *transaction, deviceID string, now time.Time) error {
	rows, err := query(ctx, tx, `SELECT d.kind,d.created_at,d.last_used_at,w.device_idle_days FROM devices d JOIN workspaces w ON w.id=d.workspace_id WHERE d.device_id=?`, deviceID)
	if err != nil || len(rows) != 1 || rows[0]["kind"] != "device" {
		return err
	}
	idle := deviceIdleDays(rows[0])
	if !now.Before(deviceDormantAt(deviceSeen(rows[0]), idle)) {
		return &DormantError{Device: deviceID, Days: idle}
	}
	return nil
}

// dormancyEventID is the one ID of a dormancy event about a device for one
// stretch of disuse, so the event is written once however many requests
// find it due.
func dormancyEventID(device, kind, seen string) string {
	sum := sha256.Sum256([]byte(device + "\x00" + kind + "\x00" + seen))
	return "dve_" + hex.EncodeToString(sum[:16])
}

// warnDormancy writes, for each live human device of the workspace that has
// entered its warning window or gone dormant, that event once. It runs when
// a device's use is stamped, at most once a minute per device in use.
func (s *Store) warnDormancy(ctx context.Context, tx *transaction, wid string, now time.Time) error {
	w, err := one(ctx, tx, `SELECT * FROM workspaces WHERE id=?`, wid)
	if err != nil {
		return err
	}
	idle := deviceIdleDays(w)
	rows, err := query(ctx, tx, `SELECT device_id,principal,label,created_at,last_used_at FROM devices WHERE workspace_id=? AND kind='device' AND revoked_at IS NULL AND (expires_at IS NULL OR expires_at>?) ORDER BY device_id`, wid, layout(now))
	if err != nil {
		return err
	}
	for _, d := range rows {
		seen := deviceSeen(d)
		due := deviceDormantAt(seen, idle)
		kind := "dormant"
		if now.Before(due) {
			kind = "dormant_soon"
			if now.Before(deviceWarnAt(seen, idle, d["principal"] == w["owner_principal"])) {
				continue
			}
		}
		id := str(d, "device_id")
		if err = s.recordDeviceEvent(ctx, tx, dormancyEventID(id, kind, seen), wid, str(d, "principal"), id, kind, str(d, "label"), "idle", systemPrincipal, "", layout(due)); err != nil {
			return err
		}
	}
	return nil
}

// workspaceSettings shows the workspace's settings to a member, and lets
// the main owner change the device idle limit.
func (s *Store) workspaceSettings(ctx context.Context, tx *transaction, w map[string]any, a map[string]any) (any, error) {
	wid := w["id"].(string)
	if v := a["device_idle_days"]; v != nil {
		if !mainOwner(s, w) {
			return nil, refused("only the workspace owner changes its settings")
		}
		n, err := integer(v)
		if err != nil || n < minDeviceIdleDays || n > maxDeviceIdleDays {
			return nil, invalid("device_idle_days must be 30 to 365")
		}
		// A lower limit makes devices dormant at once, with no warning:
		// the owner sees how many first and confirms.
		if lapsing, err := s.lapsingAt(ctx, tx, w, n); err != nil {
			return nil, err
		} else if lapsing > 0 && a["confirm"] != true {
			return nil, invalid(fmt.Sprintf("%d device(s) would go dormant at once, with no warning; rerun with --confirm", lapsing))
		}
		if _, err = tx.ExecContext(ctx, `UPDATE workspaces SET device_idle_days=? WHERE id=?`, n, wid); err != nil {
			return nil, err
		}
		if err = s.audit(ctx, tx, wid, "", "", "workspace", wid, "device_idle_days_set", map[string]any{"device_idle_days": n, "was": deviceIdleDays(w), "by": s.principal}); err != nil {
			return nil, err
		}
		w["device_idle_days"] = n
	}
	out := map[string]any{"workspace_id": wid, "device_idle_days": deviceIdleDays(w)}
	if mainOwner(s, w) {
		// The owner sees what a lower limit does at once.
		rows, err := query(ctx, tx, `SELECT * FROM devices WHERE workspace_id=? AND kind='device'`, wid)
		if err != nil {
			return nil, err
		}
		now, dormant := stamp(), 0
		for _, row := range rows {
			if deviceView(row, w, now)["status"] == "dormant" {
				dormant++
			}
		}
		out["dormant_devices"] = dormant
	}
	return out, nil
}

// confirmDevice re-confirms a human device, dormant or not: from its own
// user (on another device), or by the main owner only (R5, A1-D6). Anyone
// else is told it doesn't exist, as for device revoke. Confirming another
// member's device needs that member named, so a mistyped ID can't wake the
// wrong one.
func (s *Store) confirmDevice(ctx context.Context, tx *transaction, w map[string]any, a map[string]any) (any, error) {
	id := str(a, "device_id")
	row, err := s.deviceInWorkspace(ctx, tx, w, id, true)
	if err != nil {
		return nil, err
	}
	if row["kind"] != "device" {
		return nil, invalid("a CI device never goes dormant; it keeps its own expiry")
	}
	principal := row["principal"].(string)
	if principal != s.principal && normalPrincipal(str(a, "principal")) != principal {
		return nil, invalid("this device is another member's: name them with --member to confirm it")
	}
	now := stamp()
	before := deviceView(row, w, now)
	if before["status"] != "active" && before["status"] != "dormant" {
		return nil, invalid("the device is " + before["status"].(string) + "; it can't be confirmed")
	}
	if id == s.actorDevice() {
		return nil, invalid("a device can't confirm itself; confirm it from another of your devices")
	}
	if err = s.confirmingDevice(ctx, tx); err != nil {
		return nil, err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE devices SET last_used_at=? WHERE device_id=?`, now, id); err != nil {
		return nil, err
	}
	wasDormant := before["status"] == "dormant"
	if err = s.audit(ctx, tx, w["id"].(string), "", "", "device", id, "device_confirmed", map[string]any{"device_id": id, "principal": principal, "was_dormant": wasDormant}); err != nil {
		return nil, err
	}
	if err = s.deviceEvent(ctx, tx, w["id"].(string), principal, id, "reconfirmed", row["label"].(string), "confirm", s.principal, s.actorDevice()); err != nil {
		return nil, err
	}
	row["last_used_at"] = now
	view := deviceView(row, w, now)
	return map[string]any{"device_id": id, "principal": principal, "status": view["status"], "was_dormant": wasDormant, "dormant_at": view["dormant_at"]}, nil
}

// confirmingDevice is the vouching rule: on a server, only a person's own
// human device confirms one, never a Google sign-in or a CI key, so a
// signed-in browser or a CI secret can't wake a lost laptop. That the
// device is live, the caller's, and of this workspace, requireBound has
// checked already (liveDevice). An owner with every device dormant
// recovers by a bootstrap invite (R3). A local store has no identity to
// check.
func (s *Store) confirmingDevice(ctx context.Context, tx *transaction) error {
	if !s.postgres {
		return nil
	}
	signer := s.actorDevice()
	if signer == "" {
		return refused("confirm a device from another of your devices, not a sign-in")
	}
	row, err := one(ctx, tx, `SELECT kind FROM devices WHERE device_id=?`, signer)
	if err != nil {
		return err
	}
	if row["kind"] != "device" {
		return refused("a CI key can't confirm a device; confirm it from another of your devices")
	}
	return nil
}

// lapsingAt counts the human devices a limit of days would make dormant
// that aren't yet.
func (s *Store) lapsingAt(ctx context.Context, tx *transaction, w map[string]any, days int) (int, error) {
	rows, err := query(ctx, tx, `SELECT * FROM devices WHERE workspace_id=? AND kind='device'`, w["id"])
	if err != nil {
		return 0, err
	}
	lower := map[string]any{}
	for k, v := range w {
		lower[k] = v
	}
	lower["device_idle_days"] = days
	now, n := stamp(), 0
	for _, row := range rows {
		if deviceView(row, w, now)["status"] == "active" && deviceView(row, lower, now)["status"] == "dormant" {
			n++
		}
	}
	return n, nil
}
