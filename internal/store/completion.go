package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/KleitonBarone/webhook-redrive/internal/id"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

type Outcome struct {
	Status  int
	Code    string
	Message string
	RetryAt *time.Time
}

// Complete fences every result with the claim generation and an unexpired
// lease. A failed result and its successor commit together or neither does.
func (s *Store) Complete(ctx context.Context, claim ClaimedDelivery, workerID string, now time.Time, outcome Outcome) (bool, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `SELECT id FROM events WHERE id=$1 FOR NO KEY UPDATE`, claim.EventID); err != nil {
		return false, err
	}
	var number, cycle, maximum int
	var endpointID string
	err = tx.QueryRow(ctx, `SELECT attempt_number, cycle_attempt, max_attempts, endpoint_id
        FROM delivery_attempts WHERE id=$1 AND event_id=$2 AND state='in_progress'
        AND claimed_by=$3 AND claim_count=$4 AND lease_until > $5 FOR UPDATE`,
		claim.AttemptID, claim.EventID, workerID, claim.ClaimCount, now).Scan(&number, &cycle, &maximum, &endpointID)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	state := "succeeded"
	retryable := outcome.RetryAt != nil
	if outcome.Code != "" {
		state = "failed"
		if retryable && cycle >= maximum {
			state = "dead_letter"
		}
	}
	_, err = tx.Exec(ctx, `UPDATE delivery_attempts SET state=$2, completed_at=$3,
        updated_at=$3, lease_until=NULL, response_status=NULLIF($4,0),
        error_code=NULLIF($5,''), error_message=NULLIF($6,''), retryable=$7 WHERE id=$1`,
		claim.AttemptID, state, now, outcome.Status, outcome.Code, outcome.Message, retryable)
	if err != nil {
		return false, err
	}
	if state == "failed" && retryable {
		nextID, err := id.New()
		if err != nil {
			return false, err
		}
		_, err = tx.Exec(ctx, `INSERT INTO delivery_attempts
            (id,event_id,endpoint_id,state,attempt_number,cycle_attempt,max_attempts,available_at,created_at,updated_at)
            VALUES ($1,$2,$3,'pending',$4,$5,$6,$7,$8,$8)`,
			nextID, claim.EventID, endpointID, number+1, cycle+1, maximum, outcome.RetryAt, now)
		if err != nil {
			return false, fmt.Errorf("schedule retry: %w", err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return false, err
	}
	return true, nil
}

type ReplayRequest struct {
	AttemptID string `json:"attempt_id"`
	RequestID string `json:"request_id"`
	Actor     string `json:"actor"`
	Reason    string `json:"reason"`
}

type ReplayResult struct {
	AttemptID string `json:"attempt_id"`
	EventID   string `json:"event_id"`
	RequestID string `json:"request_id"`
}

// Replay requires the current failed attempt as a precondition. Its request ID
// makes repeated submissions safe even after the replay has finished.
func (s *Store) Replay(ctx context.Context, eventID string, input ReplayRequest, now time.Time) (ReplayResult, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return ReplayResult{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var endpointID string
	err = tx.QueryRow(ctx, `SELECT endpoint_id FROM events WHERE id=$1 FOR NO KEY UPDATE`, eventID).Scan(&endpointID)
	if errors.Is(err, pgx.ErrNoRows) {
		return ReplayResult{}, ErrNotFound
	}
	if err != nil {
		return ReplayResult{}, err
	}
	var existing ReplayResult
	var original ReplayRequest
	err = tx.QueryRow(ctx, `SELECT id,event_id,replay_request_id,replay_of,replay_actor,replay_reason
        FROM delivery_attempts WHERE replay_request_id=$1`, input.RequestID).Scan(
		&existing.AttemptID, &existing.EventID, &existing.RequestID, &original.AttemptID, &original.Actor, &original.Reason)
	if err == nil {
		if existing.EventID != eventID || original.AttemptID != input.AttemptID || original.Actor != input.Actor || original.Reason != input.Reason {
			return ReplayResult{}, ErrConflict
		}
		return existing, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return ReplayResult{}, err
	}
	var latestID, state string
	var number, maximum int
	err = tx.QueryRow(ctx, `SELECT id,state,attempt_number,max_attempts FROM delivery_attempts
        WHERE event_id=$1 ORDER BY attempt_number DESC LIMIT 1`, eventID).Scan(&latestID, &state, &number, &maximum)
	if err != nil {
		return ReplayResult{}, err
	}
	if latestID != input.AttemptID || (state != "failed" && state != "dead_letter") {
		return ReplayResult{}, ErrConflict
	}
	nextID, err := id.New()
	if err != nil {
		return ReplayResult{}, err
	}
	_, err = tx.Exec(ctx, `INSERT INTO delivery_attempts
        (id,event_id,endpoint_id,state,attempt_number,cycle_attempt,max_attempts,available_at,created_at,updated_at,
         replay_of,replay_request_id,replay_actor,replay_reason)
        VALUES ($1,$2,$3,'pending',$4,1,$5,$6,$6,$6,$7,$8,$9,$10)`,
		nextID, eventID, endpointID, number+1, maximum, now, input.AttemptID, input.RequestID, input.Actor, input.Reason)
	if err != nil {
		var postgresError *pgconn.PgError
		if errors.As(err, &postgresError) && postgresError.Code == "23505" {
			return ReplayResult{}, ErrConflict
		}
		return ReplayResult{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return ReplayResult{}, err
	}
	return ReplayResult{AttemptID: nextID, EventID: eventID, RequestID: input.RequestID}, nil
}
