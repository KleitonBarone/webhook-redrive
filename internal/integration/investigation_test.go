package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/KleitonBarone/webhook-redrive/internal/delivery"
	"github.com/KleitonBarone/webhook-redrive/internal/httpapi"
	"github.com/KleitonBarone/webhook-redrive/internal/id"
	"github.com/KleitonBarone/webhook-redrive/internal/logsafe"
	"github.com/KleitonBarone/webhook-redrive/internal/secret"
	"github.com/KleitonBarone/webhook-redrive/internal/store"
	"github.com/KleitonBarone/webhook-redrive/internal/testdb"
	"github.com/KleitonBarone/webhook-redrive/signature"
)

// TestInvestigationRecoveryDemo demonstrates a frozen outage selection, a lost
// recovery response, and a new API/store instance resuming durable progress.
func TestInvestigationRecoveryDemo(t *testing.T) {
	ctx := context.Background()
	databaseURL := testdb.URL(t)
	s, err := store.Open(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	c := &demoClock{}
	c.nanos.Store(time.Date(2026, 9, 18, 12, 0, 0, 0, time.UTC).UnixNano())
	if err := s.Migrate(ctx, c.Now()); err != nil {
		t.Fatal(err)
	}
	box, _ := secret.NewBox(make([]byte, secret.KeySize))
	const key = "synthetic-investigation-secret"
	const reference = "synthetic-private-order-reference"
	const reason = "synthetic-private-outage-reason"
	payload := []byte(`{"private":"synthetic-investigation-body"}`)
	var healthy atomic.Bool
	var sends atomic.Int64
	receiver := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil || !bytes.Equal(body, payload) || signature.VerifyRequest([]byte(key), r, body, c.Now(), 5*time.Minute) != nil {
			t.Error("invalid delivery")
			w.WriteHeader(401)
			return
		}
		sends.Add(1)
		if !healthy.Load() {
			w.WriteHeader(400)
			return
		}
		w.WriteHeader(204)
	}))
	defer receiver.Close()
	var logs lockedBuffer
	logger := slog.New(logsafe.New(slog.NewJSONHandler(&logs, nil)))
	security := testSecurityFor(t, s, c.Now(), receiver.URL, time.Hour)
	var dropResponse atomic.Bool
	newAPI := func(db *store.Store) *httptest.Server {
		apiSecurity := security
		apiSecurity.Authenticator = db
		handler := httpapi.New(db, box, c, logger, nil, apiSecurity)
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if strings.HasSuffix(r.URL.Path, "/run") && dropResponse.CompareAndSwap(true, false) {
				recorded := httptest.NewRecorder()
				handler.ServeHTTP(recorded, r)
				if recorded.Code != 200 {
					t.Errorf("recovery before disconnect: %d %s", recorded.Code, recorded.Body.String())
				}
				conn, _, err := w.(http.Hijacker).Hijack()
				if err != nil {
					t.Error(err)
					return
				}
				_ = conn.Close()
				return
			}
			handler.ServeHTTP(w, r)
		}))
	}
	api := newAPI(s)
	defer func() { api.Close() }()
	worker, err := delivery.NewWorker(s, box, c, logger, delivery.Config{Destinations: security.Destinations, WorkerID: "investigation", Lease: 10 * time.Second, RequestTimeout: time.Second, BatchSize: 2, PollPeriod: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	var endpoint store.Endpoint
	call(t, api.URL+"/v1/endpoints", "POST", map[string]any{"url": receiver.URL, "secret": key, "max_attempts": 1, "concurrency_limit": 2, "rate_limit": 2}, 201, &endpoint)
	ingest := func(ref string) store.Event {
		t.Helper()
		r, _ := http.NewRequest("POST", api.URL+"/v1/endpoints/"+endpoint.ID+"/events", bytes.NewReader(payload))
		r.Header.Set("Authorization", "Bearer "+testToken)
		r.Header.Set("X-Event-Type", "order.created")
		r.Header.Set("X-Producer-Reference", ref)
		res, err := http.DefaultClient.Do(r)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		if res.StatusCode != 202 {
			t.Fatal("ingest failed")
		}
		var event store.Event
		if json.NewDecoder(res.Body).Decode(&event) != nil {
			t.Fatal("invalid receipt")
		}
		return event
	}
	run := func(want int) {
		t.Helper()
		n, err := worker.RunOnce(ctx)
		if err != nil || n != want {
			t.Fatalf("dispatch %d want %d: %v", n, want, err)
		}
		c.Advance(time.Second)
	}
	// One old failure outside the selected incident window.
	outside := ingest("outside-window")
	run(1)
	from := c.Now()
	var selected []store.Event
	for i := 0; i < 13; i++ {
		selected = append(selected, ingest(fmt.Sprintf("%s-%d", reference, i)))
	}
	until := c.Now().Add(time.Microsecond)
	for i := 0; i < 6; i++ {
		run(2)
	}
	run(1)
	t.Log("1. Thirteen terminal failures retained signed delivery history inside the incident window.")
	query := "/v1/events?endpoint_id=" + endpoint.ID + "&state=recoverable&from=" + from.Format(time.RFC3339Nano) + "&until=" + until.Format(time.RFC3339Nano) + "&limit=5"
	var selection []store.ReplaySelection
	after := ""
	for {
		var page store.EventPage
		path := api.URL + query
		if after != "" {
			path += "&after=" + after
		}
		call(t, path, "GET", nil, 200, &page)
		for _, e := range page.Events {
			if e.FailureCode != "http_status" || e.ResponseStatus == nil || *e.ResponseStatus != 400 {
				t.Fatal("unsafe or missing failure summary")
			}
			selection = append(selection, store.ReplaySelection{EventID: e.ID, AttemptID: e.AttemptID})
		}
		after = page.NextAfter
		if after == "" {
			break
		}
	}
	if len(selection) != 13 {
		t.Fatalf("selected %d", len(selection))
	}
	var lookup store.EventPage
	call(t, api.URL+"/v1/events?producer_reference="+selected[0].ProducerReference, "GET", nil, 200, &lookup)
	if len(lookup.Events) != 1 || lookup.Events[0].ID != selected[0].ID {
		t.Fatal("reference lookup failed")
	}
	batchID, _ := id.New()
	input := store.BatchRequest{RequestID: batchID, Reason: reason, Events: selection}
	var batch store.ReplayBatch
	call(t, api.URL+"/v1/replay-batches", "POST", input, 200, &batch)
	if batch.State != "preview" {
		t.Fatal("preview executed recovery")
	}
	// Another operator already recovered one selected event. It must be skipped.
	requestID, _ := id.New()
	call(t, api.URL+"/v1/events/"+selection[0].EventID+"/replays", "POST", store.ReplayRequest{AttemptID: selection[0].AttemptID, RequestID: requestID, Reason: "single recovery"}, 202, nil)
	healthy.Store(true)
	run(1)
	base := api.URL + "/v1/endpoints/" + endpoint.ID
	call(t, base+"/pause", "POST", map[string]any{"expected_version": endpoint.Version, "reason": "incident recovery"}, 200, &endpoint)
	t.Log("2. Search paginated the selected window; preview froze thirteen event/attempt pairs without dispatching.")
	dropResponse.Store(true)
	r, _ := http.NewRequest("POST", api.URL+"/v1/replay-batches/"+batchID+"/run", strings.NewReader(`{}`))
	r.Header.Set("Authorization", "Bearer "+testToken)
	response, err := http.DefaultClient.Do(r)
	if response != nil {
		response.Body.Close()
	}
	if err == nil {
		t.Fatal("expected lost recovery acknowledgement")
	}
	api.Close()
	fresh, err := store.Open(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer fresh.Close()
	api = newAPI(fresh)
	call(t, api.URL+"/v1/replay-batches/"+batchID, "GET", nil, 200, &batch)
	pending := 0
	for _, item := range batch.Items {
		if item.Result == "pending" {
			pending++
		}
	}
	if pending != 3 || batch.State != "running" {
		t.Fatalf("lost response progress: %d %s", pending, batch.State)
	}
	call(t, api.URL+"/v1/replay-batches/"+batchID+"/run", "POST", struct{}{}, 200, &batch)
	call(t, api.URL+"/v1/replay-batches/"+batchID+"/run", "POST", struct{}{}, 200, &batch)
	call(t, api.URL+"/v1/replay-batches", "POST", input, 200, &batch)
	replays, skipped := 0, 0
	for _, item := range batch.Items {
		switch item.Result {
		case "replayed":
			replays++
		case "skipped":
			skipped++
		}
	}
	if batch.State != "completed" || replays != 12 || skipped != 1 {
		t.Fatalf("results replayed=%d skipped=%d", replays, skipped)
	}
	run(0)
	call(t, api.URL+"/v1/endpoints/"+endpoint.ID+"/resume", "POST", map[string]any{"expected_version": endpoint.Version, "reason": "receiver repaired"}, 200, &endpoint)
	for i := 0; i < 6; i++ {
		run(2)
	}
	run(0)
	call(t, api.URL+"/v1/replay-batches/"+batchID+"/run", "POST", struct{}{}, 200, &batch)
	for _, item := range selection {
		history, err := s.ListAttempts(ctx, item.EventID)
		if err != nil || len(history) != 2 || history[1].State != "succeeded" {
			t.Fatal("duplicate or lost recovery")
		}
	}
	old, err := s.GetEvent(ctx, outside.ID)
	if err != nil || old.State != "failed" {
		t.Fatal("out-of-window event replayed")
	}
	if sends.Load() != 27 {
		t.Fatalf("wire deliveries %d", sends.Load())
	}
	t.Log("3. A lost response and fresh API pool resumed per-event results; repeated recovery skipped the success and preserved endpoint limits.")
	if err := s.RevokeCredential(ctx, "00000000-0000-4000-8000-000000000002", c.Now()); err != nil {
		t.Fatal(err)
	}
	call(t, api.URL+"/v1/replay-batches/"+batchID+"/run", "POST", struct{}{}, 401, nil)
	for _, private := range []string{key, reference, reason, testToken, "synthetic-investigation-body", receiver.URL} {
		if strings.Contains(logs.String(), private) {
			t.Fatal("investigation logs leaked private values")
		}
	}
	t.Log("4. Credential revocation denied further recovery; logs excluded payloads, references, reasons, destinations, and secrets.")
}
