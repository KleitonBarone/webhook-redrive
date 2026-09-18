package httpapi

import (
	"bytes"
	"log/slog"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/KleitonBarone/webhook-redrive/internal/logsafe"
)

func TestInvestigationQueryValidationAndRedaction(t *testing.T) {
	for _, query := range []string{"?limit=0", "?limit=101", "?limit=x", "?state=failed&state=pending", "?from=invalid", "?until=", "?unexpected=foo", "?state=%zz"} {
		api := New(&fakeStore{}, nil, apiClock{}, discardLogger(), nil, testSecurity(t))
		r := authenticatedRequest("GET", "/v1/events"+query)
		w := httptest.NewRecorder()
		api.ServeHTTP(w, r)
		if w.Code != 400 {
			t.Fatalf("%s returned %d", query, w.Code)
		}
	}
	s := &fakeStore{}
	var logs bytes.Buffer
	api := New(s, nil, apiClock{}, slog.New(logsafe.New(slog.NewJSONHandler(&logs, nil))), nil, testSecurity(t))
	r := authenticatedRequest("GET", "/v1/events?producer_reference=synthetic-private-reference&state=recoverable&from=2026-09-18T12:00:00Z&until=2026-09-19T12:00:00Z")
	w := httptest.NewRecorder()
	api.ServeHTTP(w, r)
	if w.Code != 200 || s.searchFilter.ProducerReference != "synthetic-private-reference" || s.searchFilter.From == nil {
		t.Fatal("filters not passed")
	}
	for _, body := range []string{`{"request_id":"00000000-0000-4000-8000-000000000001","reason":"synthetic-private-reason","events":[]}`, `{"actor":"spoofed"}`} {
		r := httptest.NewRequest("POST", "/v1/replay-batches", strings.NewReader(body))
		r.Header.Set("Authorization", "Bearer "+testToken)
		w := httptest.NewRecorder()
		api.ServeHTTP(w, r)
		if strings.Contains(body, "actor") {
			if w.Code != 400 {
				t.Fatal("spoofed actor accepted")
			}
		} else if s.batchInput.PrincipalID != "00000000-0000-4000-8000-000000000001" {
			t.Fatal("missing authenticated creator")
		}
	}
	for _, secret := range []string{"synthetic-private-reference", "synthetic-private-reason", testToken} {
		if strings.Contains(logs.String(), secret) {
			t.Fatal("private query or body logged")
		}
	}
}

func TestProducerReferenceHeaderValidation(t *testing.T) {
	for _, values := range [][]string{nil, {"order-42"}, {""}, {"a", "a"}, {"a b"}, {strings.Repeat("a", 129)}} {
		s := &fakeStore{}
		api := New(s, nil, apiClock{}, discardLogger(), nil, testSecurity(t))
		r := httptest.NewRequest("POST", "/v1/endpoints/00000000-0000-4000-8000-000000000001/events", strings.NewReader(`{}`))
		r.Header.Set("Authorization", "Bearer "+testToken)
		r.Header.Set("X-Event-Type", "order.created")
		for _, v := range values {
			r.Header.Add("X-Producer-Reference", v)
		}
		w := httptest.NewRecorder()
		api.ServeHTTP(w, r)
		valid := len(values) == 0 || len(values) == 1 && values[0] == "order-42"
		if valid {
			if w.Code != 202 {
				t.Fatal("valid reference denied")
			}
			if len(values) == 1 && s.event.ProducerReference != values[0] {
				t.Fatal("reference lost")
			}
		} else if w.Code != 400 || s.event.ID != "" {
			t.Fatal("invalid reference persisted")
		}
	}
}

func TestRecoveryRequiresAnEmptyConfirmationObject(t *testing.T) {
	api := New(&fakeStore{}, nil, apiClock{}, discardLogger(), nil, testSecurity(t))
	for _, body := range []string{"", "null", "[]", `{"actor":"spoofed"}`, "{}"} {
		r := httptest.NewRequest("POST", "/v1/replay-batches/00000000-0000-4000-8000-000000000001/run", strings.NewReader(body))
		r.Header.Set("Authorization", "Bearer "+testToken)
		w := httptest.NewRecorder()
		api.ServeHTTP(w, r)
		want := 400
		if body == "{}" {
			want = 200
		}
		if w.Code != want {
			t.Fatalf("confirmation %q status %d", body, w.Code)
		}
	}
}
