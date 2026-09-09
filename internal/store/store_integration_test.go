package store

import (
	"context"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/KleitonBarone/webhook-redrive/internal/id"
)

func TestAtomicEventAndAttemptPersistence(t *testing.T) {
	dataStore := integrationStore(t)
	endpointID := insertTestEndpoint(t, dataStore)
	eventID := mustID(t)
	now := time.Date(2026, time.August, 27, 12, 0, 0, 0, time.UTC)
	event := Event{ID: eventID, EndpointID: endpointID, EventType: "test", CreatedAt: now}

	if err := dataStore.CreateEvent(context.Background(), event, []byte(`{"atomic":true}`), "not-a-uuid"); err == nil {
		t.Fatal("invalid attempt id should abort ingestion")
	}
	var eventCount, attemptCount int
	if err := dataStore.pool.QueryRow(context.Background(), `SELECT count(*) FROM events WHERE id = $1`, eventID).Scan(&eventCount); err != nil {
		t.Fatal(err)
	}
	if err := dataStore.pool.QueryRow(context.Background(), `SELECT count(*) FROM delivery_attempts WHERE event_id = $1`, eventID).Scan(&attemptCount); err != nil {
		t.Fatal(err)
	}
	if eventCount != 0 || attemptCount != 0 {
		t.Fatalf("event_count=%d attempt_count=%d, want atomic rollback", eventCount, attemptCount)
	}

	if err := dataStore.CreateEvent(context.Background(), event, []byte(`{"atomic":true}`), mustID(t)); err != nil {
		t.Fatal(err)
	}
	if err := dataStore.pool.QueryRow(context.Background(), `SELECT count(*) FROM events WHERE id = $1`, eventID).Scan(&eventCount); err != nil {
		t.Fatal(err)
	}
	if err := dataStore.pool.QueryRow(context.Background(), `SELECT count(*) FROM delivery_attempts WHERE event_id = $1`, eventID).Scan(&attemptCount); err != nil {
		t.Fatal(err)
	}
	if eventCount != 1 || attemptCount != 1 {
		t.Fatalf("event_count=%d attempt_count=%d, want one of each", eventCount, attemptCount)
	}
}

func TestConcurrentWorkersClaimEachAttemptOnce(t *testing.T) {
	dataStore := integrationStore(t)
	endpointID := insertTestEndpoint(t, dataStore)
	now := time.Date(2026, time.August, 27, 12, 0, 0, 0, time.UTC)
	const attemptTotal = 24
	for index := 0; index < attemptTotal; index++ {
		event := Event{ID: mustID(t), EndpointID: endpointID, EventType: "test", CreatedAt: now}
		if err := dataStore.CreateEvent(context.Background(), event, []byte(`{"claim":true}`), mustID(t)); err != nil {
			t.Fatal(err)
		}
	}

	start := make(chan struct{})
	results := make(chan []ClaimedDelivery, attemptTotal)
	errors := make(chan error, attemptTotal)
	var workers sync.WaitGroup
	for index := 0; index < attemptTotal; index++ {
		workers.Add(1)
		go func(worker int) {
			defer workers.Done()
			<-start
			claimed, err := dataStore.ClaimAvailable(context.Background(), mustWorkerID(worker), now, time.Minute, 1)
			if err != nil {
				errors <- err
				return
			}
			results <- claimed
		}(index)
	}
	close(start)
	workers.Wait()
	close(results)
	close(errors)
	for err := range errors {
		t.Fatal(err)
	}
	seen := make(map[string]bool, attemptTotal)
	for claimed := range results {
		if len(claimed) > 1 {
			t.Fatalf("worker claimed %d attempts with limit 1", len(claimed))
		}
		if len(claimed) == 0 {
			continue
		}
		recordClaim(t, seen, claimed[0])
	}
	// SKIP LOCKED may return an empty batch while every visible candidate is
	// locked by another transaction. Drain after the concurrent statements end.
	remainder, err := dataStore.ClaimAvailable(context.Background(), "drain", now, time.Minute, attemptTotal)
	if err != nil {
		t.Fatal(err)
	}
	for _, claimed := range remainder {
		recordClaim(t, seen, claimed)
	}
	if len(seen) != attemptTotal {
		t.Fatalf("claimed %d attempts, want %d", len(seen), attemptTotal)
	}
}

func recordClaim(t *testing.T, seen map[string]bool, claimed ClaimedDelivery) {
	t.Helper()
	if seen[claimed.AttemptID] {
		t.Fatalf("attempt %s was claimed twice", claimed.AttemptID)
	}
	if claimed.ClaimCount != 1 {
		t.Fatalf("attempt %s claim count=%d, want 1", claimed.AttemptID, claimed.ClaimCount)
	}
	seen[claimed.AttemptID] = true
}

func TestExpiredClaimIsRecoveredAfterWorkerCrash(t *testing.T) {
	dataStore := integrationStore(t)
	endpointID := insertTestEndpoint(t, dataStore)
	now := time.Date(2026, time.August, 27, 12, 0, 0, 0, time.UTC)
	event := Event{ID: mustID(t), EndpointID: endpointID, EventType: "test", CreatedAt: now}
	if err := dataStore.CreateEvent(context.Background(), event, []byte(`{"crash":true}`), mustID(t)); err != nil {
		t.Fatal(err)
	}

	first, err := dataStore.ClaimAvailable(context.Background(), "worker-before-crash", now, time.Minute, 1)
	if err != nil || len(first) != 1 || first[0].ClaimCount != 1 {
		t.Fatalf("first claim=%+v err=%v", first, err)
	}
	tooEarly, err := dataStore.ClaimAvailable(context.Background(), "worker-too-early", now.Add(59*time.Second), time.Minute, 1)
	if err != nil || len(tooEarly) != 0 {
		t.Fatalf("claim before lease expiry=%+v err=%v", tooEarly, err)
	}
	recovered, err := dataStore.ClaimAvailable(context.Background(), "worker-after-crash", now.Add(time.Minute), time.Minute, 1)
	if err != nil || len(recovered) != 1 || recovered[0].AttemptID != first[0].AttemptID || recovered[0].ClaimCount != 2 {
		t.Fatalf("recovered claim=%+v err=%v", recovered, err)
	}
}

func integrationStore(t *testing.T) *Store {
	t.Helper()
	databaseURL := os.Getenv("TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("TEST_DATABASE_URL is not set")
	}
	dataStore, err := Open(context.Background(), databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(dataStore.Close)
	if err := dataStore.Migrate(context.Background(), time.Unix(1, 0)); err != nil {
		t.Fatal(err)
	}
	if _, err := dataStore.pool.Exec(context.Background(), `TRUNCATE delivery_attempts, events, webhook_endpoints`); err != nil {
		t.Fatal(err)
	}
	return dataStore
}

func insertTestEndpoint(t *testing.T, dataStore *Store) string {
	t.Helper()
	endpointID := mustID(t)
	if err := dataStore.CreateEndpoint(context.Background(), Endpoint{
		ID: endpointID, URL: "http://receiver.test/success", CreatedAt: time.Unix(1, 0),
	}, []byte("synthetic-ciphertext")); err != nil {
		t.Fatal(err)
	}
	return endpointID
}

func mustID(t *testing.T) string {
	t.Helper()
	value, err := id.New()
	if err != nil {
		t.Fatal(err)
	}
	return value
}

func mustWorkerID(index int) string {
	return time.Unix(int64(index+1), 0).Format(time.RFC3339Nano)
}
