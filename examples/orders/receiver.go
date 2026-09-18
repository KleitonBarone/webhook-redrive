package orders

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"time"

	"github.com/KleitonBarone/webhook-redrive/signature"
	"github.com/jackc/pgx/v5/pgxpool"
)

var ErrBusinessConflict = errors.New("business event identity conflicts with processed body")

// Receiver verifies exact bytes before reading the authenticated business ID.
// Give each integration its own secret and receipt namespace. Never trust the
// unsigned X-Webhook-ID or X-Webhook-Event headers for business decisions.
func Receiver(pool *pgxpool.Pool, secret []byte, now func() time.Time) http.Handler {
	return ReceiverWithKeys(pool, [][]byte{secret}, now)
}

// ReceiverWithKeys accepts old and new signing keys during a planned rotation.
// Rebuild the handler after retirement rather than mutating its key ring.
func ReceiverWithKeys(pool *pgxpool.Pool, keys [][]byte, now func() time.Time) http.Handler {
	captured := make([][]byte, len(keys))
	for i, key := range keys {
		captured[i] = append([]byte(nil), key...)
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
		if err != nil {
			http.Error(w, "invalid body", 400)
			return
		}
		if signature.VerifyRequestKeys(captured, r, body, now(), 5*time.Minute) != nil {
			http.Error(w, "invalid signature", 401)
			return
		}
		var event Created
		decoder := json.NewDecoder(bytes.NewReader(body))
		decoder.DisallowUnknownFields()
		if decoder.Decode(&event) != nil || !valid(event) {
			http.Error(w, "invalid order", 400)
			return
		}
		var extra any
		if decoder.Decode(&extra) != io.EOF {
			http.Error(w, "invalid order", 400)
			return
		}
		err = apply(r.Context(), pool, event, body)
		if errors.Is(err, ErrBusinessConflict) {
			http.Error(w, "business event conflict", 409)
			return
		}
		if err != nil {
			http.Error(w, "processing failed", 500)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
}

func apply(ctx context.Context, pool *pgxpool.Pool, event Created, body []byte) error {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	hash := sha256.Sum256(body)
	tag, err := tx.Exec(ctx, `INSERT INTO receipts(business_event_id,body_hash) VALUES($1,$2) ON CONFLICT DO NOTHING`, event.EventID, hash[:])
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		var original []byte
		if err := tx.QueryRow(ctx, `SELECT body_hash FROM receipts WHERE business_event_id=$1`, event.EventID).Scan(&original); err != nil {
			return err
		}
		if !bytes.Equal(original, hash[:]) {
			return ErrBusinessConflict
		}
		return nil
	}
	if _, err := tx.Exec(ctx, `INSERT INTO order_totals(id,orders,amount_cents) VALUES(1,1,$1)
        ON CONFLICT(id) DO UPDATE SET orders=order_totals.orders+1,amount_cents=order_totals.amount_cents+excluded.amount_cents`, event.AmountCents); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
