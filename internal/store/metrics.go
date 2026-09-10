package store

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

var LatencyBuckets = []float64{0.005, 0.025, 0.1, 0.5, 1, 2, 5, 10, 30, 60}

type Histogram struct {
	Count   uint64
	Sum     float64
	Buckets map[float64]uint64
}

type MetricsSnapshot struct {
	Events             int64
	Claims             int64
	Recoveries         int64
	Retries            int64
	Replays            int64
	Queue              map[string]int64
	States             map[string]int64
	Completed          map[string]int64
	OldestReadySeconds float64
	Latency            map[string]Histogram
}

// Metrics reads one consistent committed snapshot. Its cost grows with retained
// history; the HTTP caller supplies a deadline and never serves stale zeroes.
func (s *Store) Metrics(ctx context.Context, now time.Time) (MetricsSnapshot, error) {
	m := MetricsSnapshot{Queue: map[string]int64{}, States: map[string]int64{}, Completed: map[string]int64{}, Latency: map[string]Histogram{}}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return m, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var ready, scheduled, active, expired int64
	err = tx.QueryRow(ctx, `SELECT count(*) FILTER (WHERE state='pending' AND available_at <= $1),
        count(*) FILTER (WHERE state='pending' AND available_at > $1),
        count(*) FILTER (WHERE state='in_progress' AND lease_until > $1),
        count(*) FILTER (WHERE state='in_progress' AND lease_until <= $1),
        coalesce(sum(claim_count),0), coalesce(sum(greatest(claim_count-1,0)),0),
        count(*) FILTER (WHERE cycle_attempt > 1), count(*) FILTER (WHERE replay_of IS NOT NULL),
        coalesce(max(greatest(0,extract(epoch FROM $1::timestamptz -
            CASE WHEN state='in_progress' THEN lease_until ELSE available_at END))) FILTER
            (WHERE (state='pending' AND available_at <= $1) OR (state='in_progress' AND lease_until <= $1)),0)
        FROM delivery_attempts`, now).Scan(&ready, &scheduled, &active, &expired,
		&m.Claims, &m.Recoveries, &m.Retries, &m.Replays, &m.OldestReadySeconds)
	if err != nil {
		return m, err
	}
	m.Queue = map[string]int64{"ready": ready, "scheduled": scheduled, "in_progress": active, "expired": expired}
	rows, err := tx.Query(ctx, `SELECT state,count(*) FROM (
        SELECT DISTINCT ON (event_id) state FROM delivery_attempts ORDER BY event_id,attempt_number DESC
        ) latest GROUP BY state`)
	if err != nil {
		return m, err
	}
	for rows.Next() {
		var state string
		var count int64
		if err := rows.Scan(&state, &count); err != nil {
			rows.Close()
			return m, err
		}
		m.States[state] = count
		m.Events += count
	}
	if err := rows.Err(); err != nil {
		return m, err
	}
	rows.Close()
	rows, err = tx.Query(ctx, `SELECT state,count(*) FROM delivery_attempts WHERE completed_at IS NOT NULL GROUP BY state`)
	if err != nil {
		return m, err
	}
	for rows.Next() {
		var state string
		var count int64
		if err := rows.Scan(&state, &count); err != nil {
			rows.Close()
			return m, err
		}
		m.Completed[state] = count
	}
	if err := rows.Err(); err != nil {
		return m, err
	}
	rows.Close()
	// Fixed boundaries, not caller input. Aggregate in PostgreSQL rather than
	// copying every historical attempt into the API process on each scrape.
	var filters []string
	for _, boundary := range LatencyBuckets {
		filters = append(filters, fmt.Sprintf("count(*) FILTER (WHERE seconds <= %g)", boundary))
	}
	rows, err = tx.Query(ctx, `SELECT kind,count(*),coalesce(sum(seconds),0),ARRAY[`+strings.Join(filters, ",")+`] FROM (
        SELECT 'attempt' AS kind, greatest(0,extract(epoch FROM completed_at-last_started_at)) AS seconds
        FROM delivery_attempts WHERE completed_at IS NOT NULL AND last_started_at IS NOT NULL
        UNION ALL
        SELECT 'queue',greatest(0,extract(epoch FROM last_started_at-available_at))
        FROM delivery_attempts WHERE completed_at IS NOT NULL AND last_started_at IS NOT NULL
        ) durations GROUP BY kind`)
	if err != nil {
		return m, err
	}
	for rows.Next() {
		var kind string
		var h Histogram
		var buckets []int64
		if err := rows.Scan(&kind, &h.Count, &h.Sum, &buckets); err != nil {
			rows.Close()
			return m, err
		}
		h.Buckets = map[float64]uint64{}
		for i, value := range buckets {
			h.Buckets[LatencyBuckets[i]] = uint64(value)
		}
		m.Latency[kind] = h
	}
	if err := rows.Err(); err != nil {
		return m, err
	}
	rows.Close()
	return m, tx.Commit(ctx)
}
