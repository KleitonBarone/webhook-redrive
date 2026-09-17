package orders

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/KleitonBarone/webhook-redrive/internal/testdb"
	"github.com/KleitonBarone/webhook-redrive/signature"
	"github.com/jackc/pgx/v5/pgxpool"
)

var testTime = time.Date(2026, 9, 17, 12, 0, 0, 0, time.UTC)
var testEvent = Created{EventID: "00000000-0000-4000-8000-000000000001", OrderID: "00000000-0000-4000-8000-000000000002", Type: "order.created", AmountCents: 1250}

func exampleDB(t *testing.T) *pgxpool.Pool {
	t.Helper()
	p, err := pgxpool.New(context.Background(), testdb.URL(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(p.Close)
	if _, err := p.Exec(context.Background(), Schema); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestOrderAndOutboxRollbackTogether(t *testing.T) {
	p := exampleDB(t)
	ctx := context.Background()
	if _, err := p.Exec(ctx, `ALTER TABLE outbox ADD CONSTRAINT reject_insert CHECK(false)`); err != nil {
		t.Fatal(err)
	}
	if err := Create(ctx, p, testEvent, testEvent.OrderID, testTime); err == nil {
		t.Fatal("expected outbox failure")
	}
	var n int
	if err := p.QueryRow(ctx, `SELECT count(*) FROM orders`).Scan(&n); err != nil || n != 0 {
		t.Fatal("order committed without outbox")
	}
}

func TestReceiverVerifiesAndAtomicallyDeduplicates(t *testing.T) {
	p := exampleDB(t)
	ctx := context.Background()
	secret := []byte("synthetic-orders-secret")
	h := Receiver(p, secret, func() time.Time { return testTime })
	body, _ := json.Marshal(testEvent)
	send := func(body []byte, stamp time.Time, headerID string, valid bool) int {
		r := httptest.NewRequest("POST", "/orders", bytes.NewReader(body))
		r.Header.Set("X-Webhook-Timestamp", strconv.FormatInt(stamp.Unix(), 10))
		r.Header.Set("X-Webhook-ID", headerID)
		r.Header.Set("X-Webhook-Event", "untrusted-header")
		if valid {
			r.Header.Set("X-Webhook-Signature", signature.Sign(secret, stamp, body))
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w.Code
	}
	if send(body, testTime, "untrusted", false) != 401 || send(body, testTime.Add(-6*time.Minute), "untrusted", true) != 401 {
		t.Fatal("unverified request accepted")
	}
	if _, err := p.Exec(ctx, `ALTER TABLE order_totals ADD CONSTRAINT reject_action CHECK(false)`); err != nil {
		t.Fatal(err)
	}
	if send(body, testTime, "first", true) != 500 {
		t.Fatal("expected action failure")
	}
	var n int
	if err := p.QueryRow(ctx, `SELECT count(*) FROM receipts`).Scan(&n); err != nil || n != 0 {
		t.Fatal("receipt committed without action")
	}
	if _, err := p.Exec(ctx, `ALTER TABLE order_totals DROP CONSTRAINT reject_action`); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	results := make(chan int, 12)
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); results <- send(body, testTime, "changed-unsigned-ID", true) }()
	}
	wg.Wait()
	close(results)
	for code := range results {
		if code != 204 {
			t.Fatalf("duplicate status %d", code)
		}
	}
	changed := testEvent
	changed.AmountCents++
	other, _ := json.Marshal(changed)
	if send(other, testTime, "first", true) != 409 {
		t.Fatal("signed conflicting identity accepted")
	}
	if send(append(body, 'x'), testTime, "first", true) != 400 {
		t.Fatal("invalid signed JSON accepted")
	}
	// Recreate both connection pool and handler; deduplication is not in memory.
	fresh, err := pgxpool.New(ctx, p.Config().ConnString())
	if err != nil {
		t.Fatal(err)
	}
	defer fresh.Close()
	h = Receiver(fresh, secret, func() time.Time { return testTime })
	if send(body, testTime, "new-unsigned-ID-after-restart", true) != 204 {
		t.Fatal("receipt did not survive restart")
	}
	var count, total int64
	if err := p.QueryRow(ctx, `SELECT orders,amount_cents FROM order_totals WHERE id=1`).Scan(&count, &total); err != nil || count != 1 || total != 1250 {
		t.Fatalf("business action %d %d %v", count, total, err)
	}
}

func TestPublisherPersistsRetryDelayAndBlocksPermanentRejection(t *testing.T) {
	for _, status := range []int{202, 408, 429, 500, 409, 401, 302} {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			p := exampleDB(t)
			ctx := context.Background()
			if err := Create(ctx, p, testEvent, testEvent.OrderID, testTime); err != nil {
				t.Fatal(err)
			}
			var receiverCalls atomic.Int64
			api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				receiverCalls.Add(1)
				if r.Header.Get("Idempotency-Key") != testEvent.EventID {
					t.Error("unstable key")
				}
				w.Header().Set("Location", "http://127.0.0.1:1/never-follow")
				w.WriteHeader(status)
				_, _ = w.Write([]byte(`{"error":"synthetic-sensitive-error"}`))
			}))
			defer api.Close()
			pub, err := NewPublisher(p, api.URL, "wr_AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA")
			if err != nil {
				t.Fatal(err)
			}
			defer pub.Close()
			if pub.client.Timeout != 5*time.Second {
				t.Fatal("missing outbound bound")
			}
			if sent, err := pub.PublishOne(ctx, testTime); !sent || !errors.Is(err, ErrPublish) {
				t.Fatalf("outcome %v %v", sent, err)
			}
			if sent, err := pub.PublishOne(ctx, testTime.Add(4*time.Second)); sent || err != nil {
				t.Fatal("ignored retry delay")
			}
			var blocked *int
			var accepted *string
			if err := p.QueryRow(ctx, `SELECT blocked_status,accepted_event_id FROM outbox`).Scan(&blocked, &accepted); err != nil || accepted != nil {
				t.Fatal("unacknowledged row marked accepted")
			}
			permanent := status == 409 || status == 401 || status == 302
			if (blocked != nil) != permanent {
				t.Fatal("wrong rejection classification")
			}
			if sent, _ := pub.PublishOne(ctx, testTime.Add(5*time.Second)); sent == permanent {
				t.Fatal("wrong retry behavior")
			}
			if permanent && receiverCalls.Load() != 1 {
				t.Fatal("permanent rejection retried")
			}
		})
	}
}
