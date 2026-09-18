package httpapi

import (
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/KleitonBarone/webhook-redrive/internal/auth"
	"github.com/KleitonBarone/webhook-redrive/internal/store"
)

type endpointSettings struct {
	URL              string `json:"url"`
	RetryProfile     string `json:"retry_profile"`
	MaxAttempts      *int   `json:"max_attempts"`
	ConcurrencyLimit *int   `json:"concurrency_limit"`
	RateLimit        *int   `json:"rate_limit"`
	RetryBaseSeconds *int   `json:"retry_base_seconds"`
	RetryCapSeconds  *int   `json:"retry_cap_seconds"`
	EventTTLSeconds  *int   `json:"event_ttl_seconds"`
}

func (a *API) endpointSettings(input endpointSettings) (store.Endpoint, error) {
	e := store.Endpoint{URL: input.URL, Version: 1, SigningVersion: 1, MaxAttempts: 5, ConcurrencyLimit: 2, RateLimit: 10, RetryBaseSeconds: 1, RetryCapSeconds: 60, EventTTLSeconds: 86400}
	switch input.RetryProfile {
	case "", "demo":
	case "outage":
		e.MaxAttempts, e.RetryBaseSeconds, e.RetryCapSeconds = 20, 300, 7200
	default:
		return e, errors.New("retry_profile must be demo or outage")
	}
	for _, p := range []struct {
		src *int
		dst *int
	}{
		{input.MaxAttempts, &e.MaxAttempts}, {input.ConcurrencyLimit, &e.ConcurrencyLimit}, {input.RateLimit, &e.RateLimit},
		{input.RetryBaseSeconds, &e.RetryBaseSeconds}, {input.RetryCapSeconds, &e.RetryCapSeconds}, {input.EventTTLSeconds, &e.EventTTLSeconds},
	} {
		if p.src != nil {
			*p.dst = *p.src
		}
	}
	if validateEndpointURL(e.URL) != nil || a.security.Destinations.Validate(e.URL) != nil {
		return e, errors.New("destination denied")
	}
	if e.MaxAttempts < 1 || e.MaxAttempts > 20 || e.ConcurrencyLimit < 1 || e.ConcurrencyLimit > 100 || e.RateLimit < 1 || e.RateLimit > 1000 {
		return e, errors.New("max_attempts must be 1..20, concurrency_limit 1..100, and rate_limit 1..1000")
	}
	if e.RetryBaseSeconds < 1 || e.RetryCapSeconds < e.RetryBaseSeconds || e.RetryCapSeconds > 86400 || e.EventTTLSeconds < 1 || e.EventTTLSeconds > 604800 {
		return e, errors.New("retry delays must satisfy 1 <= base <= cap <= 86400; event_ttl_seconds must be 1..604800")
	}
	return e, nil
}

func (a *API) getEndpoint(w http.ResponseWriter, r *http.Request) {
	endpointID := r.PathValue("endpointID")
	if !validID(w, endpointID) {
		return
	}
	e, err := a.store.GetEndpoint(r.Context(), endpointID)
	if a.endpointError(w, r, err) {
		return
	}
	writeJSON(w, 200, e)
}

func (a *API) listEndpoints(w http.ResponseWriter, r *http.Request) {
	after := r.URL.Query().Get("after")
	if after != "" && !validID(w, after) {
		return
	}
	endpoints, err := a.store.ListEndpoints(r.Context(), after)
	if a.endpointError(w, r, err) {
		return
	}
	next := ""
	if len(endpoints) == 100 {
		next = endpoints[len(endpoints)-1].ID
	}
	writeJSON(w, 200, struct {
		Endpoints []store.Endpoint `json:"endpoints"`
		Next      string           `json:"next_after,omitempty"`
	}{endpoints, next})
}

func (a *API) endpointAudit(w http.ResponseWriter, r *http.Request) {
	endpointID := r.PathValue("endpointID")
	if !validID(w, endpointID) {
		return
	}
	var after int64
	if value := r.URL.Query().Get("after"); value != "" {
		n, err := strconv.ParseInt(value, 10, 64)
		if err != nil || n < 0 {
			writeError(w, 400, "invalid audit cursor")
			return
		}
		after = n
	}
	if _, err := a.store.GetEndpoint(r.Context(), endpointID); a.endpointError(w, r, err) {
		return
	}
	entries, err := a.store.ListEndpointAudit(r.Context(), endpointID, after)
	if a.endpointError(w, r, err) {
		return
	}
	var next int64
	if len(entries) == 100 {
		next = entries[len(entries)-1].Version
	}
	writeJSON(w, 200, struct {
		Entries []store.EndpointAudit `json:"entries"`
		Next    int64                 `json:"next_after,omitempty"`
	}{entries, next})
}

type endpointPrecondition struct {
	ExpectedVersion int64  `json:"expected_version"`
	Reason          string `json:"reason"`
}

func (a *API) updateEndpoint(w http.ResponseWriter, r *http.Request) {
	var input struct {
		endpointSettings
		endpointPrecondition
	}
	if !validID(w, r.PathValue("endpointID")) {
		return
	}
	if decodeJSON(w, r, &input) != nil {
		writeError(w, 400, "invalid endpoint update")
		return
	}
	settings, err := a.endpointSettings(input.endpointSettings)
	if err != nil {
		writeError(w, 400, err.Error())
		return
	}
	a.changeEndpoint(w, r, input.endpointPrecondition, store.EndpointChange{Action: "updated", Settings: settings})
}

func (a *API) endpointAction(action string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !validID(w, r.PathValue("endpointID")) {
			return
		}
		var pre endpointPrecondition
		change := store.EndpointChange{Action: action}
		if action == "rotated" {
			var input struct {
				endpointPrecondition
				Secret         string `json:"secret"`
				OverlapSeconds int    `json:"overlap_seconds"`
			}
			if decodeJSON(w, r, &input) != nil || len(input.Secret) < minSecretBytes || input.OverlapSeconds < 300 || input.OverlapSeconds > 86400 {
				writeError(w, 400, "rotation requires a secret of at least 16 bytes and overlap_seconds of 300..86400")
				return
			}
			var err error
			change.Ciphertext, err = a.box.Encrypt([]byte(input.Secret))
			if err != nil {
				a.internalError(w, r, "encrypt endpoint secret", err)
				return
			}
			change.Overlap = time.Duration(input.OverlapSeconds) * time.Second
			pre = input.endpointPrecondition
		} else {
			if decodeJSON(w, r, &pre) != nil {
				writeError(w, 400, "invalid endpoint action")
				return
			}
		}
		a.changeEndpoint(w, r, pre, change)
	}
}

func (a *API) changeEndpoint(w http.ResponseWriter, r *http.Request, pre endpointPrecondition, change store.EndpointChange) {
	pre.Reason = strings.TrimSpace(pre.Reason)
	if pre.ExpectedVersion < 1 || len(pre.Reason) < 1 || len(pre.Reason) > 500 {
		writeError(w, 400, "expected_version and a reason of 1..500 bytes are required")
		return
	}
	change.ExpectedVersion, change.Reason, change.PrincipalID = pre.ExpectedVersion, pre.Reason, auth.FromContext(r.Context()).ID
	e, err := a.store.ChangeEndpoint(r.Context(), r.PathValue("endpointID"), change, a.clock.Now())
	if a.endpointError(w, r, err) {
		return
	}
	writeJSON(w, 200, e)
}

func (a *API) endpointError(w http.ResponseWriter, r *http.Request, err error) bool {
	switch {
	case err == nil:
		return false
	case errors.Is(err, store.ErrNotFound):
		writeError(w, 404, "endpoint not found")
	case errors.Is(err, store.ErrEndpointConflict):
		writeError(w, 409, "endpoint version or rotation precondition failed; inspect before retrying")
	default:
		a.internalError(w, r, "endpoint operation", err)
	}
	return true
}
