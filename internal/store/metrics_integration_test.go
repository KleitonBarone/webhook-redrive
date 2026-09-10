package store

import (
	"context"
	"testing"
	"time"
)

func TestMetricsTrackCommittedStateAndRecovery(t *testing.T) {
	s := integrationStore(t)
	ctx := context.Background()
	endpoint := insertTestEndpoint(t, s)
	eventID := seed(t, s, endpoint)
	m, err := s.Metrics(ctx, testNow.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if m.Events != 1 || m.Queue["ready"] != 1 || m.OldestReadySeconds != 1 || m.Claims != 0 {
		t.Fatalf("pending: %+v", m)
	}
	first := claimOne(t, s, testNow.Add(time.Second))
	m, err = s.Metrics(ctx, testNow.Add(61*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if m.Queue["expired"] != 1 || m.Queue["in_progress"] != 0 || m.Claims != 1 {
		t.Fatalf("expired: %+v", m)
	}
	claim := claimOne(t, s, testNow.Add(61*time.Second))
	if ok, err := s.Complete(ctx, first, "worker", testNow.Add(61*time.Second), Outcome{Status: 204}); err != nil || ok {
		t.Fatalf("stale: %v %v", ok, err)
	}
	due := testNow.Add(65 * time.Second)
	finish(t, s, claim, testNow.Add(62*time.Second), Outcome{Status: 503, Code: "http_status", RetryAt: &due})
	m, err = s.Metrics(ctx, testNow.Add(62*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if m.Retries != 1 || m.Recoveries != 1 || m.Claims != 2 || m.Queue["scheduled"] != 1 || m.Completed["failed"] != 1 {
		t.Fatalf("retry: %+v", m)
	}
	if h := m.Latency["attempt"]; h.Count != 1 || h.Sum != 1 || h.Buckets[0.5] != 0 || h.Buckets[1] != 1 {
		t.Fatalf("latency: %+v", h)
	}
	last := claimOne(t, s, due)
	finish(t, s, last, due.Add(500*time.Millisecond), Outcome{Status: 204})
	m, err = s.Metrics(ctx, due.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if m.States["succeeded"] != 1 || m.Completed["succeeded"] != 1 || m.Claims != 3 || m.Latency["attempt"].Sum != 1.5 {
		t.Fatalf("success: %+v", m)
	}
	// Reopening the pool models a process restart; counters are database-derived.
	history, err := s.ListAttempts(ctx, eventID)
	if err != nil || len(history) != 2 {
		t.Fatalf("history: %v %v", history, err)
	}
	other, err := Open(ctx, s.pool.Config().ConnString())
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	after, err := other.Metrics(ctx, due.Add(time.Second))
	if err != nil || after.Claims != m.Claims || after.Retries != m.Retries {
		t.Fatalf("restart: %+v %v", after, err)
	}
}
