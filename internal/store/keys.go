package store

import (
	"bytes"
	"context"
	"errors"
	"time"

	"github.com/KleitonBarone/webhook-redrive/internal/secret"
)

var ErrMasterKey = errors.New("master key does not match database")
var keyMarker = []byte("webhook-redrive/master-key/v1")

// CheckMasterKey binds even an empty database to its wrapping key. On upgrade,
// every existing endpoint must decrypt before the marker can be initialized.
func (s *Store) CheckMasterKey(ctx context.Context, box *secret.Box) error {
	_, err := s.masterKey(ctx, box, nil, time.Time{}, true)
	return err
}

// RotateMasterKey requires all API and worker processes to be stopped. Table
// locking protects the transaction, not already-running processes' cached keys.
func (s *Store) RotateMasterKey(ctx context.Context, old, next *secret.Box, now time.Time, apply bool) (int, error) {
	if next == nil {
		return 0, ErrMasterKey
	}
	return s.masterKey(ctx, old, next, now, apply)
}

func (s *Store) masterKey(ctx context.Context, old, next *secret.Box, now time.Time, apply bool) (int, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var marker []byte
	if err = tx.QueryRow(ctx, `SELECT check_ciphertext FROM master_key_state FOR UPDATE`).Scan(&marker); err != nil {
		return 0, err
	}
	if marker != nil {
		plain, e := old.Decrypt(marker)
		if e != nil || !bytes.Equal(plain, keyMarker) {
			return 0, ErrMasterKey
		}
		if next == nil {
			return 0, tx.Commit(ctx)
		}
	}
	if _, err = tx.Exec(ctx, `LOCK TABLE webhook_endpoints IN EXCLUSIVE MODE`); err != nil {
		return 0, err
	}
	rows, err := tx.Query(ctx, `SELECT id,secret_ciphertext FROM webhook_endpoints ORDER BY id`)
	if err != nil {
		return 0, err
	}
	type wrapped struct {
		id     string
		cipher []byte
	}
	var endpoints []wrapped
	for rows.Next() {
		var e wrapped
		if err = rows.Scan(&e.id, &e.cipher); err != nil {
			rows.Close()
			return 0, err
		}
		endpoints = append(endpoints, e)
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return 0, err
	}
	for _, e := range endpoints {
		plain, err := old.Decrypt(e.cipher)
		if err != nil {
			return 0, ErrMasterKey
		}
		if next != nil && apply {
			cipher, err := next.Encrypt(plain)
			if err != nil {
				return 0, err
			}
			if _, err = tx.Exec(ctx, `UPDATE webhook_endpoints SET secret_ciphertext=$2 WHERE id=$1`, e.id, cipher); err != nil {
				return 0, err
			}
		}
	}
	if !apply {
		return len(endpoints), nil
	}
	target := old
	if next != nil {
		target = next
	}
	cipher, err := target.Encrypt(keyMarker)
	if err != nil {
		return 0, err
	}
	if _, err = tx.Exec(ctx, `UPDATE master_key_state SET check_ciphertext=$1,generation=generation+$2`, cipher, boolInt(next != nil)); err != nil {
		return 0, err
	}
	if next != nil {
		if _, err = tx.Exec(ctx, `INSERT INTO maintenance_audit(action,affected,created_at) VALUES('master_key_rotation',$1,$2)`, len(endpoints), now); err != nil {
			return 0, err
		}
	}
	return len(endpoints), tx.Commit(ctx)
}

func boolInt(v bool) int {
	if v {
		return 1
	}
	return 0
}

func (s *Store) Ready(ctx context.Context) error { return s.pool.Ping(ctx) }
