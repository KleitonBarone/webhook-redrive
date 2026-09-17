package httpapi

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/KleitonBarone/webhook-redrive/internal/store"
)

type ingestionStore struct {
	fakeStore
	principal, key string
	conflict       bool
}

func (s *ingestionStore) IngestEvent(ctx context.Context, e store.Event, b []byte, a, p, k string) (store.IngestionReceipt, error) {
	s.principal, s.key = p, k
	if s.conflict {
		return store.IngestionReceipt{}, store.ErrIdempotencyConflict
	}
	r, err := s.fakeStore.IngestEvent(ctx, e, b, a, p, k)
	r.Repeated = true
	return r, err
}

func TestIngestionKeyValidationAndConflict(t *testing.T) {
	for _, tc := range []struct {
		keys     []string
		conflict bool
		status   int
	}{
		{[]string{"order-1:v1"}, false, 202}, {[]string{"order-1"}, true, 409},
		{[]string{""}, false, 400}, {[]string{"a", "a"}, false, 400}, {[]string{"contains space"}, false, 400}, {[]string{strings.Repeat("x", 129)}, false, 400},
	} {
		s := &ingestionStore{conflict: tc.conflict}
		api := New(s, nil, apiClock{}, discardLogger(), nil, testSecurity(t))
		req := httptest.NewRequest("POST", "/v1/endpoints/00000000-0000-4000-8000-000000000001/events", strings.NewReader(`{}`))
		req.Header.Set("Authorization", "Bearer "+testToken)
		req.Header.Set("X-Event-Type", "order.created")
		for _, key := range tc.keys {
			req.Header.Add("Idempotency-Key", key)
		}
		res := httptest.NewRecorder()
		api.ServeHTTP(res, req)
		if res.Code != tc.status {
			t.Fatalf("status %d want %d", res.Code, tc.status)
		}
		if tc.status == 400 && s.principal != "" {
			t.Fatal("invalid key reached persistence")
		}
		if tc.status == 202 && (s.key != tc.keys[0] || s.principal != "00000000-0000-4000-8000-000000000001" || res.Header().Get("Idempotency-Replayed") != "true") {
			t.Fatal("key scope or duplicate receipt missing")
		}
	}
}
