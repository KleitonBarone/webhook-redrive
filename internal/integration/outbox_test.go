package integration

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/KleitonBarone/webhook-redrive/examples/orders"
	"github.com/KleitonBarone/webhook-redrive/internal/delivery"
	"github.com/KleitonBarone/webhook-redrive/internal/httpapi"
	"github.com/KleitonBarone/webhook-redrive/internal/id"
	"github.com/KleitonBarone/webhook-redrive/internal/logsafe"
	"github.com/KleitonBarone/webhook-redrive/internal/secret"
	"github.com/KleitonBarone/webhook-redrive/internal/store"
	"github.com/KleitonBarone/webhook-redrive/internal/testdb"
	"github.com/jackc/pgx/v5/pgxpool"
)

type demoClock struct{ nanos atomic.Int64 }

func (c *demoClock) Now() time.Time          { return time.Unix(0, c.nanos.Load()).UTC() }
func (c *demoClock) Advance(d time.Duration) { c.nanos.Add(int64(d)) }

// This test is also the finite, local integration demo. Faults happen at named
// commit boundaries, and a controllable clock replaces sleeps.
func TestOutboxToReceiverDemo(t *testing.T) {
	ctx := context.Background()
	appURL := testdb.URL(t)
	app, err := pgxpool.New(ctx, appURL)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { app.Close() }()
	if _, err := app.Exec(ctx, orders.Schema); err != nil {
		t.Fatal(err)
	}
	s, err := store.Open(ctx, testdb.URL(t))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	c := &demoClock{}
	c.nanos.Store(time.Date(2026, 9, 17, 12, 0, 0, 0, time.UTC).UnixNano())
	if err := s.Migrate(ctx, c.Now()); err != nil {
		t.Fatal(err)
	}
	var logs lockedBuffer
	logger := slog.New(logsafe.New(slog.NewJSONHandler(&logs, nil)))
	box, _ := secret.NewBox(make([]byte, secret.KeySize))
	const receiverSecret = "synthetic-orders-secret"
	receiverDB, err := pgxpool.New(ctx, appURL)
	if err != nil {
		t.Fatal(err)
	}
	defer receiverDB.Close()
	receiver := httptest.NewServer(orders.Receiver(receiverDB, []byte(receiverSecret), c.Now))
	defer receiver.Close()
	security := testSecurity(t, s, c.Now(), receiver.URL)
	apiHandler := httpapi.New(s, box, c, logger, nil, security)
	var loseAcknowledgement atomic.Bool
	loseAcknowledgement.Store(true)
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/events") && loseAcknowledgement.Swap(false) {
			recorder := httptest.NewRecorder()
			apiHandler.ServeHTTP(recorder, r)
			if recorder.Code != 202 {
				t.Errorf("lost-ack submission returned %d", recorder.Code)
			}
			conn, _, err := w.(http.Hijacker).Hijack()
			if err != nil {
				t.Error(err)
				return
			}
			conn.Close()
			return
		}
		apiHandler.ServeHTTP(w, r)
	}))
	defer api.Close()
	var endpoint store.Endpoint
	call(t, api.URL+"/v1/endpoints", "POST", map[string]any{"url": receiver.URL, "secret": receiverSecret}, 201, &endpoint)
	orderID, _ := id.New()
	businessID, _ := id.New()
	if err := orders.Create(ctx, app, orders.Created{EventID: businessID, OrderID: orderID, Type: "order.created", AmountCents: 1250}, endpoint.ID, c.Now()); err != nil {
		t.Fatal(err)
	}
	// Producer stops after the business commit, before any HTTP submission.
	app.Close()
	app, err = pgxpool.New(ctx, appURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Log("1. Business transaction and outbox survived producer restart.")
	pub, err := orders.NewPublisher(app, api.URL, testToken)
	if err != nil {
		t.Fatal(err)
	}
	if sent, err := pub.PublishOne(ctx, c.Now()); !sent || !errors.Is(err, orders.ErrPublish) {
		t.Fatalf("expected lost acknowledgement: %v %v", sent, err)
	}
	pub.Close()
	// New publisher retries exact stored bytes under the same key and principal.
	c.Advance(5 * time.Second)
	pub, err = orders.NewPublisher(app, api.URL, testToken)
	if err != nil {
		t.Fatal(err)
	}
	defer pub.Close()
	if sent, err := pub.PublishOne(ctx, c.Now()); !sent || err != nil {
		t.Fatalf("retry %v %v", sent, err)
	}
	var eventID string
	if err := app.QueryRow(ctx, `SELECT accepted_event_id FROM outbox WHERE business_event_id=$1`, businessID).Scan(&eventID); err != nil {
		t.Fatal(err)
	}
	metrics, err := s.Metrics(ctx, c.Now())
	if err != nil || metrics.Events != 1 {
		t.Fatalf("accepted events: %d %v", metrics.Events, err)
	}
	history, err := s.ListAttempts(ctx, eventID)
	if err != nil || len(history) != 1 {
		t.Fatal("duplicate initial attempt")
	}
	t.Log("2. Lost API acknowledgement retried with the same key; one durable event and initial attempt.")
	crash := &lostCompletion{Store: s, lose: true}
	newWorker := func() *delivery.Worker {
		w, err := delivery.NewWorker(crash, box, c, logger, delivery.Config{Destinations: security.Destinations, WorkerID: "orders-demo", Lease: 10 * time.Second, RequestTimeout: time.Second, BatchSize: 1, PollPeriod: time.Millisecond})
		if err != nil {
			t.Fatal(err)
		}
		return w
	}
	if _, err := newWorker().RunOnce(ctx); err == nil {
		t.Fatal("expected lost completion")
	}
	c.Advance(10 * time.Second)
	if n, err := newWorker().RunOnce(ctx); err != nil || n != 1 {
		t.Fatalf("recovery %d %v", n, err)
	}
	// Receiver pool remains open across producer restart and persists receipts.
	var count, total int64
	if err := app.QueryRow(ctx, `SELECT orders,amount_cents FROM order_totals WHERE id=1`).Scan(&count, &total); err != nil || count != 1 || total != 1250 {
		t.Fatalf("duplicate action: %d %d %v", count, total, err)
	}
	history, err = s.ListAttempts(ctx, eventID)
	if err != nil || len(history) != 1 || history[0].ClaimCount != 2 || history[0].State != "succeeded" {
		t.Fatal("invalid crash-recovery history")
	}
	t.Log("3. Worker crashed after receiver commit; recovered delivery caused no duplicate business action.")
	for _, sensitive := range []string{testToken, receiverSecret, businessID, orderID} {
		if strings.Contains(logs.String(), sensitive) {
			t.Fatal("integration data leaked into logs")
		}
	}
	t.Log("4. One order, one accepted event, two wire deliveries, one business action. Credentials and business IDs absent from service logs.")
}
