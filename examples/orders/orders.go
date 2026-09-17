// Package orders is a reference integration, not a service dependency or SDK.
package orders

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"time"

	"github.com/KleitonBarone/webhook-redrive/internal/id"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Schema belongs to the example application, never the delivery schema.
//
//go:embed schema.sql
var Schema string

type Created struct {
	EventID     string `json:"event_id"`
	Type        string `json:"type"`
	OrderID     string `json:"order_id"`
	AmountCents int64  `json:"amount_cents"`
}

// Create commits the business change and immutable outgoing bytes together.
// Generate the two UUIDs once before calling; persist no credentials in outbox.
func Create(ctx context.Context, pool *pgxpool.Pool, event Created, endpointID string, now time.Time) error {
	if !valid(event) || !id.Valid(endpointID) {
		return errors.New("invalid order")
	}
	body, err := json.Marshal(event)
	if err != nil {
		return err
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `INSERT INTO orders(id,amount_cents) VALUES($1,$2)`, event.OrderID, event.AmountCents); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO outbox(business_event_id,endpoint_id,payload,available_at) VALUES($1,$2,$3,$4)`, event.EventID, endpointID, body, now); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func valid(e Created) bool {
	return id.Valid(e.EventID) && id.Valid(e.OrderID) && e.Type == "order.created" && e.AmountCents > 0
}
