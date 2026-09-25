package store

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// ClaimAvailable reserves endpoint capacity and rate permits in the same
// transaction as attempt leases. All workers share these PostgreSQL counters.
func (s *Store) ClaimAvailable(ctx context.Context, workerID string, now time.Time, lease time.Duration, limit int) ([]ClaimedDelivery, error) {
	if limit < 1 || limit > 1000 || lease <= 0 || workerID == "" {
		return nil, fmt.Errorf("invalid claim configuration")
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	// Expire bounded batches even when endpoints are paused or rate limited.
	// Never overwrite a live lease; its worker may still acknowledge success.
	if _, err = tx.Exec(ctx, `WITH due AS (
        SELECT id FROM delivery_attempts WHERE expires_at <= $1
        AND (state='pending' OR (state='in_progress' AND lease_until <= $1))
        ORDER BY expires_at,id LIMIT 1000 FOR UPDATE SKIP LOCKED
    ) UPDATE delivery_attempts a SET state='dead_letter',completed_at=$1,updated_at=$1,lease_until=NULL,
        error_code='event_expired',error_message='delivery cycle expired',retryable=false
        FROM due WHERE a.id=due.id`, now); err != nil {
		return nil, err
	}
	rows, err := tx.Query(ctx, `
        SELECT endpoint.id FROM webhook_endpoints endpoint
        WHERE NOT endpoint.paused AND (rate_window <= $1::timestamptz - INTERVAL '1 second' OR rate_used < rate_limit)
          AND (SELECT count(*) FROM delivery_attempts a WHERE a.endpoint_id=endpoint.id
               AND a.state='in_progress' AND a.lease_until > $1) < concurrency_limit
          AND EXISTS (SELECT 1 FROM delivery_attempts a WHERE a.endpoint_id=endpoint.id
               AND (a.expires_at IS NULL OR a.expires_at > $1)
               AND a.available_at <= $1 AND (a.state='pending' OR (a.state='in_progress' AND a.lease_until <= $1)))
        ORDER BY (SELECT count(*) FROM delivery_attempts a WHERE a.endpoint_id=endpoint.id
                  AND a.state='in_progress' AND a.lease_until > $1), endpoint.last_service_seq, endpoint.id
        LIMIT $2 FOR NO KEY UPDATE OF endpoint SKIP LOCKED`, now, limit)
	if err != nil {
		return nil, fmt.Errorf("lock endpoints: %w", err)
	}
	endpoints, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, err
	}
	allocations := make([]endpointAllocation, 0, len(endpoints))
	for _, endpointID := range endpoints {
		var capacity, permits, ready, ceiling int
		err := tx.QueryRow(ctx, `
            SELECT concurrency_limit - (SELECT count(*) FROM delivery_attempts
                WHERE endpoint_id=$1 AND state='in_progress' AND lease_until > $2),
                rate_limit - CASE WHEN rate_window <= $2::timestamptz - INTERVAL '1 second' THEN 0 ELSE rate_used END,
                (SELECT count(*) FROM (SELECT 1 FROM delivery_attempts
                    WHERE endpoint_id=$1 AND available_at <= $2
                    AND (expires_at IS NULL OR expires_at > $2)
                    AND (state='pending' OR (state='in_progress' AND lease_until <= $2))
                    LIMIT $3) due), concurrency_limit
            FROM webhook_endpoints WHERE id=$1`, endpointID, now, limit).Scan(&capacity, &permits, &ready, &ceiling)
		if err != nil {
			return nil, err
		}
		allocations = append(allocations, endpointAllocation{id: endpointID, active: ceiling - capacity, capacity: max(0, min(capacity, permits, ready))})
	}
	claimed := make([]ClaimedDelivery, 0, limit)
	for _, allocation := range allocateClaims(allocations, limit) {
		endpointID, take := allocation.id, allocation.take
		if take == 0 {
			continue
		}
		rows, err := tx.Query(ctx, `
            WITH candidates AS (
                SELECT id FROM delivery_attempts WHERE endpoint_id=$1 AND available_at <= $2
                  AND (expires_at IS NULL OR expires_at > $2)
                  AND (state='pending' OR (state='in_progress' AND lease_until <= $2))
                ORDER BY available_at, id LIMIT $3 FOR UPDATE SKIP LOCKED
            ), claimed AS (
                UPDATE delivery_attempts a SET state='in_progress', lease_until=$4,
                    claimed_by=$5, claim_count=claim_count+1, last_started_at=$2, updated_at=$2,
                    endpoint_version=(SELECT version FROM webhook_endpoints WHERE id=$1),
                    signing_version=(SELECT signing_version FROM webhook_endpoints WHERE id=$1)
                FROM candidates WHERE a.id=candidates.id RETURNING a.*
            )
            SELECT c.id, e.id, e.event_type, e.payload, endpoint.id, endpoint.url,
                   endpoint.secret_ciphertext, c.claim_count, c.cycle_attempt, c.lease_until,
                   c.trace_parent, c.created_at, c.available_at,c.retry_base_seconds,c.retry_cap_seconds,c.expires_at,c.endpoint_version,c.signing_version
            FROM claimed c JOIN events e ON e.id=c.event_id
            JOIN webhook_endpoints endpoint ON endpoint.id=c.endpoint_id`,
			endpointID, now, take, now.Add(lease), workerID)
		if err != nil {
			return nil, fmt.Errorf("claim attempts: %w", err)
		}
		batch, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (ClaimedDelivery, error) {
			var d ClaimedDelivery
			err := row.Scan(&d.AttemptID, &d.EventID, &d.EventType, &d.Payload,
				&d.EndpointID, &d.EndpointURL, &d.SecretCiphertext, &d.ClaimCount, &d.CycleAttempt, &d.LeaseUntil,
				&d.TraceParent, &d.QueuedAt, &d.AvailableAt, &d.RetryBaseSeconds, &d.RetryCapSeconds, &d.ExpiresAt, &d.EndpointVersion, &d.SigningVersion)
			return d, err
		})
		if err != nil {
			return nil, err
		}
		if len(batch) == 0 {
			continue
		}
		_, err = tx.Exec(ctx, `UPDATE webhook_endpoints SET
            rate_used = CASE WHEN rate_window <= $2::timestamptz - INTERVAL '1 second' THEN $3 ELSE rate_used+$3 END,
            rate_window = CASE WHEN rate_window <= $2::timestamptz - INTERVAL '1 second' THEN $2 ELSE rate_window END,
            last_service_seq = nextval('endpoint_service_sequence')
            WHERE id=$1`, endpointID, now, len(batch))
		if err != nil {
			return nil, err
		}
		claimed = append(claimed, batch...)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO worker_progress(worker_id,polled_at,claimed_at) VALUES($1,$2,CASE WHEN $3>0 THEN $2::timestamptz END)
	ON CONFLICT(worker_id) DO UPDATE SET polled_at=excluded.polled_at,claimed_at=coalesce(excluded.claimed_at,worker_progress.claimed_at)`, workerID, now, len(claimed)); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("commit claims: %w", err)
	}
	return claimed, nil
}
