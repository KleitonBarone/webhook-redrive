package main

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/KleitonBarone/webhook-redrive/internal/clock"
	"github.com/KleitonBarone/webhook-redrive/internal/config"
	"github.com/KleitonBarone/webhook-redrive/internal/delivery"
	"github.com/KleitonBarone/webhook-redrive/internal/id"
	"github.com/KleitonBarone/webhook-redrive/internal/logsafe"
	"github.com/KleitonBarone/webhook-redrive/internal/secret"
	"github.com/KleitonBarone/webhook-redrive/internal/store"
)

func main() {
	logger := slog.New(logsafe.New(slog.NewJSONHandler(os.Stdout, nil)))
	if err := run(logger); err != nil {
		logger.Error("worker stopped", "error", err)
		os.Exit(1)
	}
}

func run(logger *slog.Logger) error {
	databaseURL, err := config.Required("DATABASE_URL")
	if err != nil {
		return err
	}
	masterKey, err := config.Required("MASTER_KEY")
	if err != nil {
		return err
	}
	box, err := secret.NewBoxFromBase64(masterKey)
	if err != nil {
		return err
	}
	requestTimeout, err := config.Duration("OUTBOUND_TIMEOUT", 2*time.Second)
	if err != nil {
		return err
	}
	lease, err := config.Duration("CLAIM_LEASE", 10*time.Second)
	if err != nil {
		return err
	}
	pollPeriod, err := config.Duration("POLL_PERIOD", 250*time.Millisecond)
	if err != nil {
		return err
	}
	batchSize, err := config.Int("BATCH_SIZE", 10)
	if err != nil {
		return err
	}
	workerID := config.String("WORKER_ID", "")
	if workerID == "" {
		workerID, err = id.New()
		if err != nil {
			return err
		}
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	dataStore, err := store.Open(ctx, databaseURL)
	if err != nil {
		return err
	}
	defer dataStore.Close()
	serviceClock := clock.Real{}
	if err := dataStore.Migrate(ctx, serviceClock.Now()); err != nil {
		return err
	}
	worker, err := delivery.NewWorker(dataStore, box, serviceClock, logger, delivery.Config{
		WorkerID: workerID, Lease: lease, BatchSize: batchSize,
		PollPeriod: pollPeriod, RequestTimeout: requestTimeout,
	})
	if err != nil {
		return err
	}
	logger.Info("worker started", "worker_id", workerID)
	if err := worker.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
		return err
	}
	return nil
}
