package delivery

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/KleitonBarone/webhook-redrive/internal/secret"
	"github.com/KleitonBarone/webhook-redrive/internal/signature"
	"github.com/KleitonBarone/webhook-redrive/internal/store"
)

type fixedClock struct{ now time.Time }

func (clock fixedClock) Now() time.Time { return clock.now }

type fakeAttemptStore struct {
	mu               sync.Mutex
	delivery         store.ClaimedDelivery
	claims           int
	successes        int
	failures         int
	failureCode      string
	loseFirstSuccess bool
	lastOutcome      store.Outcome
}

func (s *fakeAttemptStore) ClaimAvailable(_ context.Context, _ string, now time.Time, lease time.Duration, _ int) ([]store.ClaimedDelivery, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.claims++
	delivery := s.delivery
	delivery.ClaimCount = s.claims
	delivery.LeaseUntil = now.Add(lease)
	delivery.CycleAttempt = 1
	return []store.ClaimedDelivery{delivery}, nil
}

func (s *fakeAttemptStore) Complete(_ context.Context, _ store.ClaimedDelivery, _ string, _ time.Time, outcome store.Outcome) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lastOutcome = outcome
	if outcome.Code != "" {
		s.failures++
		s.failureCode = outcome.Code
		return true, nil
	}
	s.successes++
	if s.loseFirstSuccess && s.successes == 1 {
		return false, nil
	}
	return true, nil
}

func TestDuplicateDeliveryAfterLostCompletionIsSignedOverExactBytes(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, time.August, 27, 12, 0, 0, 0, time.UTC)
	payload := []byte("{\n \"order\": 42\n}\n")
	endpointSecret := []byte("synthetic-secret-32-bytes-long")
	var mu sync.Mutex
	received := 0
	receiver := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		body, err := io.ReadAll(request.Body)
		if err != nil {
			t.Errorf("read body: %v", err)
			return
		}
		if string(body) != string(payload) {
			t.Errorf("body changed in transit: %q", body)
		}
		if err := signature.Verify(
			endpointSecret,
			request.Header.Get("X-Webhook-Timestamp"),
			body,
			request.Header.Get("X-Webhook-Signature"),
			now,
			time.Minute,
		); err != nil {
			t.Errorf("verify signature: %v", err)
		}
		mu.Lock()
		received++
		mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	}))
	defer receiver.Close()

	box, _ := secret.NewBox(make([]byte, secret.KeySize))
	ciphertext, _ := box.Encrypt(endpointSecret)
	dataStore := &fakeAttemptStore{delivery: store.ClaimedDelivery{
		AttemptID: "attempt", EventID: "event", EventType: "order.created",
		Payload: payload, EndpointURL: receiver.URL, SecretCiphertext: ciphertext,
	}, loseFirstSuccess: true}
	worker := newTestWorker(t, dataStore, box, fixedClock{now: now}, time.Second)

	// The fake returns the same attempt twice. This models a worker sending the
	// first request, crashing before its completion commit, and a later reclaim.
	if _, err := worker.RunOnce(context.Background()); err == nil {
		t.Fatal("lost first completion should leave the attempt reclaimable")
	}
	if _, err := worker.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if received != 2 || dataStore.successes != 2 {
		t.Fatalf("received=%d successes=%d, want two of each", received, dataStore.successes)
	}
}

func TestOutboundRequestTimeoutIsRecorded(t *testing.T) {
	t.Parallel()
	receiver := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(200 * time.Millisecond)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer receiver.Close()

	box, _ := secret.NewBox(make([]byte, secret.KeySize))
	ciphertext, _ := box.Encrypt([]byte("synthetic-secret-32-bytes-long"))
	dataStore := &fakeAttemptStore{delivery: store.ClaimedDelivery{
		AttemptID: "attempt", EventID: "event", EventType: "test",
		Payload: []byte(`{"ok":true}`), EndpointURL: receiver.URL, SecretCiphertext: ciphertext,
	}}
	worker := newTestWorker(t, dataStore, box, fixedClock{now: time.Unix(100, 0)}, 20*time.Millisecond)
	if _, err := worker.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if dataStore.failures != 1 || dataStore.failureCode != "timeout" {
		t.Fatalf("failures=%d code=%q, want one timeout", dataStore.failures, dataStore.failureCode)
	}
	if dataStore.lastOutcome.RetryAt == nil || !dataStore.lastOutcome.RetryAt.Equal(time.Unix(100, 0).Add(500*time.Millisecond)) {
		t.Fatalf("timeout must schedule a retry: %+v", dataStore.lastOutcome)
	}
}

func newTestWorker(t *testing.T, dataStore attemptStore, box *secret.Box, serviceClock fixedClock, timeout time.Duration) *Worker {
	t.Helper()
	worker, err := NewWorker(dataStore, box, serviceClock, slog.New(slog.NewTextHandler(io.Discard, nil)), Config{
		WorkerID: "test-worker",
		Jitter:   func() float64 { return 0 },
		Lease:    timeout + time.Second, BatchSize: 1, PollPeriod: time.Millisecond, RequestTimeout: timeout,
	})
	if err != nil {
		t.Fatal(err)
	}
	return worker
}
