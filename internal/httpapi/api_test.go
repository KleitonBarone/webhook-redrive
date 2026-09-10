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

	"github.com/KleitonBarone/webhook-redrive/internal/secret"
	"github.com/KleitonBarone/webhook-redrive/internal/store"
)

type apiClock struct{ now time.Time }

func (clock apiClock) Now() time.Time { return clock.now }

type fakeStore struct {
	endpoint         store.Endpoint
	secretCiphertext []byte
	event            store.Event
	payload          []byte
	attemptID        string
}

func (s *fakeStore) Replay(_ context.Context, eventID string, input store.ReplayRequest, _ time.Time) (store.ReplayResult, error) {
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

func (s *fakeStore) CreateEvent(_ context.Context, event store.Event, payload []byte, attemptID string) error {
	s.event = event
	s.payload = append([]byte(nil), payload...)
	s.attemptID = attemptID
	return nil
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
	handler := New(dataStore, box, apiClock{now: time.Unix(100, 0)}, discardLogger())
	plaintext := "synthetic-secret-32-bytes-long"
	request := httptest.NewRequest(http.MethodPost, "/v1/endpoints", bytes.NewBufferString(
		`{"url":"https://example.test/hooks","secret":"`+plaintext+`"}`,
	))
	response := httptest.NewRecorder()
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
	handler := New(dataStore, box, apiClock{now: time.Unix(100, 0)}, discardLogger())
	payload := []byte("{\n  \"customer\": \"demo\", \"amount\": 42\n}\n")
	request := httptest.NewRequest(http.MethodPost, "/v1/endpoints/7d178c7d-cbdd-4e47-a158-69e2f5c89770/events", bytes.NewReader(payload))
	request.Header.Set("X-Event-Type", "invoice.created")
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
