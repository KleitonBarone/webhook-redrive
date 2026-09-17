package store

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

func TestKeyedIngestionAtomicityAndRecovery(t *testing.T) {
	s := integrationStore(t)
	ctx := context.Background()
	p := accessPrincipal(t, s)
	endpoint := insertTestEndpoint(t, s)
	e := Event{ID: mustID(t), EndpointID: endpoint, EventType: "order.created", CreatedAt: testNow}
	for _, failure := range []string{"attempt", "event", "key"} {
		t.Run(failure, func(t *testing.T) {
			table := map[string]string{"attempt": "delivery_attempts", "event": "events", "key": "ingestion_keys"}[failure]
			if _, err := s.pool.Exec(ctx, "ALTER TABLE "+table+" ADD CONSTRAINT reject_insert CHECK(false)"); err != nil {
				t.Fatal(err)
			}
			if _, err := s.IngestEvent(ctx, e, []byte(`{"amount":1}`), mustID(t), p.ID, "atomic-key"); err == nil {
				t.Fatal("expected insertion failure")
			}
			for _, table := range []string{"events", "delivery_attempts", "ingestion_keys"} {
				var n int
				if err := s.pool.QueryRow(ctx, "SELECT count(*) FROM "+table).Scan(&n); err != nil || n != 0 {
					t.Fatalf("partial commit in %s: %d %v", table, n, err)
				}
			}
			if _, err := s.pool.Exec(ctx, "ALTER TABLE "+table+" DROP CONSTRAINT reject_insert"); err != nil {
				t.Fatal(err)
			}
		})
	}
	first, err := s.IngestEvent(ctx, e, []byte(`{"amount":1}`), mustID(t), p.ID, "atomic-key")
	if err != nil || first.Repeated {
		t.Fatalf("first: %+v %v", first, err)
	}
	claims, err := s.ClaimAvailable(ctx, "worker", testNow, time.Minute, 1)
	if err != nil || len(claims) != 1 {
		t.Fatal("missing attempt")
	}
	if ok, err := s.Complete(ctx, claims[0], "worker", testNow, Outcome{Status: 204}); err != nil || !ok {
		t.Fatal("completion failed")
	}
	// Lost acknowledgement + process restart, after delivery has already finished.
	fresh, err := Open(ctx, s.pool.Config().ConnString())
	if err != nil {
		t.Fatal(err)
	}
	defer fresh.Close()
	e.ID = mustID(t)
	e.CreatedAt = testNow.AddDate(2, 0, 0)
	got, err := fresh.IngestEvent(ctx, e, []byte(`{"amount":1}`), mustID(t), p.ID, "atomic-key")
	if err != nil || !got.Repeated || got.Event != first.Event || got.AttemptID != first.AttemptID {
		t.Fatalf("lost acknowledgement: %+v %v", got, err)
	}
	for _, body := range []string{`{"amount":2}`, `{ "amount":1}`} {
		if _, err := s.IngestEvent(ctx, e, []byte(body), mustID(t), p.ID, "atomic-key"); !errors.Is(err, ErrIdempotencyConflict) {
			t.Fatal("changed bytes accepted")
		}
	}
	e.EventType = "order.updated"
	if _, err := s.IngestEvent(ctx, e, []byte(`{"amount":1}`), mustID(t), p.ID, "atomic-key"); !errors.Is(err, ErrIdempotencyConflict) {
		t.Fatal("changed event type accepted")
	}
}

func TestConcurrentKeyedIngestionAndScopes(t *testing.T) {
	s := integrationStore(t)
	ctx := context.Background()
	p := accessPrincipal(t, s)
	endpoint := insertTestEndpoint(t, s)
	const total = 20
	start := make(chan struct{})
	results := make(chan IngestionReceipt, total)
	errs := make(chan error, total)
	var wg sync.WaitGroup
	for i := 0; i < total; i++ {
		e := Event{ID: mustID(t), EndpointID: endpoint, EventType: "test", CreatedAt: testNow}
		attempt := mustID(t)
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			result, err := s.IngestEvent(ctx, e, []byte(`{}`), attempt, p.ID, "same-key")
			results <- result
			errs <- err
		}()
	}
	close(start)
	wg.Wait()
	close(results)
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	var original string
	created := 0
	for r := range results {
		if original == "" {
			original = r.Event.ID
		}
		if original != r.Event.ID {
			t.Fatal("concurrent duplicate event")
		}
		if !r.Repeated {
			created++
		}
	}
	if created != 1 {
		t.Fatalf("new events %d", created)
	}
	other := accessPrincipal(t, s)
	for _, scope := range []struct{ principal, endpoint, key string }{{other.ID, endpoint, "same-key"}, {p.ID, insertTestEndpoint(t, s), "same-key"}, {p.ID, endpoint, "another-key"}, {p.ID, endpoint, ""}, {p.ID, endpoint, ""}} {
		r, err := s.IngestEvent(ctx, Event{ID: mustID(t), EndpointID: scope.endpoint, EventType: "test", CreatedAt: testNow}, []byte(`{}`), mustID(t), scope.principal, scope.key)
		if err != nil || r.Repeated || r.Event.ID == original {
			t.Fatalf("scope: %+v %v", r, err)
		}
	}
	for _, table := range []string{"events", "delivery_attempts"} {
		var n int
		if err := s.pool.QueryRow(ctx, "SELECT count(*) FROM "+table).Scan(&n); err != nil || n != 6 {
			t.Fatalf("%s count %d %v", table, n, err)
		}
	}
}
