package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/KleitonBarone/webhook-redrive/internal/delivery"
	"github.com/KleitonBarone/webhook-redrive/internal/httpapi"
	"github.com/KleitonBarone/webhook-redrive/internal/id"
	"github.com/KleitonBarone/webhook-redrive/internal/secret"
	"github.com/KleitonBarone/webhook-redrive/internal/store"
	"github.com/KleitonBarone/webhook-redrive/internal/telemetry"
	"github.com/KleitonBarone/webhook-redrive/internal/testdb"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
)

func TestTraceSurvivesQueueRetryWorkerRestartAndReplay(t *testing.T) {
	ctx := context.Background()
	s, err := store.Open(ctx, testdb.URL(t))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	c := &clock{now: time.Date(2026, 9, 10, 0, 0, 0, 0, time.UTC)}
	if err := s.Migrate(ctx, c.Now()); err != nil {
		t.Fatal(err)
	}
	exporter := tracetest.NewInMemoryExporter()
	newTracer := func() trace.Tracer {
		p := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exporter))
		t.Cleanup(func() { _ = p.Shutdown(context.Background()) })
		return p.Tracer("test")
	}
	box, _ := secret.NewBox(make([]byte, secret.KeySize))
	var logs lockedBuffer
	logger := slog.New(slog.NewJSONHandler(&logs, nil))
	api := httptest.NewServer(httpapi.New(s, box, c, logger, newTracer()))
	defer api.Close()
	var mu sync.Mutex
	var delivered []string
	const payload = `{"private":"synthetic-payload-no-telemetry"}`
	receiver := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if string(body) != payload {
			t.Error("body changed")
		}
		if r.Header.Get("baggage") != "" || r.Header.Get("tracestate") != "" {
			t.Error("untrusted trace metadata forwarded")
		}
		mu.Lock()
		defer mu.Unlock()
		delivered = append(delivered, telemetry.TraceID(telemetry.Extract(ctx, r.Header.Get("traceparent"))))
		if len(delivered) <= 2 {
			w.WriteHeader(503)
		} else {
			w.WriteHeader(204)
		}
	}))
	defer receiver.Close()
	var endpoint store.Endpoint
	call(t, api.URL+"/v1/endpoints", "POST", map[string]any{"url": receiver.URL + "/?token=synthetic-url-token", "secret": "synthetic-credential-no-telemetry", "max_attempts": 2}, 201, &endpoint)
	parent := "00-11111111111111111111111111111111-2222222222222222-01"
	request, _ := http.NewRequest("POST", api.URL+"/v1/endpoints/"+endpoint.ID+"/events", bytes.NewBufferString(payload))
	request.Header.Set("X-Event-Type", "synthetic-event-type-no-telemetry")
	request.Header.Set("traceparent", parent)
	request.Header.Set("tracestate", "vendor=synthetic-tracestate-secret")
	request.Header.Set("baggage", "secret=synthetic-baggage-secret")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	var event store.Event
	if err := json.NewDecoder(response.Body).Decode(&event); err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != 202 {
		t.Fatalf("ingestion: %d", response.StatusCode)
	}
	newWorker := func() *delivery.Worker {
		w, err := delivery.NewWorker(s, box, c, logger, delivery.Config{Tracer: newTracer(), WorkerID: "test", Lease: 10 * time.Second, RequestTimeout: time.Second, BatchSize: 1, PollPeriod: time.Millisecond, Jitter: func() float64 { return 0 }})
		if err != nil {
			t.Fatal(err)
		}
		return w
	}
	for i := 0; i < 2; i++ {
		if n, err := newWorker().RunOnce(ctx); err != nil || n != 1 {
			t.Fatalf("delivery: %d %v", n, err)
		}
		c.now = c.now.Add(time.Second)
	}
	history, err := s.ListAttempts(ctx, event.ID)
	if err != nil || len(history) != 2 || history[1].State != "dead_letter" {
		t.Fatalf("history: %v %v", history, err)
	}
	replayID, _ := id.New()
	var replay store.ReplayResult
	call(t, api.URL+"/v1/events/"+event.ID+"/replays", "POST", store.ReplayRequest{AttemptID: history[1].ID, RequestID: replayID, Actor: "synthetic-actor-private", Reason: "synthetic-reason-private"}, 202, &replay)
	if n, err := newWorker().RunOnce(ctx); err != nil || n != 1 {
		t.Fatalf("replay: %d %v", n, err)
	}
	spans := exporter.GetSpans()
	if len(spans) != 8 {
		t.Fatalf("got %d spans", len(spans))
	}
	byID := map[trace.SpanID]tracetest.SpanStub{}
	var replaySpan tracetest.SpanStub
	for _, span := range spans {
		byID[span.SpanContext.SpanID()] = span
		if span.Name == "webhook.replay" {
			replaySpan = span
		}
	}
	for _, span := range spans {
		switch span.Name {
		case "webhook.ingest":
			if span.Parent.SpanID().String() != "2222222222222222" {
				t.Fatal("lost upstream parent")
			}
		case "webhook.queue":
			if _, ok := byID[span.Parent.SpanID()]; !ok {
				t.Fatal("queue lost durable parent")
			}
		case "webhook.deliver":
			if byID[span.Parent.SpanID()].Name != "webhook.queue" {
				t.Fatal("delivery lost queue parent")
			}
		}
	}
	if len(replaySpan.Links) != 1 || replaySpan.Links[0].SpanContext.TraceID().String() != strings.Repeat("1", 32) {
		t.Fatal("replay lost link to original trace")
	}
	mu.Lock()
	defer mu.Unlock()
	if len(delivered) != 3 || delivered[0] != strings.Repeat("1", 32) || delivered[1] != delivered[0] || delivered[2] == delivered[0] || delivered[2] == "" {
		t.Fatalf("trace IDs: %v", delivered)
	}
	encoded, err := json.Marshal(spans)
	if err != nil {
		t.Fatal(err)
	}
	for _, private := range []string{"synthetic-payload-no-telemetry", "synthetic-url-token", "synthetic-credential-no-telemetry", "synthetic-event-type-no-telemetry", "synthetic-tracestate-secret", "synthetic-baggage-secret", "synthetic-actor-private", "synthetic-reason-private"} {
		if bytes.Contains(encoded, []byte(private)) || strings.Contains(logs.String(), private) {
			t.Fatalf("telemetry leaked %s", private)
		}
	}
}
