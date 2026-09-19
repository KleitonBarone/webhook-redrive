package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/KleitonBarone/webhook-redrive/internal/id"
	"github.com/KleitonBarone/webhook-redrive/internal/telemetry"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"go.opentelemetry.io/otel/trace"
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
	var base, cap int
	var expires *time.Time
	err = tx.QueryRow(ctx, `SELECT attempt_number, cycle_attempt, max_attempts, endpoint_id,retry_base_seconds,retry_cap_seconds,expires_at
        FROM delivery_attempts WHERE id=$1 AND event_id=$2 AND state='in_progress'
        AND claimed_by=$3 AND claim_count=$4 AND lease_until > $5 FOR UPDATE`,
		claim.AttemptID, claim.EventID, workerID, claim.ClaimCount, now).Scan(&number, &cycle, &maximum, &endpointID, &base, &cap, &expires)
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
	if outcome.Code == "event_expired" || (outcome.Code != "" && expires != nil && !now.Before(*expires)) {
		state = "dead_letter"
	}
	_, err = tx.Exec(ctx, `UPDATE delivery_attempts SET state=$2, completed_at=$3,
        updated_at=$3, lease_until=NULL, response_status=NULLIF($4,0),
        error_code=NULLIF($5,''), error_message=NULLIF($6,''), retryable=$7 WHERE id=$1`,
		claim.AttemptID, state, now, outcome.Status, outcome.Code, outcome.Message, retryable)
	if err != nil {
		return false, err
	}
	if state == "failed" && retryable {
		due := *outcome.RetryAt
		if expires != nil && due.After(*expires) {
			due = *expires
		}
		nextID, err := id.New()
		if err != nil {
			return false, err
		}
		_, err = tx.Exec(ctx, `INSERT INTO delivery_attempts
            (id,event_id,endpoint_id,state,attempt_number,cycle_attempt,max_attempts,available_at,created_at,updated_at,trace_parent,retry_base_seconds,retry_cap_seconds,expires_at)
            VALUES ($1,$2,$3,'pending',$4,$5,$6,$7,$8,$8,$9,$10,$11,$12)`,
			nextID, claim.EventID, endpointID, number+1, cycle+1, maximum, due, now, telemetry.Parent(ctx), base, cap, expires)
		if err != nil {
			return false, fmt.Errorf("schedule retry: %w", err)
		}
	}
	if _, err := tx.Exec(ctx, `UPDATE worker_progress SET completed_at=$2 WHERE worker_id=$1`, workerID, now); err != nil {
		return false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return false, err
	}
	return true, nil
}

type ReplayRequest struct {
	AttemptID   string `json:"attempt_id"`
	RequestID   string `json:"request_id"`
	Actor       string `json:"-"`
	PrincipalID string `json:"-"`
	Reason      string `json:"reason"`
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
	result, err := replayTx(ctx, tx, eventID, input, now)
	if err != nil {
		return ReplayResult{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return ReplayResult{}, err
	}
	return result, nil
}

// replayTx is shared by single replay and atomic bulk item completion.
func replayTx(ctx context.Context, tx pgx.Tx, eventID string, input ReplayRequest, now time.Time) (ReplayResult, error) {
	var endpointID string
	err := tx.QueryRow(ctx, `SELECT endpoint_id FROM events WHERE id=$1 FOR NO KEY UPDATE`, eventID).Scan(&endpointID)
	if errors.Is(err, pgx.ErrNoRows) {
		return ReplayResult{}, ErrNotFound
	}
	if err != nil {
		return ReplayResult{}, err
	}
	var existing ReplayResult
	var original ReplayRequest
	err = tx.QueryRow(ctx, `SELECT id,event_id,replay_request_id,replay_of,replay_actor,replay_reason,coalesce(replay_principal_id::text,'')
        FROM delivery_attempts WHERE replay_request_id=$1`, input.RequestID).Scan(
		&existing.AttemptID, &existing.EventID, &existing.RequestID, &original.AttemptID, &original.Actor, &original.Reason, &original.PrincipalID)
	if err == nil {
		if existing.EventID != eventID || original.AttemptID != input.AttemptID || original.PrincipalID != input.PrincipalID || original.Reason != input.Reason || (input.PrincipalID == "" && original.Actor != input.Actor) {
			return ReplayResult{}, ErrConflict
		}
		return existing, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return ReplayResult{}, err
	}
	var latestID, state, parent string
	var number int
	err = tx.QueryRow(ctx, `SELECT id,state,attempt_number,trace_parent FROM delivery_attempts
		WHERE event_id=$1 ORDER BY attempt_number DESC LIMIT 1`, eventID).Scan(&latestID, &state, &number, &parent)
	if err != nil {
		return ReplayResult{}, err
	}
	if latestID != input.AttemptID || (state != "failed" && state != "dead_letter") {
		return ReplayResult{}, ErrConflict
	}
	if sc := trace.SpanContextFromContext(telemetry.Extract(ctx, parent)); sc.IsValid() {
		trace.SpanFromContext(ctx).AddLink(trace.Link{SpanContext: sc})
	}
	nextID, err := id.New()
	if err != nil {
		return ReplayResult{}, err
	}
	_, err = tx.Exec(ctx, `INSERT INTO delivery_attempts
        (id,event_id,endpoint_id,state,attempt_number,cycle_attempt,max_attempts,available_at,created_at,updated_at,
         replay_of,replay_request_id,replay_actor,replay_reason,trace_parent,replay_principal_id,retry_base_seconds,retry_cap_seconds,expires_at)
        SELECT $1,$2,$3,'pending',$4,1,max_attempts,$5,$5,$5,$6,$7,$8,$9,$10,NULLIF($11,'')::uuid,
            retry_base_seconds,retry_cap_seconds,$5::timestamptz+event_ttl_seconds * interval '1 second'
        FROM webhook_endpoints WHERE id=$3`,
		nextID, eventID, endpointID, number+1, now, input.AttemptID, input.RequestID, input.Actor, input.Reason, telemetry.Parent(ctx), input.PrincipalID)
	if err != nil {
		var postgresError *pgconn.PgError
		if errors.As(err, &postgresError) && postgresError.Code == "23505" {
			return ReplayResult{}, ErrConflict
		}
		return ReplayResult{}, err
	}
	return ReplayResult{AttemptID: nextID, EventID: eventID, RequestID: input.RequestID}, nil
}
