package bus

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

// New-device alerts (live/AUTH-INVITE-DESIGN.md §17.3). Every human device
// added to a user, and every one revoked, is an event. It shows on the
// user's other devices and on the main owner's, until each device
// acknowledges it, so a stolen unlocked device can't add or remove a device
// unseen. CI devices are never alerted, and neither is the device an event
// is about, nor the device that caused it.

// Schema 29. Kinds 'reconfirmed', 'dormant_soon' and 'dormant' are the
// dormancy rule's (§17.4; its own schema is 32). label is copied when the
// event is written: a later relabel doesn't rewrite what was shown.
const deviceEventsSchemaV29 = `
CREATE TABLE device_events(event_id TEXT PRIMARY KEY,workspace_id TEXT NOT NULL REFERENCES workspaces(id),principal TEXT NOT NULL,device_id TEXT NOT NULL,kind TEXT NOT NULL CHECK(kind IN ('added','revoked','reconfirmed','dormant_soon','dormant')),label TEXT NOT NULL,route TEXT NOT NULL,actor TEXT NOT NULL,actor_device TEXT,created_at TEXT NOT NULL);
CREATE INDEX device_events_workspace ON device_events(workspace_id,created_at);
CREATE INDEX device_events_principal ON device_events(workspace_id,principal,created_at);
CREATE TABLE device_event_acks(event_id TEXT NOT NULL,device_id TEXT NOT NULL,acked_at TEXT NOT NULL,PRIMARY KEY(event_id,device_id));
`

// deviceEventDays is how long an event is kept and shown.
const deviceEventDays = 90

// maxDeviceAck bounds the event IDs one device_ack names.
const maxDeviceAck = 100

// deviceEvent records an event about device, a device of principal, caused
// by actor (and actorDevice, when a device key signed the request).
func (s *Store) deviceEvent(ctx context.Context, tx *transaction, wid, principal, device, kind, label, route, actor, actorDevice string) error {
	return s.recordDeviceEvent(ctx, tx, newID("dve_"), wid, principal, device, kind, label, route, actor, actorDevice, "")
}

// recordDeviceEvent records an event under id, once: an ID already written
// writes nothing more. due is the date a dormancy event is about. It also
// drops the workspace's events past their lifetime, with their acks, so
// the table needs no sweep of its own.
func (s *Store) recordDeviceEvent(ctx context.Context, tx *transaction, id, wid, principal, device, kind, label, route, actor, actorDevice, due string) error {
	var by, dueAt any
	if actorDevice != "" {
		by = actorDevice
	}
	if due != "" {
		dueAt = due
	}
	now := stamp()
	res, err := tx.ExecContext(ctx, `INSERT INTO device_events(event_id,workspace_id,principal,device_id,kind,label,route,actor,actor_device,created_at,due_at) VALUES(?,?,?,?,?,?,?,?,?,?,?) ON CONFLICT(event_id) DO NOTHING`, id, wid, principal, device, kind, label, route, actor, by, now, dueAt)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return nil
	}
	if err := s.ownerDeviceNotices(ctx, tx, wid, id, principal, device, kind, route, actor, now, due); err != nil {
		return err
	}
	cutoff := hostClock().UTC().Add(-deviceEventDays * 24 * time.Hour).Format(timestampLayout)
	if _, err := tx.ExecContext(ctx, `DELETE FROM device_event_acks WHERE event_id IN (SELECT event_id FROM device_events WHERE workspace_id=? AND created_at<=?)`, wid, cutoff); err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `DELETE FROM device_events WHERE workspace_id=? AND created_at<=?`, wid, cutoff)
	return err
}

// ownerDeviceNotices tells the main owner's coordinator sessions of a
// device event (A1-D4): one security notice in each project of the
// workspace whose coordinator session the main owner registered and is
// active. A session run by anyone else gets nothing. The notice names the
// user, the device's ID, what happened and when, nothing more: never the
// label, which a member chooses and which would reach an AI coordinator
// under the system session's name; its operation ID is the event's, so a
// project gets it once. A dormancy warning goes there only for the main
// owner's own devices (§17.4 R2); a member's shows on the owner's devices.
func (s *Store) ownerDeviceNotices(ctx context.Context, tx *transaction, wid, event, principal, device, kind, route, actor, now, due string) error {
	w, err := one(ctx, tx, `SELECT * FROM workspaces WHERE id=?`, wid)
	if err != nil {
		return err
	}
	if (kind == "dormant_soon" || kind == "dormant") && principal != w["owner_principal"] {
		return nil
	}
	to, err := query(ctx, tx, `SELECT p.id AS project_id,c.id,c.name FROM projects p JOIN sessions c ON c.project_id=p.id AND c.id=p.coordinator_session WHERE p.workspace_id=? AND c.status='active' AND c.role<>'system' AND c.owner_principal=? ORDER BY p.id`, wid, w["owner_principal"])
	if err != nil || len(to) == 0 {
		return err
	}
	var body string
	switch kind {
	case "added":
		body = fmt.Sprintf("Security: new device %s added to %s at %s via %s.", device, principal, now, strings.ReplaceAll(route, "_", " "))
	case "revoked":
		body = fmt.Sprintf("Security: device %s of %s revoked at %s by %s.", device, principal, now, actor)
	case "reconfirmed":
		body = fmt.Sprintf("Security: device %s of %s re-confirmed at %s by %s.", device, principal, now, actor)
	case "dormant_soon":
		body = fmt.Sprintf("Security: your device %s goes dormant at %s unless used or confirmed (clawbridge device confirm %s).", device, due, device)
	case "dormant":
		body = fmt.Sprintf("Security: your device %s went dormant at %s. Confirm it from another device (clawbridge device confirm %s) or revoke it.", device, due, device)
	default:
		body = fmt.Sprintf("Security: device %s of %s: %s at %s.", device, principal, strings.ReplaceAll(kind, "_", " "), now)
	}
	if kind != "dormant_soon" && kind != "dormant" {
		body += " Not expected? clawbridge user list, then device revoke."
	}
	retention := asInt(w["retention_days"])
	if retention < 1 {
		retention = 30
	}
	for _, c := range to {
		pid := str(c, "project_id")
		if err = s.ensureSystemSession(ctx, tx, wid, pid); err != nil {
			return err
		}
		system, err := one(ctx, tx, `SELECT id FROM sessions WHERE project_id=? AND role='system' AND name=?`, pid, SystemSessionName)
		if err != nil {
			return err
		}
		id := newID("msg_")
		if _, err = tx.ExecContext(ctx, `INSERT INTO messages(id,project_id,source_project_id,sender_id,address,thread_id,reply_to,kind,body,created_at,expires_at,created_by_principal,operation_id,payload_hash,coordination) VALUES(?,?,?,?,?,?,NULL,'security',?,?,?,?,?,?,NULL) ON CONFLICT(sender_id,operation_id) DO NOTHING`,
			id, pid, pid, system["id"], str(c, "name"), id, body, now, layout(at(now).AddDate(0, 0, retention)), systemPrincipal, event, digestOf(body)); err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO deliveries(message_id,project_id,recipient_id) SELECT id,project_id,? FROM messages WHERE id=? AND project_id=?`, c["id"], id, pid); err != nil {
			return err
		}
	}
	return nil
}

// actorDevice is the device that signed this request, if one did.
func (s *Store) actorDevice() string {
	if s.identity != nil && s.identity.Issuer == DeviceIssuer {
		return s.identity.Subject
	}
	return ""
}

// revokedDeviceEvents records a revocation event for each of ids, devices
// of principal revoked together (recovery).
func (s *Store) revokedDeviceEvents(ctx context.Context, tx *transaction, wid, principal string, ids []string, route, actor, actorDevice string) error {
	for _, id := range ids {
		row, err := one(ctx, tx, `SELECT kind,label FROM devices WHERE device_id=?`, id)
		if err != nil {
			return err
		}
		if row["kind"] != "device" {
			continue
		}
		if err = s.deviceEvent(ctx, tx, wid, principal, id, "revoked", row["label"].(string), route, actor, actorDevice); err != nil {
			return err
		}
	}
	return nil
}

// visibleDeviceEvents is the events device may see: about its own user's
// other devices, or, on the main owner's devices, any member's; written
// since the device was added and within the lifetime; not about itself nor
// caused by it. A CI device sees none. acked_at is when it acknowledged one.
const visibleDeviceEvents = `SELECT e.event_id,e.principal,e.device_id,e.kind,e.label,e.route,e.actor,e.actor_device,e.created_at,e.due_at,a.acked_at
FROM devices me JOIN workspaces w ON w.id=me.workspace_id JOIN device_events e ON e.workspace_id=me.workspace_id
LEFT JOIN device_event_acks a ON a.event_id=e.event_id AND a.device_id=me.device_id
WHERE me.device_id=? AND me.kind='device' AND me.revoked_at IS NULL
AND e.device_id<>me.device_id AND (e.actor_device IS NULL OR e.actor_device<>me.device_id)
AND e.created_at>=me.created_at AND e.created_at>?
AND (e.principal=me.principal OR w.owner_principal=me.principal)`

func deviceEventCutoff() string {
	return hostClock().UTC().Add(-deviceEventDays * 24 * time.Hour).Format(timestampLayout)
}

// PendingDeviceEvents counts the events device hasn't acknowledged: the
// server puts it on every response a device key signed (§17.3, R7).
func (s *Store) PendingDeviceEvents(ctx context.Context, device string) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, (&transaction{postgres: s.postgres}).bind(`SELECT COUNT(*) FROM (`+visibleDeviceEvents+`) v WHERE v.acked_at IS NULL`), device, deviceEventCutoff()).Scan(&n)
	return n, err
}

// eventsDevice is the calling device: events and their acks are per device.
func (s *Store) eventsDevice() (string, error) {
	if d := s.actorDevice(); d != "" {
		return d, nil
	}
	return "", refused("device events are kept per device; sign in with this machine's device key")
}

func deviceEventView(row map[string]any) map[string]any {
	out := map[string]any{}
	for _, k := range []string{"event_id", "principal", "device_id", "kind", "label", "route", "actor", "actor_device", "created_at"} {
		out[k] = row[k]
	}
	if row["due_at"] != nil {
		out["due_at"] = row["due_at"]
	}
	out["acked"] = row["acked_at"] != nil
	return out
}

// listDeviceEvents shows the calling device its unacknowledged events, or
// with all its acknowledged ones too, newest first.
func (s *Store) listDeviceEvents(ctx context.Context, tx *transaction, all bool) (any, error) {
	device, err := s.eventsDevice()
	if err != nil {
		return nil, err
	}
	q := visibleDeviceEvents
	if !all {
		q += ` AND a.event_id IS NULL`
	}
	rows, err := query(ctx, tx, q+` ORDER BY e.created_at DESC,e.event_id`, device, deviceEventCutoff())
	if err != nil {
		return nil, err
	}
	out := []any{}
	for _, row := range rows {
		out = append(out, deviceEventView(row))
	}
	return map[string]any{"events": out}, nil
}

// ackDeviceEvents acknowledges, for the calling device only, the named
// events or every one it can see. Naming an event it can't see is refused.
func (s *Store) ackDeviceEvents(ctx context.Context, tx *transaction, a map[string]any) (any, error) {
	device, err := s.eventsDevice()
	if err != nil {
		return nil, err
	}
	all, _ := a["all"].(bool)
	raw, _ := a["event_ids"].([]any)
	if list, ok := a["event_ids"].([]string); ok {
		for _, id := range list {
			raw = append(raw, id)
		}
	}
	if all == (len(raw) > 0) {
		return nil, invalid("name the event IDs to acknowledge, or all")
	}
	if len(raw) > maxDeviceAck {
		return nil, invalid("acknowledge at most 100 events at a time")
	}
	rows, err := query(ctx, tx, visibleDeviceEvents+` AND a.event_id IS NULL`, device, deviceEventCutoff())
	if err != nil {
		return nil, err
	}
	pending := map[string]bool{}
	for _, row := range rows {
		pending[row["event_id"].(string)] = true
	}
	ids := []string{}
	if all {
		for _, row := range rows {
			ids = append(ids, row["event_id"].(string))
		}
	} else {
		seen := map[string]bool{}
		for _, v := range raw {
			id, _ := v.(string)
			if !strings.HasPrefix(id, "dve_") {
				return nil, invalid("event IDs start with dve_")
			}
			if seen[id] {
				continue
			}
			seen[id] = true
			if !pending[id] {
				var visible int
				if err = tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM (`+visibleDeviceEvents+` AND e.event_id=?) v`, device, deviceEventCutoff(), id).Scan(&visible); err != nil {
					return nil, err
				}
				if visible == 0 {
					return nil, &serviceError{code: "not_found", cause: errors.New("event not found"), public: true}
				}
				continue
			}
			ids = append(ids, id)
		}
	}
	now := stamp()
	for _, id := range ids {
		if _, err = tx.ExecContext(ctx, `INSERT INTO device_event_acks(event_id,device_id,acked_at) VALUES(?,?,?) ON CONFLICT(event_id,device_id) DO NOTHING`, id, device, now); err != nil {
			return nil, err
		}
	}
	return map[string]any{"acknowledged": len(ids)}, nil
}
