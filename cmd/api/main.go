package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/KleitonBarone/webhook-redrive/internal/clock"
	"github.com/KleitonBarone/webhook-redrive/internal/config"
	"github.com/KleitonBarone/webhook-redrive/internal/httpapi"
	"github.com/KleitonBarone/webhook-redrive/internal/logsafe"
	"github.com/KleitonBarone/webhook-redrive/internal/secret"
	"github.com/KleitonBarone/webhook-redrive/internal/store"
)

func main() {
	logger := slog.New(logsafe.New(slog.NewJSONHandler(os.Stdout, nil)))
	if err := run(logger); err != nil {
		logger.Error("API stopped", "error", err)
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

	server := &http.Server{
		Addr:              config.String("API_ADDR", ":8080"),
		Handler:           httpapi.New(dataStore, box, serviceClock, logger),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      10 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdownCtx)
	}()
	logger.Info("API listening", "address", server.Addr)
	if err := server.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}
