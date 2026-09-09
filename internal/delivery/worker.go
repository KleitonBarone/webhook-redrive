package delivery

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/KleitonBarone/webhook-redrive/internal/clock"
	"github.com/KleitonBarone/webhook-redrive/internal/secret"
	"github.com/KleitonBarone/webhook-redrive/internal/signature"
	"github.com/KleitonBarone/webhook-redrive/internal/store"
)

const maxResponseDrain = 4 << 10

type attemptStore interface {
	ClaimAvailable(context.Context, string, time.Time, time.Duration, int) ([]store.ClaimedDelivery, error)
	CompleteSuccess(context.Context, string, string, time.Time, int) (bool, error)
	CompleteFailure(context.Context, string, string, time.Time, int, string, string) (bool, error)
}

type Worker struct {
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
	if config.BatchSize <= 0 {
		return nil, errors.New("batch size must be positive")
	}
	if config.PollPeriod <= 0 {
		return nil, errors.New("poll period must be positive")
	}
	return &Worker{
		store: dataStore, box: box, client: &http.Client{Timeout: config.RequestTimeout},
		clock: serviceClock, logger: logger, workerID: config.WorkerID,
		lease: config.Lease, batchSize: config.BatchSize, pollPeriod: config.PollPeriod,
	}, nil
}

func (w *Worker) Run(ctx context.Context) error {
	ticker := time.NewTicker(w.pollPeriod)
	defer ticker.Stop()
	for {
		if _, err := w.RunOnce(ctx); err != nil {
			w.logger.ErrorContext(ctx, "worker poll failed", "error", err)
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

func (w *Worker) deliver(ctx context.Context, attempt store.ClaimedDelivery) error {
	endpointSecret, err := w.box.Decrypt(attempt.SecretCiphertext)
	if err != nil {
		return w.fail(ctx, attempt, 0, "secret_decryption", "endpoint secret could not be decrypted")
	}
	sentAt := w.clock.Now()
	timestamp := strconv.FormatInt(sentAt.Unix(), 10)
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, attempt.EndpointURL, bytes.NewReader(attempt.Payload))
	if err != nil {
		return w.fail(ctx, attempt, 0, "invalid_endpoint", "endpoint URL could not be used")
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("User-Agent", "webhook-redrive/0.1")
	request.Header.Set("X-Webhook-Event", attempt.EventType)
	request.Header.Set("X-Webhook-ID", attempt.EventID)
	request.Header.Set("X-Webhook-Timestamp", timestamp)
	request.Header.Set("X-Webhook-Signature", signature.Sign(endpointSecret, sentAt, attempt.Payload))

	response, err := w.client.Do(request)
	if err != nil {
		code, message := transportFailure(err)
		return w.fail(ctx, attempt, 0, code, message)
	}
	_, _ = io.CopyN(io.Discard, response.Body, maxResponseDrain)
	_ = response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return w.fail(ctx, attempt, response.StatusCode, "http_status", fmt.Sprintf("endpoint returned HTTP %d", response.StatusCode))
	}
	updated, err := w.store.CompleteSuccess(ctx, attempt.AttemptID, w.workerID, w.clock.Now(), response.StatusCode)
	if err != nil {
		return err
	}
	if !updated {
		return errors.New("attempt lease was lost before success could be recorded")
	}
	w.logger.InfoContext(ctx, "delivery succeeded",
		"event_id", attempt.EventID,
		"attempt_id", attempt.AttemptID,
		"claim_count", attempt.ClaimCount,
		"response_status", response.StatusCode,
	)
	return nil
}

func (w *Worker) fail(ctx context.Context, attempt store.ClaimedDelivery, status int, code, message string) error {
	updated, err := w.store.CompleteFailure(ctx, attempt.AttemptID, w.workerID, w.clock.Now(), status, code, message)
	if err != nil {
		return err
	}
	if !updated {
		return errors.New("attempt lease was lost before failure could be recorded")
	}
	w.logger.WarnContext(ctx, "delivery failed",
		"event_id", attempt.EventID,
		"attempt_id", attempt.AttemptID,
		"claim_count", attempt.ClaimCount,
		"error_code", code,
		"response_status", status,
	)
	return nil
}

func transportFailure(err error) (string, string) {
	if errors.Is(err, context.DeadlineExceeded) {
		return "timeout", "outbound request timed out"
	}
	return "request_error", "outbound request failed"
}
