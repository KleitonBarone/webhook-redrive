package httpapi

import (
	"errors"
	"net/http"
	"strings"

	"github.com/KleitonBarone/webhook-redrive/internal/id"
	"github.com/KleitonBarone/webhook-redrive/internal/store"
	"github.com/KleitonBarone/webhook-redrive/internal/telemetry"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
)

func validID(w http.ResponseWriter, value string) bool {
	if !id.Valid(value) {
		writeError(w, http.StatusBadRequest, "ID must be a lowercase UUID")
		return false
	}
	return true
}

func (a *API) replay(w http.ResponseWriter, request *http.Request) {
	ctx := request.Context()
	span := trace.SpanFromContext(ctx)
	eventID := request.PathValue("eventID")
	if !validID(w, eventID) {
		return
	}
	var input store.ReplayRequest
	if err := decodeJSON(w, request, &input); err != nil {
		writeError(w, http.StatusBadRequest, "invalid replay request")
		return
	}
	if !validID(w, input.AttemptID) || !validID(w, input.RequestID) {
		return
	}
	input.Actor, input.Reason = strings.TrimSpace(input.Actor), strings.TrimSpace(input.Reason)
	if len(input.Actor) < 1 || len(input.Actor) > 100 || len(input.Reason) < 1 || len(input.Reason) > 500 {
		writeError(w, http.StatusBadRequest, "actor must be 1..100 bytes and reason 1..500 bytes")
		return
	}
	result, err := a.store.Replay(request.Context(), eventID, input, a.clock.Now())
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeError(w, http.StatusNotFound, "event not found")
	case errors.Is(err, store.ErrConflict):
		writeError(w, http.StatusConflict, "replay requires the current failed or dead-letter attempt and a consistent request ID")
	case err != nil:
		a.internalError(w, request, "replay event", err)
	default:
		span.SetAttributes(attribute.String("event.id", eventID), attribute.String("attempt.id", result.AttemptID))
		a.logger.InfoContext(ctx, "replay accepted", "event_id", eventID, "attempt_id", result.AttemptID, "trace_id", telemetry.TraceID(ctx))
		writeJSON(w, http.StatusAccepted, result)
	}
}

func (a *API) deadLetters(w http.ResponseWriter, request *http.Request) {
	after := request.URL.Query().Get("after")
	if after != "" && !validID(w, after) {
		return
	}
	events, err := a.store.ListDeadLetters(request.Context(), after)
	if err != nil {
		a.internalError(w, request, "list dead letters", err)
		return
	}
	next := ""
	if len(events) == 100 {
		next = events[len(events)-1].ID
	}
	writeJSON(w, http.StatusOK, struct {
		Events    []store.Event `json:"events"`
		NextAfter string        `json:"next_after,omitempty"`
	}{events, next})
}
