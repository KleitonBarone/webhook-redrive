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
	rows, err := tx.Query(ctx, `
        SELECT endpoint.id FROM webhook_endpoints endpoint
        WHERE (rate_window <= $1::timestamptz - INTERVAL '1 second' OR rate_used < rate_limit)
          AND (SELECT count(*) FROM delivery_attempts a WHERE a.endpoint_id=endpoint.id
               AND a.state='in_progress' AND a.lease_until > $1) < concurrency_limit
          AND EXISTS (SELECT 1 FROM delivery_attempts a WHERE a.endpoint_id=endpoint.id
               AND a.available_at <= $1 AND (a.state='pending' OR (a.state='in_progress' AND a.lease_until <= $1)))
        ORDER BY (SELECT min(a.available_at) FROM delivery_attempts a WHERE a.endpoint_id=endpoint.id
                  AND a.state IN ('pending','in_progress')), endpoint.id
        LIMIT $2 FOR NO KEY UPDATE OF endpoint SKIP LOCKED`, now, limit)
	if err != nil {
		return nil, fmt.Errorf("lock endpoints: %w", err)
	}
	endpoints, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, err
	}
	claimed := make([]ClaimedDelivery, 0, limit)
	for _, endpointID := range endpoints {
		if len(claimed) == limit {
			break
		}
		var capacity, permits int
		err := tx.QueryRow(ctx, `
            SELECT concurrency_limit - (SELECT count(*) FROM delivery_attempts
                WHERE endpoint_id=$1 AND state='in_progress' AND lease_until > $2),
                rate_limit - CASE WHEN rate_window <= $2::timestamptz - INTERVAL '1 second' THEN 0 ELSE rate_used END
            FROM webhook_endpoints WHERE id=$1`, endpointID, now).Scan(&capacity, &permits)
		if err != nil {
			return nil, err
		}
		take := min(capacity, permits, limit-len(claimed))
		if take <= 0 {
			continue
		}
		rows, err := tx.Query(ctx, `
            WITH candidates AS (
                SELECT id FROM delivery_attempts WHERE endpoint_id=$1 AND available_at <= $2
                  AND (state='pending' OR (state='in_progress' AND lease_until <= $2))
                ORDER BY available_at, id LIMIT $3 FOR UPDATE SKIP LOCKED
            ), claimed AS (
                UPDATE delivery_attempts a SET state='in_progress', lease_until=$4,
                    claimed_by=$5, claim_count=claim_count+1, last_started_at=$2, updated_at=$2
                FROM candidates WHERE a.id=candidates.id RETURNING a.*
            )
            SELECT c.id, e.id, e.event_type, e.payload, endpoint.id, endpoint.url,
                   endpoint.secret_ciphertext, c.claim_count, c.cycle_attempt, c.lease_until
            FROM claimed c JOIN events e ON e.id=c.event_id
            JOIN webhook_endpoints endpoint ON endpoint.id=c.endpoint_id`,
			endpointID, now, take, now.Add(lease), workerID)
		if err != nil {
			return nil, fmt.Errorf("claim attempts: %w", err)
		}
		batch, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (ClaimedDelivery, error) {
			var d ClaimedDelivery
			err := row.Scan(&d.AttemptID, &d.EventID, &d.EventType, &d.Payload,
				&d.EndpointID, &d.EndpointURL, &d.SecretCiphertext, &d.ClaimCount, &d.CycleAttempt, &d.LeaseUntil)
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
            rate_window = CASE WHEN rate_window <= $2::timestamptz - INTERVAL '1 second' THEN $2 ELSE rate_window END
            WHERE id=$1`, endpointID, now, len(batch))
		if err != nil {
			return nil, err
		}
		claimed = append(claimed, batch...)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("commit claims: %w", err)
	}
	return claimed, nil
}
