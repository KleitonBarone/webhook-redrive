package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"sync"
	"time"

	"github.com/KleitonBarone/webhook-redrive/internal/clock"
	"github.com/KleitonBarone/webhook-redrive/internal/config"
	"github.com/KleitonBarone/webhook-redrive/internal/logsafe"
	"github.com/KleitonBarone/webhook-redrive/internal/signature"
	"github.com/KleitonBarone/webhook-redrive/internal/telemetry"
)

const maxReceiverBody = 1 << 20

type receivedDelivery struct {
	TraceID        string    `json:"trace_id,omitempty"`
	ResponseStatus int       `json:"response_status"`
	EventID        string    `json:"event_id"`
	EventType      string    `json:"event_type"`
	BodySHA256     string    `json:"body_sha256"`
	SignatureValid bool      `json:"signature_valid"`
	ReceivedAt     time.Time `json:"received_at"`
}

type receiver struct {
	mu          sync.Mutex
	deliveries  []receivedDelivery
	secret      []byte
	tolerance   time.Duration
	delay       time.Duration
	clock       clock.Clock
	logger      *slog.Logger
	eventCounts map[string]int
}

func main() {
	logger := slog.New(logsafe.New(slog.NewJSONHandler(os.Stdout, nil)))
	delay, err := config.Duration("RECEIVER_DELAY", 3*time.Second)
	if err != nil {
		logger.Error("invalid configuration", "error", err)
		os.Exit(1)
	}
	r := &receiver{
		secret:    []byte(config.String("RECEIVER_SECRET", "local-demo-secret-32-bytes-long")),
		tolerance: 5 * time.Minute, delay: delay, clock: clock.Real{}, logger: logger,
	}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /success", r.deliver(http.StatusNoContent, 0))
	mux.HandleFunc("POST /fail", r.deliver(http.StatusInternalServerError, 0))
	mux.HandleFunc("POST /timeout", r.deliver(http.StatusNoContent, r.delay))
	mux.HandleFunc("POST /reject", r.deliver(http.StatusBadRequest, 0))
	mux.HandleFunc("POST /rate-limit", r.deliver(http.StatusTooManyRequests, 0))
	mux.HandleFunc("POST /flaky", r.deliver(http.StatusInternalServerError, 0))
	mux.HandleFunc("GET /deliveries", r.list)
	server := &http.Server{
		Addr: config.String("RECEIVER_ADDR", ":9090"), Handler: mux,
		ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 10 * time.Second,
		WriteTimeout: 10 * time.Second, IdleTimeout: 60 * time.Second,
	}
	logger.Info("synthetic receiver listening", "address", server.Addr)
	if err := server.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
		logger.Error("receiver stopped", "error", err)
		os.Exit(1)
	}
}

func (r *receiver) deliver(status int, delay time.Duration) http.HandlerFunc {
	return func(w http.ResponseWriter, request *http.Request) {
		ctx := telemetry.Extract(request.Context(), request.Header.Get("traceparent"))
		responseStatus := status
		failures := 2
		if request.URL.Path == "/flaky" && request.URL.Query().Has("failures") {
			parsed, err := strconv.Atoi(request.URL.Query().Get("failures"))
			if err != nil || parsed < 0 || parsed > 20 {
				http.Error(w, "failures must be 0..20", http.StatusBadRequest)
				return
			}
			failures = parsed
		}
		body, err := io.ReadAll(http.MaxBytesReader(w, request.Body, maxReceiverBody))
		if err != nil {
			http.Error(w, "invalid body", http.StatusBadRequest)
			return
		}
		now := r.clock.Now()
		verificationErr := signature.Verify(
			r.secret,
			request.Header.Get("X-Webhook-Timestamp"),
			body,
			request.Header.Get("X-Webhook-Signature"),
			now,
			r.tolerance,
		)
		digest := sha256.Sum256(body)
		delivery := receivedDelivery{
			TraceID:        telemetry.TraceID(ctx),
			EventID:        request.Header.Get("X-Webhook-ID"),
			EventType:      request.Header.Get("X-Webhook-Event"),
			BodySHA256:     hex.EncodeToString(digest[:]),
			SignatureValid: verificationErr == nil,
			ReceivedAt:     now,
		}
		r.mu.Lock()
		if r.eventCounts == nil {
			r.eventCounts = make(map[string]int)
		}
		if verificationErr == nil {
			key := request.URL.Path + "/" + delivery.EventID
			r.eventCounts[key]++
			if request.URL.Path == "/flaky" && r.eventCounts[key] > failures {
				responseStatus = http.StatusNoContent
			}
		} else {
			responseStatus = http.StatusUnauthorized
		}
		delivery.ResponseStatus = responseStatus
		r.deliveries = append(r.deliveries, delivery)
		count := len(r.deliveries)
		r.mu.Unlock()
		r.logger.InfoContext(request.Context(), "synthetic delivery received",
			"trace_id", delivery.TraceID,
			"event_id", delivery.EventID,
			"signature_valid", delivery.SignatureValid,
			"delivery_count", count,
		)
		if verificationErr != nil {
			http.Error(w, "invalid signature", http.StatusUnauthorized)
			return
		}
		if delay > 0 {
			timer := time.NewTimer(delay)
			defer timer.Stop()
			select {
			case <-request.Context().Done():
				return
			case <-timer.C:
			}
		}
		w.Header().Set("X-Synthetic-Delivery-Count", strconv.Itoa(count))
		if responseStatus == http.StatusTooManyRequests {
			w.Header().Set("Retry-After", "1")
		}
		w.WriteHeader(responseStatus)
	}
}

func (r *receiver) list(w http.ResponseWriter, _ *http.Request) {
	r.mu.Lock()
	deliveries := append([]receivedDelivery(nil), r.deliveries...)
	r.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"count":      len(deliveries),
		"deliveries": deliveries,
	})
}
