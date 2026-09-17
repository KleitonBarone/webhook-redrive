package integration

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/KleitonBarone/webhook-redrive/internal/auth"
	"github.com/KleitonBarone/webhook-redrive/internal/httpapi"
	"github.com/KleitonBarone/webhook-redrive/internal/id"
	"github.com/KleitonBarone/webhook-redrive/internal/store"
	"github.com/KleitonBarone/webhook-redrive/internal/testdb"
)

func TestHTTPIngestionKeepsReceiptAndTraceAcrossCredentialRotation(t *testing.T) {
	ctx := context.Background()
	s, err := store.Open(ctx, testdb.URL(t))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	c := &clock{now: time.Date(2026, 9, 17, 0, 0, 0, 123456789, time.UTC)}
	if err := s.Migrate(ctx, c.now); err != nil {
		t.Fatal(err)
	}
	security := testSecurity(t, s, c.now, "https://receiver.test")
	api := httpapi.New(s, nil, c, slog.New(slog.NewTextHandler(io.Discard, nil)), nil, security)
	endpointID, _ := id.New()
	if err := s.CreateEndpoint(ctx, store.Endpoint{ID: endpointID, URL: "https://receiver.test", CreatedAt: c.now}, []byte("unused")); err != nil {
		t.Fatal(err)
	}
	submit := func(token, parent string, want int) *httptest.ResponseRecorder {
		t.Helper()
		r := httptest.NewRequest("POST", "/v1/endpoints/"+endpointID+"/events", strings.NewReader(`{ "business_id":"synthetic-1" }`))
		r.Header.Set("Authorization", "Bearer "+token)
		r.Header.Set("Idempotency-Key", "rotation-key")
		r.Header.Set("X-Event-Type", "order.created")
		r.Header.Set("traceparent", parent)
		w := httptest.NewRecorder()
		api.ServeHTTP(w, r)
		if w.Code != want {
			t.Fatalf("status %d want %d", w.Code, want)
		}
		return w
	}
	const parent = "00-11111111111111111111111111111111-2222222222222222-01"
	first := submit(testToken, parent, 202)
	var event store.Event
	if err := json.Unmarshal(first.Body.Bytes(), &event); err != nil {
		t.Fatal(err)
	}
	history, err := s.ListAttempts(ctx, event.ID)
	if err != nil {
		t.Fatal(err)
	}
	originalParent := history[0].TraceParent
	token, err := auth.Generate()
	if err != nil {
		t.Fatal(err)
	}
	hash, _ := auth.Hash(token)
	credentialID, _ := id.New()
	if err := s.IssueCredential(ctx, "00000000-0000-4000-8000-000000000001", credentialID, hash, c.now, c.now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := s.RevokeCredential(ctx, "00000000-0000-4000-8000-000000000002", c.now); err != nil {
		t.Fatal(err)
	}
	submit(testToken, parent, 401)
	c.now = c.now.Add(time.Minute)
	repeated := submit(token, "00-33333333333333333333333333333333-4444444444444444-01", 202)
	if first.Body.String() != repeated.Body.String() || repeated.Header().Get("Idempotency-Replayed") != "true" {
		t.Fatal("acceptance receipt changed")
	}
	history, err = s.ListAttempts(ctx, event.ID)
	if err != nil || len(history) != 1 || history[0].TraceParent != originalParent || originalParent == "" {
		t.Fatal("duplicate replaced attempt trace context")
	}
	m, err := s.Metrics(ctx, c.now)
	if err != nil || m.Events != 1 {
		t.Fatal("duplicate counted as accepted event")
	}
}
