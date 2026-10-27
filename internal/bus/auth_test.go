package bus

import (
	"context"
	"strings"
	"testing"
)

// Local mode creates schema 18's and 29's tables and runs none of sign-in's
// operations: it has no authentication.
func TestAuthSchemaLocalOnlyTables(t *testing.T) {
	h := newHarness(t)
	for _, q := range []string{
		`SELECT device_id,principal,kind,public_key,label,key_store,via,created_by,expires_at,last_used_at,revoked_at,revoked_by,revoke_reason FROM devices LIMIT 0`,
		`SELECT invite_id,workspace_id,email,code_sha256,purpose,grants,revoke_others,expires_at,used_at,used_device,failed_attempts FROM invites LIMIT 0`,
		`SELECT workspace_id,from_principal,to_principal,expires_at FROM owner_transfers LIMIT 0`,
		`SELECT device_id,nonce,expires_at FROM device_nonces LIMIT 0`,
		`SELECT event_id,workspace_id,principal,device_id,kind,label,route,actor,actor_device,created_at FROM device_events LIMIT 0`,
		`SELECT event_id,device_id,acked_at FROM device_event_acks LIMIT 0`,
	} {
		rows, err := h.s.db.Query(q)
		if err != nil {
			t.Fatal(q, err)
		}
		rows.Close()
	}
	for _, op := range []string{"invite_create", "invite_list", "device_list", "user_revoke", "owner_transfer_offer", "device_events", "device_ack"} {
		if msg := h.errorOf(op, "", map[string]any{}); !strings.Contains(msg, "hosted authority") {
			t.Fatal(op, msg)
		}
	}
	// Schema 29's event kinds include the dormancy rule's; nothing else.
	for kind, ok := range map[string]bool{"added": true, "dormant_soon": true, "dormant": true, "reconfirmed": true, "bogus": false} {
		_, err := h.s.db.Exec(`INSERT INTO device_events(event_id,workspace_id,principal,device_id,kind,label,route,actor,created_at) SELECT ?,id,'p','dev_x',?,'l','r','a','t' FROM workspaces LIMIT 1`, "dve_"+kind, kind)
		if (err == nil) != ok {
			t.Fatal(kind, err)
		}
	}
	if _, err := h.s.Join(context.Background(), JoinRequest{}); err == nil {
		t.Fatal("a local store joined")
	}
}
