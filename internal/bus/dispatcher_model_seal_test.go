package bus

import (
	"bytes"
	"strings"
	"testing"
)

// AG-L1: a rotation re-seals every key under the current seal key with a
// fresh nonce; a key that opens under neither stops it whole, unless the
// operator deletes such keys; the counts, never a key, go to history.
func TestOperatorModelSealRotate(t *testing.T) {
	h := newModelHarness(t)
	h.setup(nil)
	newKey, lost := bytes.Repeat([]byte{9}, 32), bytes.Repeat([]byte{5}, 32)
	stray, _ := NewModelSeal(lost, nil)
	c, n, err := stray.seal([]byte(testModelKey), modelAAD("other@example.com", "anthropic", "anthropic-messages", "https://api.anthropic.com", "claude-test", 1))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = h.s.db.Exec(`INSERT INTO dispatcher_model_keys(owner_principal,state,provider_id,api,base_url,model,ciphertext,nonce,key_fingerprint,config_revision,egress_text_version,egress_confirmed_at,created_by,created_at) VALUES('other@example.com','active','anthropic','anthropic-messages','https://api.anthropic.com','claude-test',?,?,'wxyz',1,'v2','2026-01-01T00:00:00Z','other@example.com','2026-01-01T00:00:00Z')`, c, n); err != nil {
		t.Fatal(err)
	}
	sealed := func() map[string]string {
		out := map[string]string{}
		r, err := h.s.db.Query(`SELECT owner_principal,ciphertext||'/'||nonce FROM dispatcher_model_keys`)
		if err != nil {
			t.Fatal(err)
		}
		defer r.Close()
		for r.Next() {
			var owner, v string
			if err = r.Scan(&owner, &v); err != nil {
				t.Fatal(err)
			}
			out[owner] = v
		}
		return out
	}
	before := sealed()
	rotating, _ := NewModelSeal(newKey, testSealKey)
	// A dry run counts and changes nothing.
	dry, err := h.s.OperatorModelSealRotate(h.ctx, rotating, false, false)
	if err != nil || dry != (SealRotation{Keys: 2, UnderPrevious: 1, Unreadable: 1, NeedPrevious: 1}) {
		t.Fatal(dry, err)
	}
	if after := sealed(); after[h.principal()] != before[h.principal()] || len(after) != 2 {
		t.Fatal("a dry run changed a key")
	}
	if _, err = h.s.OperatorModelSealRotate(h.ctx, rotating, true, false); err == nil || !strings.Contains(err.Error(), "1 stored model keys open under neither") {
		t.Fatal(err)
	}
	if after := sealed(); after[h.principal()] != before[h.principal()] || len(after) != 2 {
		t.Fatal("a refused rotation changed a key")
	}
	r, err := h.s.OperatorModelSealRotate(h.ctx, rotating, true, true)
	if err != nil || r.Resealed != 1 || r.Deleted != 1 || r.NeedPrevious != 0 {
		t.Fatal(r, err)
	}
	if again, err := h.s.OperatorModelSealRotate(h.ctx, rotating, false, false); err != nil || again.UnderCurrent != 1 || again.UnderPrevious != 0 {
		t.Fatal("after the rotation:", again, err)
	}
	after := sealed()
	if len(after) != 1 || after[h.principal()] == before[h.principal()] {
		t.Fatal("not re-sealed:", after)
	}
	// The re-sealed key opens under the new seal key alone.
	only, _ := NewModelSeal(newKey, nil)
	h.s.SetModelSeal(only)
	if _, cfg := h.start(); cfg.Provider.Key != testModelKey {
		t.Fatal("the re-sealed key does not open under the new seal key")
	}
	var details string
	if err = h.s.db.QueryRow(`SELECT details FROM history WHERE entity='dispatcher_model_key' AND action='resealed' AND changed_by_principal=?`, OperatorPrincipal).Scan(&details); err != nil || details != `{"deleted_unreadable":1,"need_previous":0,"resealed":1}` {
		t.Fatal(details, err)
	}
	if leaks(h.dump()) {
		t.Fatal("the key leaked")
	}
}
