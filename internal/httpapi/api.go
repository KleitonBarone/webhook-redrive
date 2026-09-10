package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/KleitonBarone/webhook-redrive/internal/clock"
	"github.com/KleitonBarone/webhook-redrive/internal/id"
	"github.com/KleitonBarone/webhook-redrive/internal/secret"
	"github.com/KleitonBarone/webhook-redrive/internal/store"
)

const (
	maxJSONBody    = 1 << 20
	minSecretBytes = 16
)

type dataStore interface {
	CreateEndpoint(context.Context, store.Endpoint, []byte) error
	EndpointExists(context.Context, string) (bool, error)
	CreateEvent(context.Context, store.Event, []byte, string) error
	GetEvent(context.Context, string) (store.Event, error)
	ListAttempts(context.Context, string) ([]store.Attempt, error)
	Replay(context.Context, string, store.ReplayRequest, time.Time) (store.ReplayResult, error)
	ListDeadLetters(context.Context, string) ([]store.Event, error)
}

type API struct {
	store  dataStore
	box    *secret.Box
	clock  clock.Clock
	logger *slog.Logger
}

func New(dataStore dataStore, box *secret.Box, serviceClock clock.Clock, logger *slog.Logger) http.Handler {
	api := &API{store: dataStore, box: box, clock: serviceClock, logger: logger}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", api.health)
	mux.HandleFunc("POST /v1/endpoints", api.createEndpoint)
	mux.HandleFunc("POST /v1/endpoints/{endpointID}/events", api.createEvent)
	mux.HandleFunc("GET /v1/events/{eventID}", api.getEvent)
	mux.HandleFunc("GET /v1/events/{eventID}/attempts", api.listAttempts)
	mux.HandleFunc("POST /v1/events/{eventID}/replays", api.replay)
	mux.HandleFunc("GET /v1/dead-letters", api.deadLetters)
	return api.logRequests(mux)
}

func (a *API) health(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (a *API) createEndpoint(w http.ResponseWriter, request *http.Request) {
	input := struct {
		URL              string `json:"url"`
		Secret           string `json:"secret"`
		MaxAttempts      int    `json:"max_attempts"`
		ConcurrencyLimit int    `json:"concurrency_limit"`
		RateLimit        int    `json:"rate_limit"`
	}{MaxAttempts: 5, ConcurrencyLimit: 2, RateLimit: 10}
	if err := decodeJSON(w, request, &input); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := validateEndpointURL(input.URL); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if len(input.Secret) < minSecretBytes {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("secret must be at least %d bytes", minSecretBytes))
		return
	}
	if input.MaxAttempts < 1 || input.MaxAttempts > 20 || input.ConcurrencyLimit < 1 || input.ConcurrencyLimit > 100 || input.RateLimit < 1 || input.RateLimit > 1000 {
		writeError(w, http.StatusBadRequest, "max_attempts must be 1..20, concurrency_limit 1..100, and rate_limit 1..1000")
		return
	}

	ciphertext, err := a.box.Encrypt([]byte(input.Secret))
	if err != nil {
		a.internalError(w, request, "encrypt endpoint secret", err)
		return
	}
	endpointID, err := id.New()
	if err != nil {
		a.internalError(w, request, "generate endpoint id", err)
		return
	}
	endpoint := store.Endpoint{ID: endpointID, URL: input.URL, CreatedAt: a.clock.Now(),
		MaxAttempts: input.MaxAttempts, ConcurrencyLimit: input.ConcurrencyLimit, RateLimit: input.RateLimit}
	if err := a.store.CreateEndpoint(request.Context(), endpoint, ciphertext); err != nil {
		a.internalError(w, request, "create endpoint", err)
		return
	}
	writeJSON(w, http.StatusCreated, endpoint)
}

func (a *API) createEvent(w http.ResponseWriter, request *http.Request) {
	endpointID := request.PathValue("endpointID")
	if !validID(w, endpointID) {
		return
	}
	eventType := strings.TrimSpace(request.Header.Get("X-Event-Type"))
	if eventType == "" || len(eventType) > 100 {
		writeError(w, http.StatusBadRequest, "X-Event-Type must contain 1 to 100 characters")
		return
	}
	payload, err := readPayload(w, request)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	exists, err := a.store.EndpointExists(request.Context(), endpointID)
	if err != nil {
		a.internalError(w, request, "check endpoint", err)
		return
	}
	if !exists {
		writeError(w, http.StatusNotFound, "endpoint not found")
		return
	}
	eventID, err := id.New()
	if err != nil {
		a.internalError(w, request, "generate event id", err)
		return
	}
	attemptID, err := id.New()
	if err != nil {
		a.internalError(w, request, "generate attempt id", err)
		return
	}
	event := store.Event{
		ID: eventID, EndpointID: endpointID, EventType: eventType,
		State: "pending", CreatedAt: a.clock.Now(),
	}
	if err := a.store.CreateEvent(request.Context(), event, payload, attemptID); err != nil {
		a.internalError(w, request, "create event", err)
		return
	}
	writeJSON(w, http.StatusAccepted, event)
}

func (a *API) getEvent(w http.ResponseWriter, request *http.Request) {
	if !validID(w, request.PathValue("eventID")) {
		return
	}
	event, err := a.store.GetEvent(request.Context(), request.PathValue("eventID"))
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "event not found")
		return
	}
	if err != nil {
		a.internalError(w, request, "get event", err)
		return
	}
	writeJSON(w, http.StatusOK, event)
}

func (a *API) listAttempts(w http.ResponseWriter, request *http.Request) {
	if !validID(w, request.PathValue("eventID")) {
		return
	}
	attempts, err := a.store.ListAttempts(request.Context(), request.PathValue("eventID"))
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "event not found")
		return
	}
	if err != nil {
		a.internalError(w, request, "list attempts", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string][]store.Attempt{"attempts": attempts})
}

func (a *API) internalError(w http.ResponseWriter, request *http.Request, operation string, err error) {
	a.logger.ErrorContext(request.Context(), operation, "error_type", fmt.Sprintf("%T", err))
	writeError(w, http.StatusInternalServerError, "internal server error")
}

func (a *API) logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/healthz" {
			next.ServeHTTP(w, request)
			return
		}
		started := time.Now()
		response := &statusWriter{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(response, request)
		a.logger.InfoContext(request.Context(), "http request",
			"method", request.Method,
			"route", request.Pattern,
			"status", response.status,
			"duration_ms", time.Since(started).Milliseconds(),
		)
	})
}

type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(status int) {
	w.status = status
	w.ResponseWriter.WriteHeader(status)
}

func validateEndpointURL(raw string) error {
	parsed, err := url.ParseRequestURI(raw)
	if err != nil || parsed.Host == "" {
		return errors.New("url must be an absolute HTTP or HTTPS URL")
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return errors.New("url must use HTTP or HTTPS")
	}
	if parsed.User != nil {
		return errors.New("url must not contain credentials")
	}
	return nil
}

func decodeJSON(w http.ResponseWriter, request *http.Request, target any) error {
	request.Body = http.MaxBytesReader(w, request.Body, maxJSONBody)
	decoder := json.NewDecoder(request.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return fmt.Errorf("invalid JSON: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return errors.New("request body must contain one JSON value")
	}
	return nil
}

func readPayload(w http.ResponseWriter, request *http.Request) ([]byte, error) {
	request.Body = http.MaxBytesReader(w, request.Body, maxJSONBody)
	payload, err := io.ReadAll(request.Body)
	if err != nil {
		return nil, fmt.Errorf("read payload: %w", err)
	}
	if len(payload) == 0 || !json.Valid(payload) {
		return nil, errors.New("body must contain valid JSON")
	}
	return payload, nil
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSONStatus(w, status, map[string]string{"error": message})
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	writeJSONStatus(w, status, value)
}

func writeJSONStatus(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
