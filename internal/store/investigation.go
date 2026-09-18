package store

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/KleitonBarone/webhook-redrive/internal/id"
	"github.com/jackc/pgx/v5"
)

var ErrInvalidSearch = errors.New("invalid search filters or cursor")

type EventFilter struct {
	EndpointID        string     `json:"endpoint_id,omitempty"`
	From              *time.Time `json:"from,omitempty"`
	Until             *time.Time `json:"until,omitempty"`
	State             string     `json:"state,omitempty"`
	ProducerReference string     `json:"producer_reference,omitempty"`
}

type Investigation struct {
	Event
	AttemptID      string     `json:"attempt_id"`
	AttemptNumber  int        `json:"attempt_number"`
	NextAttemptAt  *time.Time `json:"next_attempt_at,omitempty"`
	ExpiresAt      *time.Time `json:"expires_at,omitempty"`
	Paused         bool       `json:"paused"`
	FailureCode    string     `json:"failure_code,omitempty"`
	FailureSummary string     `json:"failure_summary,omitempty"`
	ResponseStatus *int       `json:"response_status,omitempty"`
}

type EventPage struct {
	Events    []Investigation `json:"events"`
	NextAfter string          `json:"next_after,omitempty"`
}

type eventCursor struct {
	CreatedAt time.Time `json:"at"`
	ID        string    `json:"id"`
	Filter    [32]byte  `json:"filter"`
}

// SearchEvents uses immutable creation time and UUID as a keyset. State is live,
// not a snapshot across pages. No payload, key, URL, or raw error is selected.
func (s *Store) SearchEvents(ctx context.Context, f EventFilter, after string, limit int) (EventPage, error) {
	if limit < 1 || limit > 100 || len(after) > 1024 || (f.EndpointID != "" && !id.Valid(f.EndpointID)) || (f.ProducerReference != "" && !ValidIdempotencyKey(f.ProducerReference)) {
		return EventPage{}, ErrInvalidSearch
	}
	switch f.State {
	case "", "pending", "in_progress", "succeeded", "failed", "dead_letter", "recoverable":
	default:
		return EventPage{}, ErrInvalidSearch
	}
	if f.From != nil && f.Until != nil && !f.From.Before(*f.Until) {
		return EventPage{}, ErrInvalidSearch
	}
	encoded, _ := json.Marshal(f)
	fingerprint := sha256.Sum256(encoded)
	args := []any{}
	clauses := []string{"true"}
	bind := func(value any) string { args = append(args, value); return fmt.Sprintf("$%d", len(args)) }
	if f.EndpointID != "" {
		clauses = append(clauses, "e.endpoint_id="+bind(f.EndpointID))
	}
	if f.From != nil {
		clauses = append(clauses, "e.created_at >= "+bind(*f.From))
	}
	if f.Until != nil {
		clauses = append(clauses, "e.created_at < "+bind(*f.Until))
	}
	if f.ProducerReference != "" {
		clauses = append(clauses, "e.producer_reference="+bind(f.ProducerReference))
	}
	if f.State == "recoverable" {
		clauses = append(clauses, "a.state IN ('failed','dead_letter')")
	} else if f.State != "" {
		clauses = append(clauses, "a.state="+bind(f.State))
	}
	if after != "" {
		var cursor eventCursor
		raw, err := base64.RawURLEncoding.DecodeString(after)
		if err != nil || json.Unmarshal(raw, &cursor) != nil || !id.Valid(cursor.ID) || cursor.CreatedAt.IsZero() || cursor.Filter != fingerprint {
			return EventPage{}, ErrInvalidSearch
		}
		clauses = append(clauses, "(e.created_at,e.id) > ("+bind(cursor.CreatedAt)+","+bind(cursor.ID)+"::uuid)")
	}
	rows, err := s.pool.Query(ctx, `SELECT e.id,e.endpoint_id,e.event_type,e.created_at,e.producer_reference,
 a.state,a.id,a.attempt_number,CASE WHEN a.state='pending' THEN a.available_at END,a.expires_at,p.paused,
 coalesce(failure.error_code,''),failure.response_status
 FROM events e JOIN webhook_endpoints p ON p.id=e.endpoint_id
 JOIN LATERAL (SELECT * FROM delivery_attempts WHERE event_id=e.id ORDER BY attempt_number DESC LIMIT 1) a ON true
 LEFT JOIN LATERAL (SELECT error_code,response_status FROM delivery_attempts WHERE event_id=e.id AND state IN ('failed','dead_letter') ORDER BY attempt_number DESC LIMIT 1) failure ON true
 WHERE `+strings.Join(clauses, " AND ")+` ORDER BY e.created_at,e.id LIMIT `+bind(limit+1), args...)
	if err != nil {
		return EventPage{}, err
	}
	events, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (Investigation, error) {
		var e Investigation
		err := row.Scan(&e.ID, &e.EndpointID, &e.EventType, &e.CreatedAt, &e.ProducerReference, &e.State, &e.AttemptID, &e.AttemptNumber, &e.NextAttemptAt, &e.ExpiresAt, &e.Paused, &e.FailureCode, &e.ResponseStatus)
		e.FailureCode, e.FailureSummary = safeFailure(e.FailureCode)
		return e, err
	})
	if err != nil {
		return EventPage{}, err
	}
	page := EventPage{Events: events}
	if len(events) > limit {
		page.Events = events[:limit]
		last := page.Events[limit-1]
		cursor, _ := json.Marshal(eventCursor{last.CreatedAt, last.ID, fingerprint})
		page.NextAfter = base64.RawURLEncoding.EncodeToString(cursor)
	}
	return page, nil
}

func safeFailure(code string) (string, string) {
	switch code {
	case "":
		return "", ""
	case "http_status":
		return code, "Receiver returned a non-success HTTP status."
	case "timeout":
		return code, "Receiver did not respond within the request timeout."
	case "request_error":
		return code, "The outbound connection failed."
	case "destination_denied":
		return code, "Deployment policy denied the destination."
	case "event_expired":
		return code, "The delivery cycle reached its deadline."
	case "secret_decryption":
		return code, "The signing secret could not be decrypted."
	case "invalid_endpoint":
		return code, "The configured destination could not be used."
	default:
		return "delivery_error", "Delivery failed; inspect attempt history for its classification."
	}
}
