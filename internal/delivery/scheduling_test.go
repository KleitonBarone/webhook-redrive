package delivery

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/KleitonBarone/webhook-redrive/internal/secret"
	"github.com/KleitonBarone/webhook-redrive/internal/store"
)

type schedulingStore struct {
	mu                        sync.Mutex
	pending                   []store.ClaimedDelivery
	active, peak, completions int
	claims                    chan int
	claimError                error
}

func (s *schedulingStore) ClaimAvailable(ctx context.Context, _ string, now time.Time, lease time.Duration, limit int) ([]store.ClaimedDelivery, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	select {
	case s.claims <- limit:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	if s.claimError != nil {
		return nil, s.claimError
	}
	n := min(limit, len(s.pending))
	batch := append([]store.ClaimedDelivery(nil), s.pending[:n]...)
	s.pending = s.pending[n:]
	for i := range batch {
		batch[i].LeaseUntil = now.Add(lease)
	}
	s.active += n
	s.peak = max(s.peak, s.active)
	return batch, nil
}

func (s *schedulingStore) Complete(context.Context, store.ClaimedDelivery, string, time.Time, store.Outcome) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.active--
	s.completions++
	return true, nil
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestRunRefillsFreeSlotsWhileSlowDeliveryIsBlocked(t *testing.T) {
	box, err := secret.NewBox(make([]byte, secret.KeySize))
	if err != nil {
		t.Fatal(err)
	}
	ciphertext, err := box.Encrypt([]byte("synthetic-scheduler-secret"))
	if err != nil {
		t.Fatal(err)
	}
	s := &schedulingStore{claims: make(chan int, 100)}
	for _, name := range []string{"slow", "fast-one", "fast-two"} {
		s.pending = append(s.pending, store.ClaimedDelivery{AttemptID: name, EndpointURL: "http://synthetic.test/" + name, SecretCiphertext: ciphertext, Payload: []byte("{}")})
	}
	w, err := NewWorker(s, box, fixedClock{now: time.Unix(100, 0)}, slog.New(slog.NewTextHandler(io.Discard, nil)), Config{WorkerID: "test", Lease: time.Minute, RequestTimeout: 30 * time.Second, BatchSize: 2, PollPeriod: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	slowStarted, fastFinished := make(chan struct{}), make(chan struct{})
	w.client.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path == "/slow" {
			close(slowStarted)
			<-r.Context().Done()
			return nil, r.Context().Err()
		}
		<-slowStarted
		if r.URL.Path == "/fast-two" {
			close(fastFinished)
		}
		return &http.Response{StatusCode: 204, Body: io.NopCloser(strings.NewReader("")), Header: make(http.Header)}, nil
	})
	ctx, cancel := context.WithCancel(context.Background())
	stopped := make(chan error, 1)
	go func() { stopped <- w.Run(ctx) }()
	defer func() {
		cancel()
		select {
		case err := <-stopped:
			if !errors.Is(err, context.Canceled) {
				t.Errorf("shutdown=%v", err)
			}
		case <-time.After(3 * time.Second):
			t.Error("worker did not stop")
		}
	}()
	select {
	case <-fastFinished:
	case <-time.After(3 * time.Second):
		t.Fatal("free slot was not refilled while the slow delivery remained blocked")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.peak > 2 {
		t.Fatalf("worker exceeded its two slots: %d", s.peak)
	}
}

func TestRunPollsEmptyQueueAndClaimErrorsOnlyOnTicks(t *testing.T) {
	for _, claimError := range []error{nil, errors.New("synthetic database failure")} {
		t.Run(fmt.Sprint(claimError), func(t *testing.T) {
			s := &schedulingStore{claims: make(chan int), claimError: claimError}
			w, err := NewWorker(s, nil, fixedClock{now: time.Unix(100, 0)}, slog.New(slog.NewTextHandler(io.Discard, nil)), Config{WorkerID: "test", Lease: time.Minute, RequestTimeout: time.Second, BatchSize: 2, PollPeriod: time.Hour})
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			ticks := make(chan time.Time)
			stopped := make(chan error, 1)
			go func() { stopped <- w.run(ctx, ticks) }()
			defer cancel()
			for i := 0; i < 3; i++ {
				select {
				case limit := <-s.claims:
					if limit != 2 {
						t.Errorf("claim limit=%d", limit)
					}
				case <-time.After(3 * time.Second):
					t.Fatal("missing scheduled claim")
				}
				if i == 2 {
					break
				}
				select {
				case ticks <- time.Unix(int64(i+101), 0):
				case <-s.claims:
					t.Fatal("queried again without a poll tick")
				case <-time.After(3 * time.Second):
					t.Fatal("worker did not wait for a poll tick")
				}
			}
			cancel()
			select {
			case err := <-stopped:
				if !errors.Is(err, context.Canceled) {
					t.Errorf("shutdown=%v", err)
				}
			case <-s.claims:
				t.Fatal("queried without a tick during shutdown")
			case <-time.After(3 * time.Second):
				t.Fatal("worker did not stop")
			}
		})
	}
}

func TestRunAlreadyCanceledDoesNotClaim(t *testing.T) {
	s := &schedulingStore{claims: make(chan int, 1)}
	w, err := NewWorker(s, nil, fixedClock{}, slog.New(slog.NewTextHandler(io.Discard, nil)), Config{WorkerID: "test", Lease: time.Minute, RequestTimeout: time.Second, BatchSize: 2, PollPeriod: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := w.Run(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("shutdown=%v", err)
	}
	if len(s.claims) != 0 {
		t.Fatal("canceled worker claimed work")
	}
}

func TestRunSaturatedWorkerDoesNotPreclaimAndWaitsForShutdown(t *testing.T) {
	box, err := secret.NewBox(make([]byte, secret.KeySize))
	if err != nil {
		t.Fatal(err)
	}
	ciphertext, err := box.Encrypt([]byte("synthetic-scheduler-secret"))
	if err != nil {
		t.Fatal(err)
	}
	s := &schedulingStore{claims: make(chan int, 100)}
	for i := 0; i < 3; i++ {
		s.pending = append(s.pending, store.ClaimedDelivery{EndpointURL: "http://synthetic.test/slow", SecretCiphertext: ciphertext})
	}
	w, err := NewWorker(s, box, fixedClock{now: time.Unix(100, 0)}, slog.New(slog.NewTextHandler(io.Discard, nil)), Config{WorkerID: "test", Lease: time.Minute, RequestTimeout: 30 * time.Second, BatchSize: 2, PollPeriod: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	started, exited := make(chan struct{}, 2), make(chan struct{}, 2)
	w.client.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		started <- struct{}{}
		<-r.Context().Done()
		exited <- struct{}{}
		return nil, r.Context().Err()
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ticks := make(chan time.Time)
	stopped := make(chan error, 1)
	go func() { stopped <- w.run(ctx, ticks) }()
	for i := 0; i < 2; i++ {
		select {
		case <-started:
		case <-time.After(3 * time.Second):
			t.Fatal("delivery did not start")
		}
	}
	for i := 0; i < 3; i++ {
		select {
		case ticks <- time.Unix(101, 0):
		case <-time.After(3 * time.Second):
			t.Fatal("worker did not process tick while saturated")
		}
	}
	cancel()
	select {
	case err := <-stopped:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("shutdown=%v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("worker did not stop")
	}
	if len(exited) != 2 || len(s.claims) != 1 || len(s.pending) != 1 || s.completions != 0 {
		t.Fatalf("exited=%d claims=%d pending=%d completions=%d", len(exited), len(s.claims), len(s.pending), s.completions)
	}
}
