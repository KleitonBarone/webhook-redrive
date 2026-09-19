package store

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

type RetentionResult struct {
	Events    int64     `json:"events"`
	Batches   int64     `json:"batches"`
	AuditRows int64     `json:"audit_rows"`
	Applied   bool      `json:"applied"`
	Cutoff    time.Time `json:"cutoff"`
}

const oldEvent = `NOT EXISTS(SELECT 1 FROM delivery_attempts a WHERE a.event_id=e.id AND (a.completed_at IS NULL OR a.completed_at >= $1))
 AND EXISTS(SELECT 1 FROM delivery_attempts a WHERE a.event_id=e.id)
 AND NOT EXISTS(SELECT 1 FROM replay_batch_items i WHERE i.event_id=e.id)`
const oldBatch = `b.created_at < $1 AND coalesce(b.started_at,b.created_at) < $1
 AND NOT EXISTS(SELECT 1 FROM replay_batch_items i WHERE i.batch_id=b.id AND i.completed_at >= $1)`

// Retain deletes bounded groups in separate transactions. One event's complete
// history is indivisible; the caller's context bounds unusually large histories.
// Counts are committed progress (or candidate counts when apply=false).
func (s *Store) Retain(ctx context.Context, now time.Time, days, limit int, apply bool) (RetentionResult, error) {
	r := RetentionResult{Applied: apply, Cutoff: now.Add(-time.Duration(days) * 24 * time.Hour)}
	if days < 1 || days > 3650 || limit < 1 || limit > 100 {
		return r, errors.New("invalid retention bounds")
	}
	for _, kind := range []string{"batch", "event"} {
		query := `SELECT b.id FROM replay_batches b WHERE ` + oldBatch + ` ORDER BY b.created_at,b.id LIMIT $2`
		if kind == "event" {
			query = `SELECT e.id FROM events e WHERE ` + oldEvent + ` ORDER BY e.created_at,e.id LIMIT $2`
		}
		rows, err := s.pool.Query(ctx, query, r.Cutoff, limit)
		if err != nil {
			return r, err
		}
		var ids []string
		for rows.Next() {
			var id string
			if err = rows.Scan(&id); err != nil {
				rows.Close()
				return r, err
			}
			ids = append(ids, id)
		}
		rows.Close()
		if err = rows.Err(); err != nil {
			return r, err
		}
		for _, id := range ids {
			affected := int64(1)
			if apply {
				affected, err = s.retainOne(ctx, kind, id, r.Cutoff, now)
				if err != nil {
					return r, err
				}
			}
			if kind == "batch" {
				r.Batches += affected
			} else {
				r.Events += affected
			}
		}
	}
	// Independent audit is retained for the same duration, in bounded chunks.
	for _, table := range []string{"endpoint_audit", "credential_audit", "maintenance_audit", "worker_progress"} {
		timestamp := "created_at"
		if table == "worker_progress" {
			timestamp = "polled_at"
		}
		// Only fixed table/column names above are interpolated.
		query := `SELECT count(*) FROM (SELECT 1 FROM ` + table + ` WHERE ` + timestamp + ` < $1 LIMIT $2) old`
		var n int64
		if apply {
			tag, err := s.pool.Exec(ctx, `DELETE FROM `+table+` WHERE ctid IN (SELECT ctid FROM `+table+` WHERE `+timestamp+` < $1 ORDER BY `+timestamp+` LIMIT $2)`, r.Cutoff, limit)
			if err != nil {
				return r, err
			}
			n = tag.RowsAffected()
		} else {
			if err := s.pool.QueryRow(ctx, query, r.Cutoff, limit).Scan(&n); err != nil {
				return r, err
			}
		}
		r.AuditRows += n
	}
	return r, nil
}

func (s *Store) retainOne(ctx context.Context, kind, id string, cutoff, now time.Time) (int64, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	table := "events"
	if kind == "batch" {
		table = "replay_batches"
	}
	// The same row serializes replay/completion or batch execution. Eligibility
	// must use a new READ COMMITTED statement after obtaining this lock.
	var locked string
	err = tx.QueryRow(ctx, `SELECT id FROM `+table+` WHERE id=$1 FOR UPDATE SKIP LOCKED`, id).Scan(&locked)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	query := `SELECT EXISTS(SELECT 1 FROM events e WHERE e.id=$2 AND ` + oldEvent + `)`
	if kind == "batch" {
		query = `SELECT EXISTS(SELECT 1 FROM replay_batches b WHERE b.id=$2 AND ` + oldBatch + `)`
	}
	var eligible bool
	if err = tx.QueryRow(ctx, query, cutoff, id).Scan(&eligible); err != nil {
		return 0, err
	}
	if !eligible {
		return 0, nil
	}
	if kind == "batch" {
		if _, err = tx.Exec(ctx, `DELETE FROM replay_batch_items WHERE batch_id=$1`, id); err != nil {
			return 0, err
		}
	} else {
		if _, err = tx.Exec(ctx, `DELETE FROM ingestion_keys WHERE event_id=$1`, id); err != nil {
			return 0, err
		}
		if _, err = tx.Exec(ctx, `DELETE FROM delivery_attempts WHERE event_id=$1`, id); err != nil {
			return 0, err
		}
	}
	if _, err = tx.Exec(ctx, `DELETE FROM `+table+` WHERE id=$1`, id); err != nil {
		return 0, err
	}
	if _, err = tx.Exec(ctx, `INSERT INTO maintenance_audit(action,affected,created_at) VALUES('retention',1,$1)`, now); err != nil {
		return 0, err
	}
	return 1, tx.Commit(ctx)
}
