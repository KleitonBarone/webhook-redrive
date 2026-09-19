package store

import (
	"context"
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
	WorkersRecent              int64
	WorkerPollAgeSeconds       float64
	WorkerCompletionAgeSeconds float64
	DatabaseBytes              int64
	Events                     int64
	Claims                     int64
	Recoveries                 int64
	Retries                    int64
	Replays                    int64
	Queue                      map[string]int64
	States                     map[string]int64
	Completed                  map[string]int64
	OldestReadySeconds         float64
	Latency                    map[string]Histogram
}

// Metrics reads transactional cumulative totals and retained-state gauges from
// one snapshot. The HTTP caller supplies a deadline, never stale zeroes.
func (s *Store) Metrics(ctx context.Context, now time.Time) (MetricsSnapshot, error) {
	m := MetricsSnapshot{Queue: map[string]int64{}, States: map[string]int64{}, Completed: map[string]int64{}, Latency: map[string]Histogram{}}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return m, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var ready, scheduled, active, expired, paused int64
	err = tx.QueryRow(ctx, `SELECT count(*) FILTER (WHERE NOT e.paused AND state='pending' AND available_at <= $1),
        count(*) FILTER (WHERE NOT e.paused AND state='pending' AND available_at > $1),
        count(*) FILTER (WHERE state='in_progress' AND lease_until > $1),
        count(*) FILTER (WHERE NOT e.paused AND state='in_progress' AND lease_until <= $1),
        coalesce(max(greatest(0,extract(epoch FROM $1::timestamptz -
            CASE WHEN state='in_progress' THEN lease_until ELSE available_at END))) FILTER
            (WHERE NOT e.paused AND ((state='pending' AND available_at <= $1) OR (state='in_progress' AND lease_until <= $1))),0),
        count(*) FILTER (WHERE e.paused AND (state='pending' OR (state='in_progress' AND lease_until <= $1)))
        FROM delivery_attempts a JOIN webhook_endpoints e ON e.id=a.endpoint_id WHERE a.state IN ('pending','in_progress')`, now).Scan(&ready, &scheduled, &active, &expired,
		&m.OldestReadySeconds, &paused)
	if err != nil {
		return m, err
	}
	m.Queue = map[string]int64{"ready": ready, "scheduled": scheduled, "in_progress": active, "expired": expired, "paused": paused}
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
	}
	if err := rows.Err(); err != nil {
		return m, err
	}
	rows.Close()
	var succeeded, failed, dead int64
	var count uint64
	var attemptSum, queueSum float64
	var attemptBuckets, queueBuckets []int64
	err = tx.QueryRow(ctx, `SELECT events,claims,recoveries,retries,replays,succeeded,failed,dead_letter,latency_count,attempt_sum,queue_sum,attempt_buckets,queue_buckets FROM cumulative_metrics`).Scan(&m.Events, &m.Claims, &m.Recoveries, &m.Retries, &m.Replays, &succeeded, &failed, &dead, &count, &attemptSum, &queueSum, &attemptBuckets, &queueBuckets)
	if err != nil {
		return m, err
	}
	m.Completed = map[string]int64{"succeeded": succeeded, "failed": failed, "dead_letter": dead}
	for _, sample := range []struct {
		kind    string
		sum     float64
		buckets []int64
	}{{"attempt", attemptSum, attemptBuckets}, {"queue", queueSum, queueBuckets}} {
		h := Histogram{Count: count, Sum: sample.sum, Buckets: map[float64]uint64{}}
		for i, value := range sample.buckets {
			h.Buckets[LatencyBuckets[i]] = uint64(value)
		}
		m.Latency[sample.kind] = h
	}
	err = tx.QueryRow(ctx, `SELECT count(*) FILTER(WHERE polled_at > $1::timestamptz-interval '1 minute'),
	coalesce(greatest(0,extract(epoch FROM $1::timestamptz-max(polled_at))),0),
	coalesce(greatest(0,extract(epoch FROM $1::timestamptz-max(completed_at))),0),pg_database_size(current_database()) FROM worker_progress`, now).Scan(&m.WorkersRecent, &m.WorkerPollAgeSeconds, &m.WorkerCompletionAgeSeconds, &m.DatabaseBytes)
	if err != nil {
		return m, err
	}
	return m, tx.Commit(ctx)
}
