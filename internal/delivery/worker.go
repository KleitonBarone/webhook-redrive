package delivery

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/KleitonBarone/webhook-redrive/internal/clock"
	"github.com/KleitonBarone/webhook-redrive/internal/retry"
	"github.com/KleitonBarone/webhook-redrive/internal/secret"
	"github.com/KleitonBarone/webhook-redrive/internal/signature"
	"github.com/KleitonBarone/webhook-redrive/internal/store"
	"github.com/KleitonBarone/webhook-redrive/internal/telemetry"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
	"go.opentelemetry.io/otel/trace/noop"
)

type attemptStore interface {
	ClaimAvailable(context.Context, string, time.Time, time.Duration, int) ([]store.ClaimedDelivery, error)
	Complete(context.Context, store.ClaimedDelivery, string, time.Time, store.Outcome) (bool, error)
}

type Worker struct {
	tracer     trace.Tracer
	jitter     func() float64
	store      attemptStore
	box        *secret.Box
	client     *http.Client
	clock      clock.Clock
	logger     *slog.Logger
	workerID   string
	lease      time.Duration
	batchSize  int
	pollPeriod time.Duration
}

type Config struct {
	Tracer         trace.Tracer
	Jitter         func() float64
	WorkerID       string
	Lease          time.Duration
	BatchSize      int
	PollPeriod     time.Duration
	RequestTimeout time.Duration
}

func NewWorker(dataStore attemptStore, box *secret.Box, serviceClock clock.Clock, logger *slog.Logger, config Config) (*Worker, error) {
	if config.WorkerID == "" {
		return nil, errors.New("worker id is required")
	}
	if config.RequestTimeout <= 0 {
		return nil, errors.New("request timeout must be positive")
	}
	if config.Lease <= config.RequestTimeout {
		return nil, errors.New("lease must be longer than request timeout")
	}
	if config.BatchSize <= 0 || config.BatchSize > 1000 {
		return nil, errors.New("batch size must be between 1 and 1000")
	}
	if config.PollPeriod <= 0 {
		return nil, errors.New("poll period must be positive")
	}
	if config.Jitter == nil {
		config.Jitter = rand.Float64
	}
	if config.Tracer == nil {
		config.Tracer = noop.NewTracerProvider().Tracer("worker")
	}
	return &Worker{
		tracer: config.Tracer,
		jitter: config.Jitter,
		store:  dataStore, box: box, client: &http.Client{
			Timeout:       config.RequestTimeout,
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
		clock: serviceClock, logger: logger, workerID: config.WorkerID,
		lease: config.Lease, batchSize: config.BatchSize, pollPeriod: config.PollPeriod,
	}, nil
}

func (w *Worker) Run(ctx context.Context) error {
	ticker := time.NewTicker(w.pollPeriod)
	defer ticker.Stop()
	for {
		if _, err := w.RunOnce(ctx); err != nil {
			w.logger.ErrorContext(ctx, "worker poll failed", "error_type", fmt.Sprintf("%T", err))
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

// RunOnce starts every attempt in a claimed batch immediately. The lease only
// needs to bound one outbound timeout, not the sum of a sequential batch.
func (w *Worker) RunOnce(ctx context.Context) (int, error) {
	claimed, err := w.store.ClaimAvailable(ctx, w.workerID, w.clock.Now(), w.lease, w.batchSize)
	if err != nil {
		return 0, err
	}
	errorsByAttempt := make(chan error, len(claimed))
	var deliveries sync.WaitGroup
	for _, attempt := range claimed {
		deliveries.Add(1)
		go func() {
			defer deliveries.Done()
			if err := w.deliver(ctx, attempt); err != nil {
				errorsByAttempt <- err
			}
		}()
	}
	deliveries.Wait()
	close(errorsByAttempt)
	var runErrors []error
	for err := range errorsByAttempt {
		runErrors = append(runErrors, err)
	}
	return len(claimed), errors.Join(runErrors...)
}

func (w *Worker) deliver(ctx context.Context, attempt store.ClaimedDelivery) (result error) {
	now := w.clock.Now()
	ctx = telemetry.Extract(ctx, attempt.TraceParent)
	queuedAt := attempt.QueuedAt
	if queuedAt.IsZero() || queuedAt.After(now) {
		queuedAt = now
	}
	ctx, queued := w.tracer.Start(ctx, "webhook.queue", trace.WithTimestamp(queuedAt),
		trace.WithAttributes(attribute.String("event.id", attempt.EventID), attribute.String("attempt.id", attempt.AttemptID),
			attribute.Int("claim.generation", attempt.ClaimCount),
			attribute.Float64("queue.ready_wait_seconds", max(0, now.Sub(attempt.AvailableAt).Seconds()))))
	queued.End(trace.WithTimestamp(now))
	ctx, span := w.tracer.Start(ctx, "webhook.deliver", trace.WithTimestamp(now), trace.WithSpanKind(trace.SpanKindClient),
		trace.WithAttributes(attribute.String("event.id", attempt.EventID), attribute.String("attempt.id", attempt.AttemptID),
			attribute.Int("claim.generation", attempt.ClaimCount)))
	defer func() {
		if result != nil {
			span.SetStatus(codes.Error, "delivery incomplete")
		}
		span.End(trace.WithTimestamp(w.clock.Now()))
	}()
	if err := ctx.Err(); err != nil {
		return err
	}
	// A delayed goroutine must not dispatch after its claim has expired.
	remaining := attempt.LeaseUntil.Sub(w.clock.Now())
	if remaining <= 0 {
		return errors.New("attempt lease expired before dispatch")
	}
	sendCtx, cancel := context.WithTimeout(ctx, remaining)
	defer cancel()
	outcome := store.Outcome{}
	endpointSecret, err := w.box.Decrypt(attempt.SecretCiphertext)
	if err != nil {
		outcome.Code, outcome.Message = "secret_decryption", "endpoint secret could not be decrypted"
		return w.finish(ctx, attempt, outcome)
	}
	sentAt := w.clock.Now()
	request, err := http.NewRequestWithContext(sendCtx, http.MethodPost, attempt.EndpointURL, bytes.NewReader(attempt.Payload))
	if err != nil {
		outcome.Code, outcome.Message = "invalid_endpoint", "endpoint URL could not be used"
		return w.finish(ctx, attempt, outcome)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("User-Agent", "webhook-redrive/0.3")
	telemetry.Inject(ctx, request.Header)
	request.Header.Set("X-Webhook-Event", attempt.EventType)
	request.Header.Set("X-Webhook-ID", attempt.EventID)
	request.Header.Set("X-Webhook-Timestamp", strconv.FormatInt(sentAt.Unix(), 10))
	request.Header.Set("X-Webhook-Signature", signature.Sign(endpointSecret, sentAt, attempt.Payload))
	response, err := w.client.Do(request)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		outcome.Code, outcome.Message = "request_error", "outbound request failed"
		if errors.Is(err, context.DeadlineExceeded) {
			outcome.Code, outcome.Message = "timeout", "outbound request timed out"
		}
		if retry.Transport(err) {
			outcome.RetryAt = w.retryAt(attempt, "")
		}
	} else {
		// Response bodies are not part of the acknowledgement contract.
		// Closing immediately avoids waiting on an unbounded response stream.
		_ = response.Body.Close()
		outcome.Status = response.StatusCode
		if response.StatusCode < 200 || response.StatusCode >= 300 {
			outcome.Code, outcome.Message = "http_status", fmt.Sprintf("endpoint returned HTTP %d", response.StatusCode)
			if retry.Status(response.StatusCode) {
				outcome.RetryAt = w.retryAt(attempt, response.Header.Get("Retry-After"))
			}
		}
	}
	return w.finish(ctx, attempt, outcome)
}

func (w *Worker) retryAt(attempt store.ClaimedDelivery, retryAfter string) *time.Time {
	now := w.clock.Now()
	due := now.Add(max(retry.Delay(attempt.CycleAttempt, w.jitter()), retry.After(retryAfter, now)))
	return &due
}

func (w *Worker) finish(ctx context.Context, attempt store.ClaimedDelivery, outcome store.Outcome) error {
	span := trace.SpanFromContext(ctx)
	span.SetAttributes(attribute.Int("http.response.status_code", outcome.Status),
		attribute.String("error.type", outcome.Code), attribute.Bool("retry.eligible", outcome.RetryAt != nil))
	if outcome.Code != "" {
		span.SetStatus(codes.Error, outcome.Code)
	}
	updated, err := w.store.Complete(ctx, attempt, w.workerID, w.clock.Now(), outcome)
	if err != nil {
		return err
	}
	if !updated {
		return errors.New("attempt lease was lost before completion could be recorded")
	}
	w.logger.InfoContext(ctx, "delivery completed",
		"trace_id", telemetry.TraceID(ctx),
		"event_id", attempt.EventID, "attempt_id", attempt.AttemptID,
		"claim_count", attempt.ClaimCount, "response_status", outcome.Status, "error_code", outcome.Code)
	return nil
}
