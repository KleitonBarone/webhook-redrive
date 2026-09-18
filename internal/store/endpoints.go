package store

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

var ErrEndpointConflict = errors.New("endpoint version or rotation precondition failed")

const endpointColumns = `id,url,created_at,coalesce(created_by::text,''),max_attempts,concurrency_limit,rate_limit,
    version,paused,signing_version,retiring_signing_version,retire_after,retry_base_seconds,retry_cap_seconds,event_ttl_seconds`

func scanEndpoint(row pgx.Row) (Endpoint, error) {
	var e Endpoint
	err := row.Scan(&e.ID, &e.URL, &e.CreatedAt, &e.CreatedBy, &e.MaxAttempts, &e.ConcurrencyLimit, &e.RateLimit,
		&e.Version, &e.Paused, &e.SigningVersion, &e.RetiringSigningVersion, &e.RetireAfter, &e.RetryBaseSeconds, &e.RetryCapSeconds, &e.EventTTLSeconds)
	if errors.Is(err, pgx.ErrNoRows) {
		err = ErrNotFound
	}
	return e, err
}

func (s *Store) GetEndpoint(ctx context.Context, endpointID string) (Endpoint, error) {
	return scanEndpoint(s.pool.QueryRow(ctx, `SELECT `+endpointColumns+` FROM webhook_endpoints WHERE id=$1`, endpointID))
}

func (s *Store) ListEndpoints(ctx context.Context, after string) ([]Endpoint, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+endpointColumns+` FROM webhook_endpoints WHERE id::text > $1 ORDER BY id LIMIT 100`, after)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (Endpoint, error) { return scanEndpoint(row) })
}

type EndpointChange struct {
	ExpectedVersion int64
	PrincipalID     string
	Reason          string
	Action          string
	Settings        Endpoint
	Ciphertext      []byte
	Overlap         time.Duration
}

type EndpointAudit struct {
	Version       int64     `json:"version"`
	PrincipalID   string    `json:"principal_id,omitempty"`
	Action        string    `json:"action"`
	Reason        string    `json:"reason"`
	Configuration Endpoint  `json:"configuration"`
	CreatedAt     time.Time `json:"created_at"`
}

func auditEndpoint(ctx context.Context, tx pgx.Tx, e Endpoint, principal, action, reason string, now time.Time) error {
	snapshot, err := json.Marshal(e)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `INSERT INTO endpoint_audit(endpoint_id,version,principal_id,action,reason,configuration,created_at)
        VALUES($1,$2,NULLIF($3,'')::uuid,$4,$5,$6,$7)`, e.ID, e.Version, principal, action, reason, snapshot, now)
	return err
}

func (s *Store) ListEndpointAudit(ctx context.Context, endpointID string, after int64) ([]EndpointAudit, error) {
	rows, err := s.pool.Query(ctx, `SELECT version,coalesce(principal_id::text,''),action,reason,configuration,created_at
        FROM endpoint_audit WHERE endpoint_id=$1 AND version > $2 ORDER BY version LIMIT 100`, endpointID, after)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (EndpointAudit, error) {
		var a EndpointAudit
		err := row.Scan(&a.Version, &a.PrincipalID, &a.Action, &a.Reason, &a.Configuration, &a.CreatedAt)
		return a, err
	})
}

// ChangeEndpoint shares the claim lock. A committed pause prevents later claims;
// claims committed before it keep their captured destination and signing key.
func (s *Store) ChangeEndpoint(ctx context.Context, endpointID string, change EndpointChange, now time.Time) (Endpoint, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Endpoint{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	e, err := scanEndpoint(tx.QueryRow(ctx, `SELECT `+endpointColumns+` FROM webhook_endpoints WHERE id=$1 FOR NO KEY UPDATE`, endpointID))
	if err != nil {
		return e, err
	}
	if e.Version != change.ExpectedVersion {
		return Endpoint{}, ErrEndpointConflict
	}
	switch change.Action {
	case "updated":
		e.URL, e.MaxAttempts, e.ConcurrencyLimit, e.RateLimit = change.Settings.URL, change.Settings.MaxAttempts, change.Settings.ConcurrencyLimit, change.Settings.RateLimit
		e.RetryBaseSeconds, e.RetryCapSeconds, e.EventTTLSeconds = change.Settings.RetryBaseSeconds, change.Settings.RetryCapSeconds, change.Settings.EventTTLSeconds
	case "paused", "resumed":
		e.Paused = change.Action == "paused"
	case "rotated":
		if e.RetiringSigningVersion != nil || len(change.Ciphertext) == 0 || change.Overlap < 5*time.Minute || change.Overlap > 24*time.Hour {
			return Endpoint{}, ErrEndpointConflict
		}
		old := e.SigningVersion
		e.RetiringSigningVersion = &old
		retirement := now.Add(change.Overlap)
		e.RetireAfter = &retirement
		e.SigningVersion++
		if _, err = tx.Exec(ctx, `UPDATE webhook_endpoints SET secret_ciphertext=$2 WHERE id=$1`, endpointID, change.Ciphertext); err != nil {
			return Endpoint{}, err
		}
	case "retired":
		if e.RetiringSigningVersion == nil || now.Before(*e.RetireAfter) {
			return Endpoint{}, ErrEndpointConflict
		}
		var active bool
		err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM delivery_attempts WHERE endpoint_id=$1
            AND state='in_progress' AND lease_until > $2 AND (signing_version IS NULL OR signing_version <= $3))`, endpointID, now, *e.RetiringSigningVersion).Scan(&active)
		if err != nil {
			return Endpoint{}, err
		}
		if active {
			return Endpoint{}, ErrEndpointConflict
		}
		e.RetiringSigningVersion, e.RetireAfter = nil, nil
	default:
		return Endpoint{}, errors.New("invalid endpoint action")
	}
	e.Version++
	_, err = tx.Exec(ctx, `UPDATE webhook_endpoints SET url=$2,max_attempts=$3,concurrency_limit=$4,rate_limit=$5,
        version=$6,paused=$7,signing_version=$8,retiring_signing_version=$9,retire_after=$10,
        retry_base_seconds=$11,retry_cap_seconds=$12,event_ttl_seconds=$13 WHERE id=$1`,
		e.ID, e.URL, e.MaxAttempts, e.ConcurrencyLimit, e.RateLimit, e.Version, e.Paused, e.SigningVersion, e.RetiringSigningVersion, e.RetireAfter, e.RetryBaseSeconds, e.RetryCapSeconds, e.EventTTLSeconds)
	if err != nil {
		return Endpoint{}, err
	}
	if err = auditEndpoint(ctx, tx, e, change.PrincipalID, change.Action, change.Reason, now); err != nil {
		return Endpoint{}, err
	}
	return e, tx.Commit(ctx)
}
