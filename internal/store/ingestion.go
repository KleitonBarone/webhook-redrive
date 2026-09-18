package store

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"time"

	"github.com/KleitonBarone/webhook-redrive/internal/id"
)

var ErrIdempotencyConflict = errors.New("idempotency key conflicts with accepted event")

// IngestionReceipt is the original acceptance, not the event's current state.
type IngestionReceipt struct {
	Event     Event
	AttemptID string
	Repeated  bool
}

func ValidIdempotencyKey(key string) bool {
	if len(key) < 1 || len(key) > 128 {
		return false
	}
	for _, c := range key {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_' || c == '.' || c == ':') {
			return false
		}
	}
	return true
}

// IngestEvent reserves a scoped key and commits event + attempt in the same
// transaction. The unique index serializes concurrent submissions of that key.
func (s *Store) IngestEvent(ctx context.Context, event Event, payload []byte, attemptID, principalID, key string) (IngestionReceipt, error) {
	event.State = "pending"
	event.CreatedAt = event.CreatedAt.UTC().Truncate(time.Microsecond)
	receipt := IngestionReceipt{Event: event, AttemptID: attemptID}
	if key == "" {
		return receipt, s.CreateEvent(ctx, event, payload, attemptID)
	}
	if !ValidIdempotencyKey(key) || !id.Valid(principalID) {
		return IngestionReceipt{}, errors.New("invalid ingestion key scope")
	}
	keyHash := sha256.Sum256([]byte(key))
	h := sha256.New()
	// Preserve existing fingerprints when no reference is supplied. The new
	// domain prefix cannot be an old event-type length (the API caps it at 100).
	if event.ProducerReference != "" {
		h.Write([]byte("producer-reference-v1\x00"))
		var referenceSize [8]byte
		binary.BigEndian.PutUint64(referenceSize[:], uint64(len(event.ProducerReference)))
		h.Write(referenceSize[:])
		h.Write([]byte(event.ProducerReference))
	}
	// Length-prefix the normalized event type, followed by the exact body bytes.
	var size [8]byte
	binary.BigEndian.PutUint64(size[:], uint64(len(event.EventType)))
	h.Write(size[:])
	h.Write([]byte(event.EventType))
	h.Write(payload)
	requestHash := h.Sum(nil)
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return IngestionReceipt{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	tag, err := tx.Exec(ctx, `INSERT INTO ingestion_keys(principal_id,endpoint_id,key_hash,request_hash,event_id,attempt_id)
        VALUES($1,$2,$3,$4,$5,$6) ON CONFLICT(principal_id,endpoint_id,key_hash) DO NOTHING`, principalID, event.EndpointID, keyHash[:], requestHash, event.ID, attemptID)
	if err != nil {
		return IngestionReceipt{}, err
	}
	if tag.RowsAffected() == 0 {
		var originalHash []byte
		err := tx.QueryRow(ctx, `SELECT k.request_hash,e.id,e.endpoint_id,e.event_type,e.created_at,k.attempt_id,e.producer_reference
            FROM ingestion_keys k JOIN events e ON e.id=k.event_id
            WHERE k.principal_id=$1 AND k.endpoint_id=$2 AND k.key_hash=$3`, principalID, event.EndpointID, keyHash[:]).Scan(
			&originalHash, &receipt.Event.ID, &receipt.Event.EndpointID, &receipt.Event.EventType, &receipt.Event.CreatedAt, &receipt.AttemptID, &receipt.Event.ProducerReference)
		if err != nil {
			return IngestionReceipt{}, err
		}
		if !bytes.Equal(originalHash, requestHash) {
			return IngestionReceipt{}, ErrIdempotencyConflict
		}
		receipt.Event.CreatedAt = receipt.Event.CreatedAt.UTC()
		receipt.Repeated = true
		return receipt, nil
	}
	if err := insertEvent(ctx, tx, event, payload, attemptID); err != nil {
		return IngestionReceipt{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return IngestionReceipt{}, err
	}
	return receipt, nil
}
