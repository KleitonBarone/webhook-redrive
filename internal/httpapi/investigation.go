package httpapi

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/KleitonBarone/webhook-redrive/internal/auth"
	"github.com/KleitonBarone/webhook-redrive/internal/store"
)

func (a *API) searchEvents(w http.ResponseWriter, r *http.Request) {
	q, err := parseSearch(r)
	if err != nil {
		writeError(w, 400, "invalid search filters or cursor")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	page, err := a.store.SearchEvents(ctx, q.filter, q.after, q.limit)
	if a.investigationError(w, r, err) {
		return
	}
	writeJSON(w, 200, page)
}

type searchQuery struct {
	filter store.EventFilter
	after  string
	limit  int
}

func parseSearch(r *http.Request) (searchQuery, error) {
	result := searchQuery{limit: 100}
	values, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		return result, err
	}
	for key, items := range values {
		if len(items) != 1 || items[0] == "" {
			return result, store.ErrInvalidSearch
		}
		value := items[0]
		switch key {
		case "endpoint_id":
			result.filter.EndpointID = value
		case "producer_reference":
			result.filter.ProducerReference = value
		case "state":
			result.filter.State = value
		case "after":
			result.after = value
		case "limit":
			n, err := strconv.Atoi(value)
			if err != nil || n < 1 || n > 100 {
				return result, store.ErrInvalidSearch
			}
			result.limit = n
		case "from", "until":
			at, err := time.Parse(time.RFC3339Nano, value)
			if err != nil {
				return result, store.ErrInvalidSearch
			}
			at = at.UTC()
			if key == "from" {
				result.filter.From = &at
			} else {
				result.filter.Until = &at
			}
		default:
			return result, store.ErrInvalidSearch
		}
	}
	return result, nil
}

func (a *API) previewBatch(w http.ResponseWriter, r *http.Request) {
	var input store.BatchRequest
	if decodeJSON(w, r, &input) != nil {
		writeError(w, 400, "invalid recovery selection")
		return
	}
	principal := auth.FromContext(r.Context())
	input.PrincipalID, input.Actor = principal.ID, principal.Name
	input.Reason = strings.TrimSpace(input.Reason)
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	batch, err := a.store.PreviewBatch(ctx, input, a.clock.Now())
	if a.investigationError(w, r, err) {
		return
	}
	writeJSON(w, 200, batch)
}
func (a *API) getBatch(w http.ResponseWriter, r *http.Request) {
	if !validID(w, r.PathValue("batchID")) {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	batch, err := a.store.GetBatch(ctx, r.PathValue("batchID"))
	if a.investigationError(w, r, err) {
		return
	}
	writeJSON(w, 200, batch)
}
func (a *API) runBatch(w http.ResponseWriter, r *http.Request) {
	if !validID(w, r.PathValue("batchID")) {
		return
	}
	var confirmation *struct{}
	if decodeJSON(w, r, &confirmation) != nil || confirmation == nil {
		writeError(w, 400, "confirmation body must be an empty object")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	batch, err := a.store.RunBatch(ctx, r.PathValue("batchID"), auth.FromContext(r.Context()).ID, a.clock.Now())
	if a.investigationError(w, r, err) {
		return
	}
	writeJSON(w, 200, batch)
}
func (a *API) investigationError(w http.ResponseWriter, r *http.Request, err error) bool {
	switch {
	case err == nil:
		return false
	case errors.Is(err, store.ErrInvalidBatch), errors.Is(err, store.ErrInvalidSearch):
		writeError(w, 400, err.Error())
	case errors.Is(err, store.ErrNotFound):
		writeError(w, 404, "selection or batch not found")
	case errors.Is(err, store.ErrConflict):
		writeError(w, 409, "batch identity or creator conflicts; inspect before retrying")
	default:
		a.internalError(w, r, "investigate or recover events", err)
	}
	return true
}
