package bus

import (
	"context"
	"errors"
	"fmt"
)

// SealRotation is what a seal-key rotation found and did: the stored keys
// under the current seal key, under the previous one, and under neither;
// what it re-sealed and deleted; and how many still need the previous key
// afterwards (zero once it is safe to drop it).
type SealRotation struct {
	Keys          int `json:"keys"`
	UnderCurrent  int `json:"under_current"`
	UnderPrevious int `json:"under_previous"`
	Unreadable    int `json:"unreadable"`
	Resealed      int `json:"resealed"`
	Deleted       int `json:"deleted_unreadable"`
	NeedPrevious  int `json:"need_previous"`
}

// OperatorModelSealRotate re-seals every stored provider key under the
// current seal key, each with a fresh nonce, in one transaction [AG-L1].
// Without confirm it only counts. A key opens under the current or the
// previous seal key, as the server opens it. If any key opens under
// neither, nothing changes unless deleteUnreadable: then those keys are
// deleted and their owners' credentials revoked, and each owner runs setup
// again. A key that changed meanwhile (a new setup, sealed by the server
// under the current key) is left as it is. A confirmed rotation's counts
// go to each workspace's history; no key, nonce or owner is recorded.
func (s *Store) OperatorModelSealRotate(ctx context.Context, seal *ModelSeal, confirm, deleteUnreadable bool) (SealRotation, error) {
	var out SealRotation
	if seal == nil {
		return out, errors.New("the seal keys are required")
	}
	raw, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return out, errors.New("authority unavailable")
	}
	tx := &transaction{Tx: raw, postgres: s.postgres}
	defer tx.Rollback()
	nowRaw, err := dbStamp(ctx, tx)
	if err != nil {
		return out, err
	}
	lock := ""
	if s.postgres && confirm {
		lock = " FOR UPDATE"
	}
	keys, err := query(ctx, tx, `SELECT owner_principal,state,provider_id,api,base_url,model,config_revision,ciphertext,nonce FROM dispatcher_model_keys ORDER BY owner_principal,state`+lock)
	if err != nil {
		return out, errors.New("authority unavailable")
	}
	current := &ModelSeal{current: seal.current}
	type opened struct {
		row map[string]any
		key []byte
	}
	readable, unreadable := []opened{}, []map[string]any{}
	// Every opened key is wiped on every way out [#123 L1].
	defer func() {
		for _, o := range readable {
			clear(o.key)
		}
	}()
	aadOf := func(k map[string]any) []byte {
		return modelAAD(str(k, "owner_principal"), str(k, "provider_id"), str(k, "api"), str(k, "base_url"), str(k, "model"), asInt(k["config_revision"]))
	}
	out.Keys = len(keys)
	for _, k := range keys {
		if key, e := current.open(str(k, "ciphertext"), str(k, "nonce"), aadOf(k)); e == nil {
			out.UnderCurrent++
			readable = append(readable, opened{k, key})
			continue
		}
		key, e := seal.open(str(k, "ciphertext"), str(k, "nonce"), aadOf(k))
		if e != nil {
			out.Unreadable++
			unreadable = append(unreadable, k)
			continue
		}
		out.UnderPrevious++
		readable = append(readable, opened{k, key})
	}
	out.NeedPrevious = out.UnderPrevious
	if !confirm {
		return out, nil
	}
	if len(unreadable) > 0 && !deleteUnreadable {
		return out, fmt.Errorf("%d stored model keys open under neither seal key, so nothing was re-sealed: set the key they were sealed with as CLAWBRIDGE_MODEL_KEY_SEAL_KEY_PREVIOUS, or pass --delete-unreadable (their owners run setup again)", len(unreadable))
	}
	for _, o := range readable {
		k := o.row
		ciphertext, nonce, e := seal.seal(o.key, aadOf(k))
		if e != nil {
			return out, e
		}
		res, e := tx.ExecContext(ctx, `UPDATE dispatcher_model_keys SET ciphertext=?,nonce=? WHERE owner_principal=? AND state=? AND ciphertext=? AND nonce=?`, ciphertext, nonce, str(k, "owner_principal"), str(k, "state"), str(k, "ciphertext"), str(k, "nonce"))
		if e != nil {
			return out, errors.New("authority unavailable")
		}
		if n, _ := res.RowsAffected(); n == 1 {
			out.Resealed++
		}
	}
	for _, k := range unreadable {
		owner := str(k, "owner_principal")
		res, e := tx.ExecContext(ctx, `DELETE FROM dispatcher_model_keys WHERE owner_principal=? AND state=? AND ciphertext=? AND nonce=?`, owner, str(k, "state"), str(k, "ciphertext"), str(k, "nonce"))
		if e != nil {
			return out, errors.New("authority unavailable")
		}
		if n, _ := res.RowsAffected(); n != 1 {
			continue
		}
		out.Deleted++
		left, e := query(ctx, tx, `SELECT 1 FROM dispatcher_model_keys WHERE owner_principal=?`, owner)
		if e != nil {
			return out, e
		}
		if len(left) == 0 {
			if e = revokeOwnerCredentials(ctx, tx, owner, nowRaw); e != nil {
				return out, e
			}
		}
	}
	// What still opens only under the previous key, read back [#123 L2].
	after, err := query(ctx, tx, `SELECT owner_principal,state,provider_id,api,base_url,model,config_revision,ciphertext,nonce FROM dispatcher_model_keys`)
	if err != nil {
		return out, errors.New("authority unavailable")
	}
	out.NeedPrevious = 0
	for _, k := range after {
		key, e := current.open(str(k, "ciphertext"), str(k, "nonce"), aadOf(k))
		if e != nil {
			out.NeedPrevious++
			continue
		}
		clear(key)
	}
	workspaces, err := query(ctx, tx, `SELECT id FROM workspaces`)
	if err != nil {
		return out, errors.New("authority unavailable")
	}
	for _, w := range workspaces {
		if _, err = tx.ExecContext(ctx, `INSERT INTO history(id,workspace_id,project_id,entity,entity_key,action,details,changed_at,changed_by_principal,changed_by_session) VALUES(?,?,NULL,'dispatcher_model_key','seal_key','resealed',?,?,?,NULL)`,
			newID("evt_"), str(w, "id"), asJSON(map[string]any{"resealed": out.Resealed, "deleted_unreadable": out.Deleted, "need_previous": out.NeedPrevious}), stamp(), OperatorPrincipal); err != nil {
			return out, errors.New("authority unavailable")
		}
	}
	if err = tx.Commit(); err != nil {
		return out, errors.New("authority commit failed")
	}
	return out, nil
}
