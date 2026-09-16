package store

import (
	"context"
	"errors"
	"time"

	"github.com/KleitonBarone/webhook-redrive/internal/auth"
	"github.com/KleitonBarone/webhook-redrive/internal/id"
	"github.com/jackc/pgx/v5"
)

func (s *Store) Authenticate(ctx context.Context, hash []byte, now time.Time) (auth.Principal, error) {
	var p auth.Principal
	err := s.pool.QueryRow(ctx, `SELECT p.id,p.name,p.kind,p.permissions FROM credentials c
        JOIN principals p ON p.id=c.principal_id
        WHERE c.token_hash=$1 AND c.revoked_at IS NULL AND c.expires_at > $2`, hash, now).
		Scan(&p.ID, &p.Name, &p.Kind, &p.Permissions)
	if errors.Is(err, pgx.ErrNoRows) {
		return p, auth.ErrUnauthorized
	}
	return p, err
}

func (s *Store) CreatePrincipal(ctx context.Context, p auth.Principal, now time.Time) error {
	if !id.Valid(p.ID) || len(p.Name) < 1 || len(p.Name) > 100 ||
		(p.Kind != "service" && p.Kind != "operator") || !auth.ValidPermissions(p.Permissions) {
		return errors.New("invalid principal")
	}
	_, err := s.pool.Exec(ctx, `INSERT INTO principals(id,name,kind,permissions,created_at) VALUES($1,$2,$3,$4,$5)`,
		p.ID, p.Name, p.Kind, p.Permissions, now)
	return err
}

func (s *Store) IssueCredential(ctx context.Context, principalID, credentialID string, hash []byte, now, expires time.Time) error {
	if !id.Valid(principalID) || !id.Valid(credentialID) || len(hash) != 32 || !expires.After(now) {
		return errors.New("invalid credential")
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err = tx.Exec(ctx, `INSERT INTO credentials(id,principal_id,token_hash,created_at,expires_at) VALUES($1,$2,$3,$4,$5)`,
		credentialID, principalID, hash, now, expires); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `INSERT INTO credential_audit(credential_id,action,created_at) VALUES($1,'issued',$2)`, credentialID, now); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// RevokeCredential is idempotent and commits the revocation with its audit row.
func (s *Store) RevokeCredential(ctx context.Context, credentialID string, now time.Time) error {
	tag, err := s.pool.Exec(ctx, `WITH revoked AS (
        UPDATE credentials SET revoked_at=$2 WHERE id=$1 AND revoked_at IS NULL RETURNING id
    ) INSERT INTO credential_audit(credential_id,action,created_at) SELECT id,'revoked',$2 FROM revoked`, credentialID, now)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		var exists bool
		if err := s.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM credentials WHERE id=$1)`, credentialID).Scan(&exists); err != nil {
			return err
		}
		if !exists {
			return ErrNotFound
		}
	}
	return nil
}
