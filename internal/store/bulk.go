package store

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"sort"
	"strings"
	"time"

	"github.com/KleitonBarone/webhook-redrive/internal/id"
	"github.com/jackc/pgx/v5"
)

var ErrInvalidBatch = errors.New("invalid recovery selection")

type ReplaySelection struct {
	EventID   string `json:"event_id"`
	AttemptID string `json:"attempt_id"`
}
type BatchRequest struct {
	RequestID   string            `json:"request_id"`
	Reason      string            `json:"reason"`
	Events      []ReplaySelection `json:"events"`
	PrincipalID string            `json:"-"`
	Actor       string            `json:"-"`
}
type BatchItem struct {
	ReplaySelection
	PreviewState    string     `json:"preview_state"`
	PreviewEligible bool       `json:"preview_eligible"`
	Result          string     `json:"result"`
	ReplayAttemptID *string    `json:"replay_attempt_id,omitempty"`
	CompletedAt     *time.Time `json:"completed_at,omitempty"`
}
type ReplayBatch struct {
	ID          string      `json:"id"`
	PrincipalID string      `json:"principal_id"`
	Reason      string      `json:"reason"`
	CreatedAt   time.Time   `json:"created_at"`
	StartedAt   *time.Time  `json:"started_at,omitempty"`
	State       string      `json:"state"`
	Items       []BatchItem `json:"items"`
}

// PreviewBatch freezes a bounded explicit selection without scheduling delivery.
// Reusing its ID with the same creator, reason, and set returns durable progress.
func (s *Store) PreviewBatch(ctx context.Context, input BatchRequest, now time.Time) (ReplayBatch, error) {
	if !id.Valid(input.RequestID) || !id.Valid(input.PrincipalID) || len(input.Events) < 1 || len(input.Events) > 100 || len(strings.TrimSpace(input.Reason)) < 1 || len(input.Reason) > 500 {
		return ReplayBatch{}, ErrInvalidBatch
	}
	selection := append([]ReplaySelection(nil), input.Events...)
	sort.Slice(selection, func(i, j int) bool { return selection[i].EventID < selection[j].EventID })
	for i, item := range selection {
		if !id.Valid(item.EventID) || !id.Valid(item.AttemptID) || (i > 0 && selection[i-1].EventID == item.EventID) {
			return ReplayBatch{}, ErrInvalidBatch
		}
	}
	encoded, _ := json.Marshal(selection)
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return ReplayBatch{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	tag, err := tx.Exec(ctx, `INSERT INTO replay_batches(id,principal_id,actor,reason,selection,created_at) VALUES($1,$2,$3,$4,$5,$6) ON CONFLICT(id) DO NOTHING`, input.RequestID, input.PrincipalID, input.Actor, input.Reason, encoded, now)
	if err != nil {
		return ReplayBatch{}, err
	}
	if tag.RowsAffected() == 0 {
		var principal, reason string
		var raw []byte
		if err := tx.QueryRow(ctx, `SELECT principal_id,reason,selection FROM replay_batches WHERE id=$1 FOR UPDATE`, input.RequestID).Scan(&principal, &reason, &raw); err != nil {
			return ReplayBatch{}, err
		}
		var original []ReplaySelection
		if err := json.Unmarshal(raw, &original); err != nil {
			return ReplayBatch{}, err
		}
		canonical, _ := json.Marshal(original)
		if principal != input.PrincipalID || reason != input.Reason || !bytes.Equal(canonical, encoded) {
			return ReplayBatch{}, ErrConflict
		}
	} else {
		// One statement captures the advisory preview of every selected latest state.
		// Requiring the expected attempt to belong to its event also fences bad IDs.
		requestIDs := make([]string, len(selection))
		for i := range requestIDs {
			requestIDs[i], err = id.New()
			if err != nil {
				return ReplayBatch{}, err
			}
		}
		tag, err = tx.Exec(ctx, `INSERT INTO replay_batch_items(batch_id,position,event_id,expected_attempt_id,request_id,preview_state,preview_eligible)
   SELECT $1,(item.n-1)::int,e.id,expected.id,($3::uuid[])[item.n],latest.state,
   latest.id=expected.id AND latest.state IN ('failed','dead_letter')
   FROM jsonb_array_elements($2::jsonb) WITH ORDINALITY item(value,n)
   JOIN events e ON e.id=(item.value->>'event_id')::uuid
   JOIN delivery_attempts expected ON expected.id=(item.value->>'attempt_id')::uuid AND expected.event_id=e.id
   JOIN LATERAL (SELECT id,state FROM delivery_attempts WHERE event_id=e.id ORDER BY attempt_number DESC LIMIT 1) latest ON true`, input.RequestID, encoded, requestIDs)
		if err != nil {
			return ReplayBatch{}, err
		}
		if int(tag.RowsAffected()) != len(selection) {
			return ReplayBatch{}, ErrNotFound
		}
	}
	batch, err := readBatch(ctx, tx, input.RequestID)
	if err != nil {
		return ReplayBatch{}, err
	}
	if err = tx.Commit(ctx); err != nil {
		return ReplayBatch{}, err
	}
	return batch, nil
}

func (s *Store) GetBatch(ctx context.Context, batchID string) (ReplayBatch, error) {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return ReplayBatch{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	return readBatch(ctx, tx, batchID)
}

func readBatch(ctx context.Context, tx pgx.Tx, batchID string) (ReplayBatch, error) {
	var b ReplayBatch
	err := tx.QueryRow(ctx, `SELECT id,principal_id,reason,created_at,started_at FROM replay_batches WHERE id=$1`, batchID).Scan(&b.ID, &b.PrincipalID, &b.Reason, &b.CreatedAt, &b.StartedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return b, ErrNotFound
	}
	if err != nil {
		return b, err
	}
	rows, err := tx.Query(ctx, `SELECT event_id,expected_attempt_id,preview_state,preview_eligible,result,replay_attempt_id,completed_at FROM replay_batch_items WHERE batch_id=$1 ORDER BY position`, batchID)
	if err != nil {
		return b, err
	}
	b.Items, err = pgx.CollectRows(rows, func(row pgx.CollectableRow) (BatchItem, error) {
		var item BatchItem
		err := row.Scan(&item.EventID, &item.AttemptID, &item.PreviewState, &item.PreviewEligible, &item.Result, &item.ReplayAttemptID, &item.CompletedAt)
		return item, err
	})
	b.State = "preview"
	if b.StartedAt != nil {
		b.State = "completed"
		for _, item := range b.Items {
			if item.Result == "pending" {
				b.State = "running"
				break
			}
		}
	}
	return b, err
}

// RunBatch commits at most ten items per call. Only the creating principal can
// confirm/resume; every HTTP call reauthenticates. No job continues in background.
func (s *Store) RunBatch(ctx context.Context, batchID, principalID string, now time.Time) (ReplayBatch, error) {
	for n := 0; n < 10; n++ {
		done, err := s.runBatchItem(ctx, batchID, principalID, now)
		if err != nil {
			return ReplayBatch{}, err
		}
		if done {
			break
		}
	}
	return s.GetBatch(ctx, batchID)
}

func (s *Store) runBatchItem(ctx context.Context, batchID, principalID string, now time.Time) (bool, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var owner, actor, reason string
	err = tx.QueryRow(ctx, `SELECT principal_id,actor,reason FROM replay_batches WHERE id=$1 FOR UPDATE`, batchID).Scan(&owner, &actor, &reason)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, ErrNotFound
	}
	if err != nil {
		return false, err
	}
	if owner != principalID {
		return false, ErrConflict
	}
	var eventID, attemptID, requestID string
	var position int
	var eligible bool
	err = tx.QueryRow(ctx, `SELECT position,event_id,expected_attempt_id,request_id,preview_eligible FROM replay_batch_items WHERE batch_id=$1 AND result='pending' ORDER BY position LIMIT 1`, batchID).Scan(&position, &eventID, &attemptID, &requestID, &eligible)
	if errors.Is(err, pgx.ErrNoRows) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	var replayID *string
	result := "skipped"
	if eligible {
		// Savepoint keeps an eligibility/unique conflict from aborting the item result.
		attemptTx, err := tx.Begin(ctx)
		if err != nil {
			return false, err
		}
		replay, replayErr := replayTx(ctx, attemptTx, eventID, ReplayRequest{AttemptID: attemptID, RequestID: requestID, Actor: actor, PrincipalID: owner, Reason: reason}, now)
		if replayErr != nil {
			if err := attemptTx.Rollback(ctx); err != nil {
				return false, err
			}
			if !errors.Is(replayErr, ErrConflict) {
				return false, replayErr
			}
		} else {
			if err := attemptTx.Commit(ctx); err != nil {
				return false, err
			}
			result = "replayed"
			replayID = &replay.AttemptID
		}
	}
	if _, err := tx.Exec(ctx, `UPDATE replay_batch_items SET result=$3,replay_attempt_id=$4,completed_at=$5 WHERE batch_id=$1 AND position=$2`, batchID, position, result, replayID, now); err != nil {
		return false, err
	}
	if _, err := tx.Exec(ctx, `UPDATE replay_batches SET started_at=coalesce(started_at,$2) WHERE id=$1`, batchID, now); err != nil {
		return false, err
	}
	return false, tx.Commit(ctx)
}
