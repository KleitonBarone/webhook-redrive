package httpapi

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/KleitonBarone/webhook-redrive/internal/auth"
	"github.com/KleitonBarone/webhook-redrive/internal/destination"
	"github.com/KleitonBarone/webhook-redrive/internal/secret"
	"github.com/KleitonBarone/webhook-redrive/internal/store"
)

type apiClock struct{ now time.Time }

func (clock apiClock) Now() time.Time { return clock.now }

type fakeStore struct {
	readinessError   error
	batchInput       store.BatchRequest
	searchFilter     store.EventFilter
	endpointChange   store.EndpointChange
	replayInput      store.ReplayRequest
	endpoint         store.Endpoint
	secretCiphertext []byte
	event            store.Event
	payload          []byte
	attemptID        string
}

func (s *fakeStore) Ready(context.Context) error { return s.readinessError }

func (s *fakeStore) SearchEvents(_ context.Context, f store.EventFilter, _ string, _ int) (store.EventPage, error) {
	s.searchFilter = f
	return store.EventPage{Events: []store.Investigation{}}, nil
}
func (s *fakeStore) PreviewBatch(_ context.Context, input store.BatchRequest, _ time.Time) (store.ReplayBatch, error) {
	s.batchInput = input
	return store.ReplayBatch{}, nil
}
func (s *fakeStore) GetBatch(context.Context, string) (store.ReplayBatch, error) {
	return store.ReplayBatch{}, nil
}
func (s *fakeStore) RunBatch(context.Context, string, string, time.Time) (store.ReplayBatch, error) {
	return store.ReplayBatch{}, nil
}

func (s *fakeStore) GetEndpoint(context.Context, string) (store.Endpoint, error) {
	return s.endpoint, nil
}
func (s *fakeStore) ListEndpoints(context.Context, string) ([]store.Endpoint, error) {
	return []store.Endpoint{s.endpoint}, nil
}
func (s *fakeStore) ListEndpointAudit(context.Context, string, int64) ([]store.EndpointAudit, error) {
	return []store.EndpointAudit{}, nil
}
func (s *fakeStore) ChangeEndpoint(_ context.Context, _ string, c store.EndpointChange, _ time.Time) (store.Endpoint, error) {
	s.endpointChange = c
	return c.Settings, nil
}

func (s *fakeStore) Metrics(context.Context, time.Time) (store.MetricsSnapshot, error) {
	return store.MetricsSnapshot{}, nil
}

func (s *fakeStore) Replay(_ context.Context, eventID string, input store.ReplayRequest, _ time.Time) (store.ReplayResult, error) {
	s.replayInput = input
	return store.ReplayResult{EventID: eventID, AttemptID: input.AttemptID, RequestID: input.RequestID}, nil
}

func (s *fakeStore) ListDeadLetters(context.Context, string) ([]store.Event, error) {
	return []store.Event{}, nil
}

func (s *fakeStore) CreateEndpoint(_ context.Context, endpoint store.Endpoint, ciphertext []byte) error {
	s.endpoint = endpoint
	s.secretCiphertext = append([]byte(nil), ciphertext...)
	return nil
}

func (s *fakeStore) EndpointExists(context.Context, string) (bool, error) { return true, nil }

func (s *fakeStore) IngestEvent(_ context.Context, event store.Event, payload []byte, attemptID, principalID, key string) (store.IngestionReceipt, error) {
	s.event = event
	s.payload = append([]byte(nil), payload...)
	s.attemptID = attemptID
	return store.IngestionReceipt{Event: event, AttemptID: attemptID}, nil
}

func (s *fakeStore) GetEvent(context.Context, string) (store.Event, error) {
	return s.event, nil
}

func (s *fakeStore) ListAttempts(context.Context, string) ([]store.Attempt, error) {
	return []store.Attempt{}, nil
}

func TestEndpointRegistrationEncryptsSecretAndDoesNotReturnIt(t *testing.T) {
	t.Parallel()
	dataStore := &fakeStore{}
	box, _ := secret.NewBox(make([]byte, secret.KeySize))
	handler := New(dataStore, box, apiClock{now: time.Unix(100, 0)}, discardLogger(), nil, testSecurity(t))
	plaintext := "synthetic-secret-32-bytes-long"
	request := httptest.NewRequest(http.MethodPost, "/v1/endpoints", bytes.NewBufferString(
		`{"url":"https://example.test/hooks","secret":"`+plaintext+`"}`,
	))
	response := httptest.NewRecorder()
	request.Header.Set("Authorization", "Bearer "+testToken)
	handler.ServeHTTP(response, request)

	if response.Code != http.StatusCreated {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	if bytes.Contains(dataStore.secretCiphertext, []byte(plaintext)) {
		t.Fatal("stored ciphertext contains plaintext secret")
	}
	decrypted, err := box.Decrypt(dataStore.secretCiphertext)
	if err != nil || string(decrypted) != plaintext {
		t.Fatalf("decrypt stored secret: plaintext=%q err=%v", decrypted, err)
	}
	if bytes.Contains(response.Body.Bytes(), []byte(plaintext)) || bytes.Contains(response.Body.Bytes(), dataStore.secretCiphertext) {
		t.Fatalf("response exposed secret: %s", response.Body.String())
	}
}

func TestEventIngestionPreservesExactJSONBytes(t *testing.T) {
	t.Parallel()
	dataStore := &fakeStore{}
	box, _ := secret.NewBox(make([]byte, secret.KeySize))
	handler := New(dataStore, box, apiClock{now: time.Unix(100, 0)}, discardLogger(), nil, testSecurity(t))
	payload := []byte("{\n  \"customer\": \"demo\", \"amount\": 42\n}\n")
	request := httptest.NewRequest(http.MethodPost, "/v1/endpoints/7d178c7d-cbdd-4e47-a158-69e2f5c89770/events", bytes.NewReader(payload))
	request.Header.Set("X-Event-Type", "invoice.created")
	request.Header.Set("Authorization", "Bearer "+testToken)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)

	if response.Code != http.StatusAccepted {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	if !bytes.Equal(dataStore.payload, payload) {
		t.Fatalf("stored payload=%q want=%q", dataStore.payload, payload)
	}
	if dataStore.event.State != "pending" || dataStore.attemptID == "" {
		t.Fatalf("event=%+v attempt_id=%q", dataStore.event, dataStore.attemptID)
	}
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

const testToken = "wr_AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"

type fakeAuth struct {
	permissions []string
	err         error
}

func (f fakeAuth) Authenticate(context.Context, []byte, time.Time) (auth.Principal, error) {
	return auth.Principal{ID: "00000000-0000-4000-8000-000000000001", Name: "test-operator", Kind: "operator", Permissions: f.permissions}, f.err
}
func testSecurity(t *testing.T) Security {
	t.Helper()
	policy, err := destination.New([]destination.Rule{{Origin: "https://example.test"}, {Origin: "http://synthetic.test"}})
	if err != nil {
		t.Fatal(err)
	}
	return Security{Authenticator: fakeAuth{permissions: []string{auth.Ingest, auth.Inspect, auth.Endpoints, auth.Replay, auth.Metrics}}, Destinations: policy}
}
func authenticatedRequest(method, path string) *http.Request {
	r := httptest.NewRequest(method, path, nil)
	r.Header.Set("Authorization", "Bearer "+testToken)
	return r
}
