package store

import (
	"context"
	"github.com/jackc/pgx/v5"
)

// ListDeadLetters returns only events whose latest attempt exhausted its budget.
func (s *Store) ListDeadLetters(ctx context.Context, after string) ([]Event, error) {
	rows, err := s.pool.Query(ctx, `SELECT e.id,e.endpoint_id,e.event_type,a.state,e.created_at
        FROM events e JOIN LATERAL (SELECT state FROM delivery_attempts WHERE event_id=e.id
            ORDER BY attempt_number DESC LIMIT 1) a ON true
        WHERE a.state='dead_letter' AND e.id::text > $1 ORDER BY e.id LIMIT 100`, after)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (Event, error) {
		var e Event
		err := row.Scan(&e.ID, &e.EndpointID, &e.EventType, &e.State, &e.CreatedAt)
		return e, err
	})
}
